import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { resolve, join } from "node:path";
import { test } from "node:test";

// Offline native-binary smoke check. Only invalid evaluations are submitted, so
// the production executable never reaches the TypeSafe endpoint.
const binary = process.env.JEV_TEST_BINARY;
const expectedVersion = process.env.JEV_EXPECTED_VERSION;
if (!binary) throw new Error("Set JEV_TEST_BINARY to a staged absolute executable path.");
if (!expectedVersion) throw new Error("Set JEV_EXPECTED_VERSION to the injected release version.");

async function start(t) {
  const root = resolve("temp");
  await mkdir(root, { recursive: true });
  const dir = await mkdtemp(join(root, "stdio-smoke-"));
  const key = join(dir, "fixture.key");
  await writeFile(key, "local-smoke-sentinel-not-a-key\n", { mode: 0o600 });
  const child = spawn(binary, ["--key-file", key], {
    env: { HOME: dir, PATH: "/usr/bin:/bin" },
    stdio: ["pipe", "pipe", "pipe"],
  });
  let buffer = "";
  let stderr = "";
  const messages = [];
  const closed = new Promise((res) => child.once("close", (code, signal) => res({ code, signal })));
  child.stdout.setEncoding("utf8");
  child.stdout.on("data", (chunk) => {
    buffer += chunk;
    for (let n; (n = buffer.indexOf("\n")) >= 0;) {
      const line = buffer.slice(0, n);
      buffer = buffer.slice(n + 1);
      messages.push(JSON.parse(line));
    }
  });
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  const failures = [];
  child.on("error", (error) => failures.push(error.code));
  child.stdin.on("error", (error) => failures.push(error.code));
  t.after(async () => {
    child.kill("SIGKILL");
    await closed;
    await rm(dir, { recursive: true, force: true });
  });
  const send = (message) => child.stdin.write(JSON.stringify(message) + "\n");
  const next = async () => {
    const deadline = Date.now() + 2000;
    while (messages.length === 0 && Date.now() < deadline && failures.length === 0) {
      await new Promise((res) => setTimeout(res, 10));
    }
    assert.deepEqual(failures, []);
    assert.ok(messages.length, "expected a protocol response before timeout");
    return messages.shift();
  };
  const finish = async () => {
    child.stdin.end();
    let timer;
    const exit = await Promise.race([
      closed,
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error("child did not exit after EOF")), 2000); }),
    ]).finally(() => clearTimeout(timer));
    assert.deepEqual(exit, { code: 0, signal: null });
    assert.equal(buffer, "", "stdout must consist of complete JSON-RPC lines");
    assert.ok(!stderr.includes("local-smoke-sentinel"), "credential sentinel must not be logged");
  };
  return { child, send, next, finish };
}

test("native binary handshakes, rejects invalid evaluation without HTTP, and exits on EOF", { timeout: 10000 }, async (t) => {
  const server = await start(t);
  server.send({ jsonrpc: "2.0", id: 1, method: "tools/list" });
  assert.equal((await server.next()).error.code, -32002);
  server.send({ jsonrpc: "2.0", id: 2, method: "initialize", params: {
    protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "offline-smoke", version: "1" },
  } });
  const initialized = await server.next();
  assert.equal(initialized.id, 2);
  assert.equal(initialized.result.serverInfo.name, "jev-mcp");
  assert.equal(initialized.result.serverInfo.version, expectedVersion);
  server.send({ jsonrpc: "2.0", method: "notifications/initialized" });
  server.send({ jsonrpc: "2.0", id: 3, method: "tools/list" });
  const listed = await server.next();
  assert.deepEqual(listed.result.tools.map((tool) => tool.name), ["evaluate"]);
  server.send({ jsonrpc: "2.0", id: 4, method: "tools/call", params: {
    name: "evaluate", arguments: { state: "offline", questions: {} },
  } });
  const invalid = await server.next();
  assert.equal(invalid.id, 4);
  assert.equal(invalid.result.isError, true);
  await server.finish();
});

test("native binary recovers from malformed and oversized frames", { timeout: 10000 }, async (t) => {
  const server = await start(t);
  server.child.stdin.write("{invalid\n");
  assert.equal((await server.next()).error.code, -32700);
  server.child.stdin.write("x".repeat(131073) + "\n");
  assert.equal((await server.next()).error.code, -32600);
  server.send({ jsonrpc: "2.0", id: 7, method: "ping" });
  assert.deepEqual(await server.next(), { jsonrpc: "2.0", id: 7, result: {} });
  await server.finish();
});
