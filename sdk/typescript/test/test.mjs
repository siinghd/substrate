import http from "node:http";
import assert from "node:assert";
import Substrate from "../src/index.mjs";

const srv = http.createServer((req, res) => {
  const url = new URL(req.url, "http://x");
  if (req.method === "POST" && url.pathname === "/v1/sandboxes") {
    assert(req.headers["idempotency-key"], "SDK must set idempotency key");
    res.writeHead(201, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ id: "sb_1", state: "creating" }));
  } else if (req.method === "GET" && url.pathname === "/v1/sandboxes/sb_1") {
    res.writeHead(200); res.end(JSON.stringify({ id: "sb_1", state: "running" }));
  } else if (req.method === "POST" && url.pathname === "/v1/sandboxes/sb_1/exec") {
    res.writeHead(201); res.end(JSON.stringify({ exec_id: "ex_1" }));
  } else if (url.pathname === "/v1/execs/ex_1/stream") {
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    res.write("event: output\ndata: hel\n");
    res.write("event: output\ndata: lo\\n\n");
    res.write("event: exit\ndata: {\"exit_code\":0}\n\n");
    res.end();
  } else if (url.pathname === "/v1/execs/ex_1") {
    res.writeHead(200); res.end(JSON.stringify({ id: "ex_1", state: "exited", exit_code: 0 }));
  } else {
    res.writeHead(404); res.end(JSON.stringify({ code: "not_found", message: "no" }));
  }
});

srv.listen(0, "127.0.0.1", async () => {
  const port = srv.address().port;
  const c = new Substrate(`http://127.0.0.1:${port}`, "sk_test");
  const sb = await c.sandboxes.create({ templateName: "demo" });
  assert.equal(sb.id, "sb_1");

  const r = await sb.exec(["echo", "hi"]);
  assert.equal(r.exitCode, 0);
  assert.equal(r.output, "hello\n");
  assert.ok(r.ok);

  try {
    await c._http.request("GET", "/v1/nope");
    throw new Error("expected throw");
  } catch (e) {
    assert.equal(e.status, 404);
  }

  console.log("ts-sdk-tests: PASS");
  process.exit(0);
});
