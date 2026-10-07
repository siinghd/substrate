// substrate — TypeScript SDK for the Substrate sandbox platform (Node 18+,
// zero dependencies). Mirrors the Python SDK surface 1:1.

const APIError = class extends Error {
  constructor(status, code, message, fix) {
    super(`[${status}] ${code}: ${message}` + (fix ? ` (fix: ${fix})` : ""));
    this.status = status;
    this.code = code;
    this.fix = fix || "";
  }
};

class HTTP {
  constructor(baseUrl, apiKey, timeoutMs = 60000) {
    this.baseUrl = baseUrl.replace(/\/$/, "");
    this.apiKey = apiKey;
    this.timeoutMs = timeoutMs;
  }

  async request(method, path, { body, rawBody, token, idempotency } = {}) {
    const headers = {};
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const auth = token ?? this.apiKey;
    if (auth) headers["Authorization"] = `Bearer ${auth}`;
    if (idempotency) headers["Idempotency-Key"] = `ts-${crypto.randomUUID()}`;
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), this.timeoutMs);
    let resp;
    try {
      resp = await fetch(this.baseUrl + path, {
        method,
        headers,
        body: rawBody ?? (body !== undefined ? JSON.stringify(body) : undefined),
        signal: ctrl.signal,
      });
    } finally {
      clearTimeout(timer);
    }
    if (!resp.ok) {
      let payload = await resp.text();
      let err = { code: "", message: payload, fix: "" };
      try { err = JSON.parse(payload); } catch {}
      throw new APIError(resp.status, err.code, err.message, err.fix);
    }
    const text = await resp.text();
    return text ? JSON.parse(text) : {};
  }
}

export class ExecResult {
  constructor(execId, exitCode, output) {
    this.execId = execId;
    this.exitCode = exitCode;
    this.output = output;
  }
  get ok() { return this.exitCode === 0; }
}

export class Sandbox {
  constructor(http, data) {
    this._http = http;
    this.id = data.id;
    this.state = data.state;
  }

  _path(suffix = "") { return `/v1/sandboxes/${encodeURIComponent(this.id)}${suffix}`; }

  // guestFSPath escapes a sandbox-absolute path while keeping "/" separators:
  // a "?" or "#" in a file name must never become a query string or fragment.
  static guestFSPath(p) {
    return p.split("/").map(encodeURIComponent).join("/");
  }

  async reload() {
    const d = await this._http.request("GET", this._path());
    this.state = d.state;
    return d;
  }

  async wait(states = ["running"], timeoutMs = 30000, pollMs = 100) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const d = await this.reload();
      if (states.includes(d.state)) return this;
      await new Promise((r) => setTimeout(r, pollMs));
    }
    throw new Error(`sandbox ${this.id} did not reach ${states}`);
  }

  async exec(argv, { cwd = "", env = {}, timeoutMs = 120000, onOutput } = {}) {
    const d = await this._http.request("POST", this._path("/exec"),
      { body: { argv, cwd, env }, idempotency: true });
    const execId = d.exec_id;
    const chunks = [];
    let offset = 0;
    let exited = false;
    const deadline = Date.now() + timeoutMs;

    while (!exited && Date.now() < deadline) {
      const resp = await fetch(
        `${this._http.baseUrl}/v1/execs/${encodeURIComponent(execId)}/stream?offset=${offset}`,
        { headers: { Authorization: `Bearer ${this._http.apiKey}` } });
      let event = "";
      for await (const line of sseLines(resp.body)) {
        if (line === "event: exit") { exited = true; break; }
        else if (line.startsWith("id: ")) offset = parseInt(line.slice(4), 10);
        else if (line.startsWith("event: ")) event = line.slice(7);
        else if (line.startsWith("data: ") && event === "output") {
          const chunk = line.slice(6).replace(/\\n/g, "\n").replace(/\\r/g, "\r");
          chunks.push(chunk);
          onOutput?.(chunk);
        }
      }
      if (!exited) await sleep(150);
    }
    if (!exited) throw new Error(`exec ${execId} timed out`);
    const st = await this._http.request("GET", `/v1/execs/${encodeURIComponent(execId)}`);
    return new ExecResult(execId, st.exit_code ?? -1, chunks.join(""));
  }

  async read(path) {
    const resp = await fetch(`${this._http.baseUrl}${this._path("/fs" + Sandbox.guestFSPath(path))}`,
      { headers: { Authorization: `Bearer ${this._http.apiKey}` } });
    if (!resp.ok) throw new APIError(resp.status, "fs_error", await resp.text());
    return Buffer.from(await resp.arrayBuffer());
  }

  async write(path, data, append = false) {
    const body = typeof data === "string" ? Buffer.from(data) : data;
    await this._http.request("PUT", this._path(`/fs${Sandbox.guestFSPath(path)}${append ? "?append=true" : ""}`),
      { rawBody: body, idempotency: true });
  }

  async list(path = "/") {
    const d = await this._http.request("GET", this._path(`/fs${path}?list=true`));
    return d.entries ?? [];
  }

  async remove(path) { await this._http.request("DELETE", this._path("/fs" + Sandbox.guestFSPath(path))); }

  async exposePort(port, isPublic = false) {
    return this._http.request("POST", this._path("/ports"), { body: { port, public: isPublic }, idempotency: true });
  }

  async pause() { return this._http.request("POST", this._path("/pause"), { body: {}, idempotency: true }); }
  async resume() { return this._http.request("POST", this._path("/resume"), { body: {}, idempotency: true }); }
  async snapshot() { return this._http.request("POST", this._path("/snapshots"), { body: {}, idempotency: true }); }

  async mintToken(scopes = ["exec", "fs", "ports"], ttlSecs = 86400) {
    const d = await this._http.request("POST", this._path("/tokens"),
      { body: { scopes, ttl_secs: ttlSecs }, idempotency: true });
    return d.token;
  }

  async destroy() { await this._http.request("DELETE", this._path(), { idempotency: true }); }
}

async function* sseLines(stream) {
  const decoder = new TextDecoder();
  let buf = "";
  for await (const chunk of stream) {
    buf += decoder.decode(chunk, { stream: true });
    let idx;
    while ((idx = buf.indexOf("\n")) >= 0) {
      yield buf.slice(0, idx).replace(/\r$/, "");
      buf = buf.slice(idx + 1);
    }
  }
  if (buf) yield buf;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export class Substrate {
  constructor(baseUrl = "http://localhost:8080", apiKey = "") {
    this._http = new HTTP(baseUrl, apiKey);
    this.sandboxes = {
      create: async (opts = {}) => {
        const body = Object.fromEntries(Object.entries({
          template_name: opts.templateName, template_id: opts.templateId,
          external_user_id: opts.externalUserId, env: opts.env,
          mem_mib: opts.memMib, vcpus: opts.vcpus,
          ttl_idle_secs: opts.ttlIdleSecs,
        }).filter(([, v]) => v !== undefined && v !== ""));
        const d = await this._http.request("POST", "/v1/sandboxes", { body, idempotency: true });
        const sb = new Sandbox(this._http, d);
        return opts.wait === false ? sb : sb.wait(undefined, opts.waitTimeoutMs);
      },
      get: async (id) => new Sandbox(this._http, await this._http.request("GET", `/v1/sandboxes/${id}`)),
      list: async ({ externalUserId, state } = {}) => {
        const q = new URLSearchParams();
        if (externalUserId) q.set("external_user_id", externalUserId);
        if (state) q.set("state", state);
        const qs = q.toString() ? `?${q}` : "";
        const d = await this._http.request("GET", "/v1/sandboxes" + qs);
        return (d.sandboxes ?? []).map((s) => new Sandbox(this._http, s));
      },
    };
    this.templates = {
      list: async () => (await this._http.request("GET", "/v1/templates")).templates ?? [],
      create: async (name, { rootfsPath = "", memMib = 512, vcpus = 1 } = {}) =>
        this._http.request("POST", "/v1/templates",
          { body: { name, rootfs_path: rootfsPath, mem_mib: memMib, vcpus }, idempotency: true }),
    };
  }

  static forSandbox(baseUrl, token) {
    const c = new Substrate(baseUrl);
    c._http.apiKey = token;
    return c;
  }

  async events(sandboxId = "", limit = 100) {
    const qs = `?limit=${limit}` + (sandboxId ? `&sandbox_id=${sandboxId}` : "");
    return (await this._http.request("GET", "/v1/events" + qs)).events ?? [];
  }

  async usage(groupBy = "external_user_id") {
    return (await this._http.request("GET", `/v1/usage?group_by=${groupBy}`)).rows ?? [];
  }
}

export default Substrate;
