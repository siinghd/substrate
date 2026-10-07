"""substrate — Python SDK for the Substrate sandbox platform.

Covers the full v1 surface: sandboxes, exec (with streaming), filesystem,
ports/preview URLs, sandbox-scoped tokens, events and usage.

    from substrate import Substrate

    sb = Substrate("http://localhost:8080", "sk_...").sandboxes.create(
        template_name="demo", external_user_id="user-42")
    r = sb.exec(["bash", "-lc", "make test"])
    print(r.output)
"""

from __future__ import annotations

import json as _json
import time
import uuid
from typing import Any, Callable, Iterator, Optional

import urllib.error
import urllib.parse
import urllib.request


class APIError(Exception):
    def __init__(self, status: int, code: str, message: str, fix: str = ""):
        super().__init__(f"[{status}] {code}: {message}" + (f" (fix: {fix})" if fix else ""))
        self.status = status
        self.code = code
        self.message = message
        self.fix = fix


class SandboxError(Exception):
    pass


class _HTTP:
    def __init__(self, base_url: str, api_key: str, timeout: float = 60.0):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def request(
        self,
        method: str,
        path: str,
        body: Optional[dict] = None,
        raw_body: Optional[bytes] = None,
        token: Optional[str] = None,
        idempotency_key: bool = False,
        headers: Optional[dict] = None,
        stream: bool = False,
    ):
        url = self.base_url + path
        data = None
        hdrs = dict(headers or {})
        if body is not None:
            data = _json.dumps(body).encode()
            hdrs["Content-Type"] = "application/json"
        if raw_body is not None:
            data = raw_body
        auth = token or self.api_key
        if auth:
            hdrs["Authorization"] = f"Bearer {auth}"
        if idempotency_key:
            hdrs.setdefault("Idempotency-Key", f"py-{uuid.uuid4()}")
        req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
        try:
            resp = urllib.request.urlopen(req, timeout=self.timeout)
        except urllib.error.HTTPError as e:
            payload = e.read().decode()
            try:
                err = _json.loads(payload)
                raise APIError(e.code, err.get("code", ""), err.get("message", ""), err.get("fix", "")) from None
            except _json.JSONDecodeError:
                raise APIError(e.code, "", payload) from None
        if stream:
            return resp
        out = resp.read()
        if not out:
            return {}
        return _json.loads(out)


class ExecResult:
    def __init__(self, exec_id: str, exit_code: int, output: str):
        self.exec_id = exec_id
        self.exit_code = exit_code
        self.output = output

    @property
    def ok(self) -> bool:
        return self.exit_code == 0


class Sandbox:
    def __init__(self, http: _HTTP, data: dict):
        self._http = http
        self.id = data["id"]
        self.state = data.get("state")
        self.template_id = data.get("template_id")

    def _path(self, suffix: str = "") -> str:
        # Sandbox ids are percent-encoded: they become URL path segments, so a
        # hostile id must never inject "?" fragments or extra path segments.
        return f"/v1/sandboxes/{urllib.parse.quote(self.id, safe='')}{suffix}"

    @staticmethod
    def _fs_path(path: str) -> str:
        """Escape a sandbox-absolute path while keeping '/' separators."""
        return "/".join(urllib.parse.quote(seg, safe="") for seg in path.split("/"))

    def reload(self) -> dict:
        d = self._http.request("GET", self._path())
        self.state = d.get("state")
        return d

    def wait(self, states=("running",), timeout: float = 30.0, poll: float = 0.1) -> "Sandbox":
        deadline = time.time() + timeout
        while time.time() < deadline:
            d = self.reload()
            if d.get("state") in states:
                return self
            time.sleep(poll)
        raise SandboxError(f"sandbox {self.id} did not reach {states} within {timeout}s")

    def exec(self, argv: list, cwd: str = "", env: Optional[dict] = None,
             timeout: float = 120.0, on_output: Optional[Callable[[str], None]] = None) -> ExecResult:
        d = self._http.request("POST", self._path("/exec"),
                               {"argv": argv, "cwd": cwd, "env": env or {}},
                               idempotency_key=True)
        exec_id = d["exec_id"]
        output: list[str] = []
        offset = 0
        deadline = time.time() + timeout
        exited = False
        exit_code = -1
        while not exited and time.time() < deadline:
            resp = self._http.request("GET", f"/v1/execs/{urllib.parse.quote(exec_id, safe='')}/stream?offset={offset}", stream=True)
            event = ""
            for raw in resp:
                line = raw.decode().rstrip("\n")
                if line.startswith("id: "):
                    offset = int(line[4:])
                elif line == "event: exit":
                    exited = True
                    break
                elif line.startswith("event: "):
                    event = line[7:]
                elif line.startswith("data: ") and event == "output":
                    chunk = line[6:].replace("\\n", "\n").replace("\\r", "\r")
                    output.append(chunk)
                    if on_output:
                        on_output(chunk)
            if not exited:
                time.sleep(0.15)
        if not exited:
            raise SandboxError(f"exec {exec_id} timed out")
        st = self._http.request("GET", f"/v1/execs/{urllib.parse.quote(exec_id, safe='')}")
        exit_code = st.get("exit_code", -1)
        return ExecResult(exec_id, exit_code, "".join(output))

    def read(self, path: str, offset: int = 0, length: int = 0) -> bytes:
        q = f"?offset={offset}&length={length}" if offset or length else ""
        resp = self._http.request("GET", self._path(f"/fs{self._fs_path(path)}{q}"), stream=True)
        return resp.read()

    def write(self, path: str, data: bytes | str, append: bool = False) -> None:
        if isinstance(data, str):
            data = data.encode()
        q = "?append=true" if append else ""
        self._http.request("PUT", self._path(f"/fs{self._fs_path(path)}{q}"), raw_body=data, idempotency_key=True)

    def list(self, path: str = "/") -> list[dict]:
        d = self._http.request("GET", self._path(f"/fs{self._fs_path(path)}?list=true"))
        entries = d.get("entries", [])
        if d.get("truncated"):
            entries.append({"_truncated": True})
        return entries

    def remove(self, path: str) -> None:
        self._http.request("DELETE", self._path(f"/fs{self._fs_path(path)}"))

    def expose_port(self, port: int, public: bool = False) -> dict:
        return self._http.request("POST", self._path("/ports"), {"port": port, "public": public},
                                  idempotency_key=True)

    def close_port(self, port: int) -> None:
        self._http.request("DELETE", self._path(f"/ports/{port}"))

    def pause(self) -> dict:
        return self._http.request("POST", self._path("/pause"), {}, idempotency_key=True)

    def resume(self) -> dict:
        return self._http.request("POST", self._path("/resume"), {}, idempotency_key=True)

    def snapshot(self) -> dict:
        return self._http.request("POST", self._path("/snapshots"), {}, idempotency_key=True)

    def mint_token(self, scopes=("exec", "fs", "ports"), ttl_secs: int = 86400) -> str:
        d = self._http.request("POST", self._path("/tokens"),
                               {"scopes": list(scopes), "ttl_secs": ttl_secs}, idempotency_key=True)
        return d["token"]

    def destroy(self) -> None:
        self._http.request("DELETE", self._path(), idempotency_key=True)


class SandboxesAPI:
    def __init__(self, http: _HTTP):
        self._http = http

    def create(self, template_name: str = "", template_id: str = "",
               external_user_id: str = "", env: Optional[dict] = None,
               mem_mib: int = 0, vcpus: int = 0,
               ttl_idle_secs: int = 0, wait: bool = True, **kw) -> Sandbox:
        body = {
            "template_name": template_name, "template_id": template_id,
            "external_user_id": external_user_id, "env": env or {},
            "mem_mib": mem_mib, "vcpus": vcpus, "ttl_idle_secs": ttl_idle_secs,
        }
        body = {k: v for k, v in body.items() if v}
        d = self._http.request("POST", "/v1/sandboxes", body, idempotency_key=True)
        sb = Sandbox(self._http, d)
        return sb.wait(timeout=kw.pop("wait_timeout", 30.0)) if wait else sb

    def get(self, sandbox_id: str) -> Sandbox:
        return Sandbox(self._http, self._http.request("GET", f"/v1/sandboxes/{sandbox_id}"))

    def list(self, external_user_id: str = "", state: str = "") -> list[Sandbox]:
        q = []
        if external_user_id:
            q.append(f"external_user_id={external_user_id}")
        if state:
            q.append(f"state={state}")
        suffix = "?" + "&".join(q) if q else ""
        d = self._http.request("GET", "/v1/sandboxes" + suffix)
        return [Sandbox(self._http, s) for s in d.get("sandboxes", [])]


class TemplatesAPI:
    def __init__(self, http: _HTTP):
        self._http = http

    def create(self, name: str, rootfs_path: str = "", mem_mib: int = 512,
               vcpus: int = 1, warm_cmd: str = "") -> dict:
        return self._http.request("POST", "/v1/templates",
                                  {"name": name, "rootfs_path": rootfs_path,
                                   "mem_mib": mem_mib, "vcpus": vcpus, "warm_cmd": warm_cmd},
                                  idempotency_key=True)

    def list(self) -> list[dict]:
        return self._http.request("GET", "/v1/templates").get("templates", [])


class Substrate:
    """Entry point. `Substrate(url, key)` or `Substrate.sandbox_token(token, sandbox_id)`."""

    def __init__(self, base_url: str = "http://localhost:8080", api_key: str = "", token: str = ""):
        self._http = _HTTP(base_url, api_key)
        self._token = token
        self.sandboxes = SandboxesAPI(self._http)
        self.templates = TemplatesAPI(self._http)

    @classmethod
    def for_sandbox(cls, base_url: str, token: str) -> "Substrate":
        """A client scoped to exactly one sandbox via a sandbox-scoped JWT."""
        c = cls(base_url)
        c._http.api_key = token
        return c

    def events(self, sandbox_id: str = "", limit: int = 100) -> list[dict]:
        q = f"?limit={limit}" + (f"&sandbox_id={sandbox_id}" if sandbox_id else "")
        return self._http.request("GET", "/v1/events" + q).get("events", [])

    def usage(self, group_by: str = "external_user_id") -> list[dict]:
        return self._http.request("GET", f"/v1/usage?group_by={group_by}").get("rows", [])
