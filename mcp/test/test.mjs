import assert from "node:assert";
import http from "node:http";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { createServer } from "../src/index.mjs";

// fake substrate API
const srv = http.createServer((req, res) => {
  const auth = req.headers.authorization || "";
  if (!auth.includes("tok_")) {
    res.writeHead(401); res.end("{}"); return;
  }
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const respond = (code, obj) => {
      res.writeHead(code, { "Content-Type": "application/json" });
      res.end(JSON.stringify(obj));
    };
    if (req.method === "POST" && req.url === "/v1/sandboxes") {
      respond(201, { id: "sb_new1", state: "running" });
    } else if (req.method === "POST" && req.url === "/v1/sandboxes/sb_1/exec") {
      respond(201, { exec_id: "ex_9" });
    } else if (req.url === "/v1/execs/ex_9") {
      respond(200, { id: "ex_9", state: "exited", exit_code: 0 });
    } else if (req.url === "/v1/execs/ex_9/stream?offset=0") {
      res.writeHead(200); res.end("event: output\ndata: ran ok\\n\nevent: exit\ndata: {}\n\n");
    } else if (req.method === "PUT" && req.url.startsWith("/v1/sandboxes/sb_1/fs")) {
      respond(201, { size: 5 });
    } else if (req.method === "DELETE" && req.url === "/v1/sandboxes/sb_1") {
      respond(202, { state: "destroying" });
    } else {
      respond(404, { code: "not_found", message: `no route ${req.method} ${req.url}` });
    }
  });
});

srv.listen(0, "127.0.0.1", async () => {
  process.env.SUBSTRATE_URL = `http://127.0.0.1:${srv.address().port}`;
  const mcpSrv = await new Promise((resolve) => {
    const s = http.createServer(async (req, res) => {
      const { createServer: cs } = await import("../src/index.mjs");
      const server = cs("tok_x");
      const { StreamableHTTPServerTransport } = await import(
        "@modelcontextprotocol/sdk/server/streamableHttp.js"
      );
      const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
      await server.connect(transport);
      await transport.handleRequest(req, res);
    });
    s.listen(0, "127.0.0.1", () => resolve(s));
  });

  const client = new Client({ name: "test", version: "1.0.0" });
  const transport = new StreamableHTTPClientTransport(
    new URL(`http://127.0.0.1:${mcpSrv.address().port}/mcp`)
  );
  await client.connect(transport);

  const tools = await client.listTools();
  const names = tools.tools.map((t) => t.name);
  assert.deepEqual(names, [
    "sandbox_create", "sandbox_exec", "sandbox_exec_status", "sandbox_exec_kill",
    "sandbox_fs_read", "sandbox_fs_write", "sandbox_port_expose",
    "sandbox_pause", "sandbox_resume", "sandbox_destroy",
  ]);
  // deterministic order for prompt-cache hits
  const tools2 = await client.listTools();
  assert.deepEqual(tools2.tools.map((t) => t.name), names);

  const created = await client.callTool({ name: "sandbox_create", arguments: { template_name: "demo" } });
  assert.match(created.content[0].text, /sb_new1/);

  const ex = await client.callTool({
    name: "sandbox_exec",
    arguments: { sandbox_id: "sb_1", argv: ["echo", "hi"] },
  });
  assert.match(ex.content[0].text, /exit_code: 0/);
  assert.match(ex.content[0].text, /ran ok/);

  const destroyed = await client.callTool({
    name: "sandbox_destroy",
    arguments: { sandbox_id: "sb_1", confirm: "WRONG" },
  });
  assert.equal(destroyed.isError, true);
  assert.match(destroyed.content[0].text, /confirm must equal/);

  console.log("mcp-tests: PASS");
  process.exit(0);
});
