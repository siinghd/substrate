import io
import json
import os
import sys
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.dirname(__file__))
from substrate import Substrate, APIError  # noqa: E402


class Handler(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/v1/sandboxes/sb_1":
            self._send(200, {"id": "sb_1", "state": "running", "template_id": "tpl_1"})
        elif self.path.startswith("/v1/execs/ex_1/stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for chunk in ["event: output\ndata: hel\n",
                          "event: output\ndata: lo\\n\n",
                          "event: exit\ndata: {\"exit_code\":0}\n\n"]:
                self.wfile.write(chunk.encode())
        elif self.path == "/v1/execs/ex_1":
            self._send(200, {"id": "ex_1", "state": "exited", "exit_code": 0})
        else:
            self._send(404, {"code": "not_found", "message": "no"})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        if self.path == "/v1/sandboxes":
            assert self.headers.get("Idempotency-Key"), "SDK must set idempotency key"
            self._send(201, {"id": "sb_1", "state": "creating"})
        elif self.path == "/v1/sandboxes/sb_1/exec":
            self._send(201, {"exec_id": "ex_1", "state": "running"})
        else:
            self._send(404, {})

    def log_message(self, *a):
        pass


def main():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    port = srv.server_address[1]
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()

    c = Substrate(f"http://127.0.0.1:{port}", "sk_test")
    sb = c.sandboxes.create(template_name="demo")
    assert sb.id == "sb_1"

    r = sb.exec(["echo", "hi"])
    assert r.exit_code == 0 and r.output == "hello\n", repr(r.output)

    try:
        c._http.request("GET", "/v1/nope")
        raise SystemExit("expected APIError")
    except APIError as e:
        assert e.status == 404

    print("python-sdk-tests: PASS")


if __name__ == "__main__":
    main()
