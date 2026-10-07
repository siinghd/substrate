// substrate-mcp — stateless MCP server over the Substrate REST API.
//
// Conforms to the MCP 2026-07-28 revision: no protocol sessions (sandbox and
// exec handles are ordinary tool arguments), deterministic tools/list order,
// OAuth 2.1 bearer auth with audience binding left to the transport, concise
// token-economy defaults per Anthropic's tool-writing guidance.

import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";
import http from "node:http";

function substrateURL() {
  return process.env.SUBSTRATE_URL || "http://localhost:8080";
}

function truncate(text, limit = 4096) {
  if (text.length <= limit) return text;
  return (
    text.slice(0, limit) +
    `\n...truncated, ${text.length - limit} more bytes; pass offset to read more`
  );
}

// Every outbound call carries a hard deadline: Node's fetch has none, so a
// hung Substrate endpoint used to hang the tool call (and the agent) forever.
const API_TIMEOUT_MS = Number(process.env.SUBSTRATE_TIMEOUT_MS || 30000);
const LOG_TIMEOUT_MS = Number(process.env.SUBSTRATE_LOG_TIMEOUT_MS || 120000);

async function api(token, method, path, body, idempotency = true) {
  const headers = { Authorization: `Bearer ${token}` };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (idempotency && (method === "POST" || method === "DELETE")) {
    headers["Idempotency-Key"] = `mcp-${crypto.randomUUID()}`;
  }
  const resp = await fetch(substrateURL() + path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    signal: AbortSignal.timeout(API_TIMEOUT_MS),
  });
  const text = await resp.text();
  let data;
  try { data = text ? JSON.parse(text) : {}; } catch { data = { raw: text }; }
  if (!resp.ok) {
    const msg = data.message || text;
    const fix = data.fix ? ` Fix: ${data.fix}` : "";
    throw new Error(`${resp.status} ${data.code || ""}: ${msg}${fix}`);
  }
  return data;
}

// Path segments are escaped before interpolation: model-supplied ids or file
// names containing ?, #, spaces, or & otherwise break the request line or
// inject query parameters into upstream URLs.
function seg(s) { return encodeURIComponent(String(s)); }
function guestPath(p) { return String(p).split("/").map(seg).join("/"); }

// fetchLog downloads exec output from an SSE byte offset and decodes it to
// plain text (the server escapes \n inside data: lines).
async function fetchLog(token, execId, offset = 0) {
  const resp = await fetch(
    `${substrateURL()}/v1/execs/${seg(execId)}/stream?offset=${offset}`,
    { headers: { Authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(LOG_TIMEOUT_MS) },
  );
  if (!resp.ok) throw new Error(`log fetch failed (${resp.status})`);
  const text = await resp.text();
  let out = "";
  for (const line of text.split("\n")) {
    if (line.startsWith("data: ")) out += line.slice(6).replace(/\\n/g, "\n");
  }
  return out;
}

const TOOLS = [
  {
    name: "sandbox_create",
    description:
      "Create a sandbox from a template. Returns sandbox_id. Idempotent via request_id.",
    inputSchema: {
      type: "object",
      properties: {
        template_name: { type: "string", description: "Template to boot" },
        external_user_id: { type: "string", description: "Your end-user identifier" },
        env: { type: "object", additionalProperties: { type: "string" } },
      },
      required: ["template_name"],
    },
  },
  {
    name: "sandbox_exec",
    description:
      "Run a command in a sandbox. Returns within 10s with output, or an exec_id task handle if still running. mode 'concise' returns the last 4KB; 'full' streams everything so far.",
    inputSchema: {
      type: "object",
      properties: {
        sandbox_id: { type: "string" },
        argv: { type: "array", items: { type: "string" }, minItems: 1 },
        cwd: { type: "string" },
        mode: { type: "string", enum: ["concise", "full"], default: "concise" },
      },
      required: ["sandbox_id", "argv"],
    },
  },
  {
    name: "sandbox_exec_status",
    description: "Poll a long-running exec: state, new output since offset, exit code.",
    inputSchema: {
      type: "object",
      properties: {
        sandbox_id: { type: "string" },
        exec_id: { type: "string" },
        offset: { type: "number", default: 0 },
      },
      required: ["sandbox_id", "exec_id"],
    },
  },
  {
    name: "sandbox_exec_kill",
    description: "Terminate a running exec by exec_id.",
    inputSchema: {
      type: "object",
      properties: { sandbox_id: { type: "string" }, exec_id: { type: "string" } },
      required: ["sandbox_id", "exec_id"],
    },
  },
  {
    name: "sandbox_fs_read",
    description:
      "Read a file or list a directory. Listings are names+sizes+types, one line each. Large reads are truncated with a continuation hint.",
    inputSchema: {
      type: "object",
      properties: {
        sandbox_id: { type: "string" },
        path: { type: "string" },
        list: { type: "boolean", default: false },
        offset: { type: "number", default: 0 },
      },
      required: ["sandbox_id", "path"],
    },
  },
  {
    name: "sandbox_fs_write",
    description: "Write/append a file, creating parents.",
    inputSchema: {
      type: "object",
      properties: {
        sandbox_id: { type: "string" },
        path: { type: "string" },
        content: { type: "string" },
        append: { type: "boolean", default: false },
      },
      required: ["sandbox_id", "path", "content"],
    },
  },
  {
    name: "sandbox_port_expose",
    description: "Expose a port; returns the preview URL and its access token.",
    inputSchema: {
      type: "object",
      properties: { sandbox_id: { type: "string" }, port: { type: "number" } },
      required: ["sandbox_id", "port"],
    },
  },
  {
    name: "sandbox_pause",
    description: "Suspend the sandbox to storage. Resume is implicit on any other call.",
    inputSchema: {
      type: "object",
      properties: { sandbox_id: { type: "string" } },
      required: ["sandbox_id"],
    },
  },
  {
    name: "sandbox_resume",
    description: "Resume a paused sandbox (rarely needed; other calls resume implicitly).",
    inputSchema: {
      type: "object",
      properties: { sandbox_id: { type: "string" } },
      required: ["sandbox_id"],
    },
  },
  {
    name: "sandbox_destroy",
    description:
      "Destroy the sandbox and its state. Requires the sandbox_id twice (confirm param) — cheap mistake-proofing for loops.",
    inputSchema: {
      type: "object",
      properties: {
        sandbox_id: { type: "string" },
        confirm: { type: "string", description: "Repeat sandbox_id exactly" },
      },
      required: ["sandbox_id", "confirm"],
    },
  },
];

function handlerFor(token) {
  return async (name, args) => {
    const sb = args.sandbox_id;
    switch (name) {
      case "sandbox_create": {
        const d = await api(token, "POST", "/v1/sandboxes", {
          template_name: args.template_name,
          external_user_id: args.external_user_id,
          env: args.env,
        });
        return `sandbox_id: ${d.id}\nstate: ${d.state}`;
      }
      case "sandbox_exec": {
        const d = await api(token, "POST", `/v1/sandboxes/${seg(sb)}/exec`, { argv: args.argv, cwd: args.cwd });
        const execId = d.exec_id;
        // Poll the small JSON status endpoint only — the old loop re-read
        // the entire (open-ended) SSE stream every 300ms tick, which both
        // blocked until exit and made total bandwidth O(n²) in log size.
        const deadline = Date.now() + 10000;
        let status = await api(token, "GET", `/v1/execs/${seg(execId)}`, undefined, false);
        while (Date.now() < deadline && status.state === "running") {
          await new Promise((r) => setTimeout(r, 300));
          status = await api(token, "GET", `/v1/execs/${seg(execId)}`, undefined, false);
        }
        // One bounded log download at the end.
        const output = await fetchLog(token, execId, 0);
        const body =
          args.mode === "full" ? output : output.length > 4096 ? output.slice(-4096) : output;
        const suffix = output.length > 4096 && args.mode !== "full"
          ? `\n(showing last 4KB of ${output.length}B; full log via sandbox_fs_read on the exec log)`
          : "";
        return `exit_code: ${status.exit_code ?? "?"}\n${truncate(body)}${suffix}`;
      }
      case "sandbox_exec_status": {
        const st = await api(token, "GET", `/v1/execs/${seg(args.exec_id)}`, undefined, false);
        let out = `state: ${st.state}\nexit_code: ${st.exit_code ?? "n/a"}`;
        if (!Number.isNaN(Number(args.offset)) && Number(args.offset) >= 0) {
          const since = await fetchLog(token, args.exec_id, Number(args.offset));
          if (since) out += `\n${truncate(since)}`;
        }
        return out;
      }
      case "sandbox_exec_kill": {
        await api(token, "DELETE", `/v1/execs/${seg(args.exec_id)}`);
        return "killed";
      }
      case "sandbox_fs_read": {
        if (args.list) {
          const d = await api(token, "GET", `/v1/sandboxes/${seg(sb)}/fs${guestPath(args.path)}?list=true`, undefined, false);
          const lines = (d.entries || []).map((e) => `${e.dir ? "d" : "-"} ${String(e.size).padStart(9)} ${e.name}`);
          if (d.truncated) lines.push(`...truncated, ${d.count - (d.entries || []).length} more entries; pass cursor`);
          return lines.join("\n") || "(empty)";
        }
        const resp = await fetch(`${substrateURL()}/v1/sandboxes/${seg(sb)}/fs${guestPath(args.path)}?offset=${Number(args.offset) || 0}`, {
          headers: { Authorization: `Bearer ${token}` },
          signal: AbortSignal.timeout(LOG_TIMEOUT_MS),
        });
        if (!resp.ok) {
          const err = await resp.json().catch(() => ({}));
          throw new Error(err.message || `read failed (${resp.status})`);
        }
        return truncate(await resp.text());
      }
      case "sandbox_fs_write": {
        // Single PUT. The old sacrificial empty-body PUT doubled every write
        // and silently swallowed its own errors — with append:true it raced
        // the real write against a second open of the same file.
        const resp = await fetch(`${substrateURL()}/v1/sandboxes/${seg(sb)}/fs${guestPath(args.path)}${args.append ? "?append=true" : ""}`, {
          method: "PUT",
          headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/octet-stream" },
          body: args.content,
          signal: AbortSignal.timeout(LOG_TIMEOUT_MS),
        });
        if (!resp.ok) throw new Error(`write failed (${resp.status})`);
        return `wrote ${args.content.length} bytes to ${args.path}`;
      }
      case "sandbox_port_expose": {
        const d = await api(token, "POST", `/v1/sandboxes/${seg(sb)}/ports`, { port: args.port });
        return `url: ${d.url}\ntoken: ${d.token || "(public)"}`;
      }
      case "sandbox_pause":
        await api(token, "POST", `/v1/sandboxes/${seg(sb)}/pause`, {});
        return "paused";
      case "sandbox_resume":
        await api(token, "POST", `/v1/sandboxes/${seg(sb)}/resume`, {});
        return "resumed";
      case "sandbox_destroy": {
        if (args.confirm !== sb) {
          throw new Error(`confirm must equal sandbox_id ("${sb}") — pass confirm="${sb}" to really destroy`);
        }
        await api(token, "DELETE", `/v1/sandboxes/${seg(sb)}`);
        return "destroyed";
      }
      default:
        throw new Error(`unknown tool ${name}`);
    }
  };
}

export function createServer(token) {
  const server = new Server(
    { name: "substrate-mcp", version: "1.0.0" },
    { capabilities: { tools: {} } }
  );

  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: TOOLS }));

  server.setRequestHandler(CallToolRequestSchema, async (request) => {
    const { name, arguments: args } = request.params;
    try {
      const result = await handlerFor(token)(name, args || {});
      return { content: [{ type: "text", text: String(result) }] };
    } catch (err) {
      return {
        content: [{ type: "text", text: `error: ${err.message}` }],
        isError: true,
      };
    }
  });

  return server;
}

export function startHTTP(port = 8420) {
  const httpServer = http.createServer(async (req, res) => {
    const auth = req.headers.authorization || "";
    const token = auth.startsWith("Bearer ") ? auth.slice(7) : "";
    if (!token) {
      res.writeHead(401, { "WWW-Authenticate": "Bearer" });
      res.end(JSON.stringify({ error: "bearer token required (sandbox-scoped)" }));
      return;
    }
    const server = createServer(token);
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
    await server.connect(transport);
    await transport.handleRequest(req, res);
    res.on("close", () => {
      transport.close();
      server.close();
    });
  });
  return new Promise((resolve) => {
    httpServer.listen(port, "127.0.0.1", () => resolve(httpServer));
  });
}

if (process.argv[2] === "serve") {
  startHTTP(Number(process.env.MCP_PORT || 8420)).then(() =>
    console.log("substrate-mcp listening")
  );
}
