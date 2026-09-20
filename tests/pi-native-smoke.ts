import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { test } from "node:test";
import { ChildRegistry, JevError, runEvaluation } from "../adapters/pi/jev.ts";

const binary = process.env.JEV_TEST_BINARY;
if (!binary) throw new Error("Set JEV_TEST_BINARY to a staged native executable.");

test("Pi adapter handshakes with the real Go binary and reaps a rejected offline call", { timeout: 12000 }, async (t) => {
  const root = resolve("temp");
  await mkdir(root, { recursive: true });
  const home = await mkdtemp(join(root, "pi-native-smoke-"));
  const keyFile = join(home, "fixture.key");
  await writeFile(keyFile, "local-smoke-sentinel-not-a-key\n", { mode: 0o600 });
  const registry = new ChildRegistry();
  t.after(async () => {
    await registry.terminateAll();
    await rm(home, { recursive: true, force: true });
  });
  const phases: string[] = [];
  // Empty questions fail server-side validation before HTTP. Bypass the adapter's
  // normal input validator solely to exercise the actual binary's error boundary.
  await assert.rejects(runEvaluation({ state: "offline", questions: {} }, {}, {
    env: { JEV_MCP_BIN: binary, JEV_MCP_KEY_FILE: keyFile },
    home,
    registry,
    deadlineMs: 8000,
    onPhase: (phase) => phases.push(phase),
  }), (error: unknown) => error instanceof JevError && error.code === "server");
  assert.deepEqual(phases, ["starting", "handshake", "evaluating", "closing"]);
  assert.equal(registry.prune(), 0, "real server must have exited before the call settles");
});

test("real Go binary exposes opt-in selection and rejects invalid selection offline", { timeout: 12000 }, async (t) => {
  const root = resolve("temp");
  await mkdir(root, { recursive: true });
  const home = await mkdtemp(join(root, "pi-native-selection-"));
  const keyFile = join(home, "fixture.key");
  await writeFile(keyFile, "local-smoke-sentinel-not-a-key\n", { mode: 0o600 });
  const child = spawn(binary, ["--key-file", keyFile, "--enable-selection"], {
    cwd: home,
    env: { HOME: home, PATH: "/usr/bin:/bin", LANG: "C" },
    shell: false,
    stdio: ["pipe", "pipe", "pipe"],
  });
  t.after(async () => {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    await rm(home, { recursive: true, force: true });
  });

  let buffer = "";
  const messages: unknown[] = [];
  const waiters: Array<() => void> = [];
  child.stdout.setEncoding("utf8");
  child.stdout.on("data", (chunk: string) => {
    buffer += chunk;
    let newline = buffer.indexOf("\n");
    while (newline !== -1) {
      const line = buffer.slice(0, newline);
      buffer = buffer.slice(newline + 1);
      if (line.length > 0) messages.push(JSON.parse(line));
      waiters.shift()?.();
      newline = buffer.indexOf("\n");
    }
  });
  const next = async () => {
    if (messages.length === 0) await new Promise<void>((resolve) => waiters.push(resolve));
    return messages.shift() as { id?: number; result?: Record<string, unknown> };
  };
  const send = (message: unknown) => child.stdin.write(`${JSON.stringify(message)}\n`);

  send({ jsonrpc: "2.0", id: 1, method: "initialize", params: {
    protocolVersion: "2024-11-05", capabilities: {}, clientInfo: { name: "native-smoke", version: "1" },
  } });
  assert.equal((await next()).id, 1);
  send({ jsonrpc: "2.0", method: "notifications/initialized" });
  send({ jsonrpc: "2.0", id: 2, method: "tools/list" });
  const listed = await next();
  const tools = (listed.result?.tools as Array<{ name: string }> | undefined) ?? [];
  assert.deepEqual(tools.map((tool) => tool.name), ["evaluate", "select_evidence"]);

  send({ jsonrpc: "2.0", id: 3, method: "tools/call", params: {
    name: "select_evidence",
    arguments: { task: "offline", items: [{ id: "same", text: "a" }, { id: "same", text: "b" }] },
  } });
  const rejected = await next();
  assert.equal((rejected.result as { isError?: boolean } | undefined)?.isError, true);
  child.stdin.end();
  await new Promise<void>((resolve, reject) => {
    child.once("close", (code) => code === 0 ? resolve() : reject(new Error(`native server exited ${String(code)}`)));
    child.once("error", reject);
  });
});
