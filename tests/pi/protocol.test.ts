/**
 * Subprocess tests. Every case here spawns a real child through the adapter's own
 * spawn path, so protocol ordering, buffering caps, fail-closed response checking,
 * cancellation, and reaping are exercised end to end rather than mocked.
 *
 * Several fake servers plant SECRET_SENTINEL where a credential could realistically
 * leak: a JSON-RPC error message, an isError body, a stderr log line, an undocumented
 * result member, a rewritten score legend. No test may find it in an error, a result,
 * or the details.
 */

import assert from "node:assert/strict";
import { realpathSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { describe, it } from "node:test";
import {
	BUSY_MESSAGE,
	ChildRegistry,
	createEvaluateTool,
	type EvaluationOutcome,
	JEV_MODEL,
	JevError,
	JevRunner,
	type QuestionSpec,
	type RunOptions,
	runEvaluation,
	validateEvaluateInput,
} from "../../adapters/pi/jev.ts";
import {
	createFakeServer,
	type FakeServer,
	type FakeServerMode,
	fakeServerEnv,
	isAlive,
	type LogEntry,
	SECRET_SENTINEL,
} from "./helpers/fake-server.ts";
import { officialExampleRequest, sampleAnswers, sampleRequest } from "./helpers/fixtures.ts";

function validated(input: unknown = sampleRequest()) {
	const outcome = validateEvaluateInput(input);
	assert.equal(outcome.ok, true, outcome.ok ? "" : outcome.error);
	if (!outcome.ok) throw new Error("unreachable");
	return outcome;
}

async function run(
	server: FakeServer,
	options: {
		request?: unknown;
		deadlineMs?: number;
		signal?: AbortSignal;
		registry?: ChildRegistry;
		env?: Record<string, string>;
		onPhase?: (phase: string) => void;
	} = {},
): Promise<EvaluationOutcome> {
	const { request, specs } = validated(options.request ?? sampleRequest());
	return runEvaluation(request, specs, {
		env: fakeServerEnv(server, options.env ?? {}),
		home: server.home,
		deadlineMs: options.deadlineMs ?? 10000,
		...(options.signal ? { signal: options.signal } : {}),
		...(options.registry ? { registry: options.registry } : {}),
		...(options.onPhase ? { onPhase: options.onPhase as RunOptions["onPhase"] } : {}),
	});
}

async function expectJevError(
	promise: Promise<unknown>,
	predicate: (error: JevError) => boolean,
	description: string,
): Promise<JevError> {
	try {
		await promise;
	} catch (error) {
		assert.ok(error instanceof JevError, `expected JevError, got ${String(error)}`);
		assert.ok(predicate(error), `${description}; actual message: ${error.message}`);
		assert.ok(!error.message.includes(SECRET_SENTINEL), "an error message must never carry the sentinel");
		return error;
	}
	assert.fail(`expected a rejection: ${description}`);
}

/** Run a mode that must fail, and assert the sentinel is nowhere in the failure. */
async function expectFailure(mode: FakeServerMode, code: string, fragment: RegExp): Promise<void> {
	const server = createFakeServer(mode);
	const error = await expectJevError(
		run(server, { deadlineMs: 5000 }),
		(candidate) => candidate.code === code && fragment.test(candidate.message),
		`${mode} must fail with a ${code} error matching ${fragment}`,
	);
	assert.ok(!JSON.stringify({ message: error.message, code: error.code }).includes(SECRET_SENTINEL));
}

function eventOrder(log: LogEntry[]): string[] {
	return log.map((entry) => {
		if (entry.event === "recv") return `recv:${String(entry.method)}`;
		if (entry.event === "sent") return `sent:${entry.id === null ? String(entry.method) : `id${String(entry.id)}`}`;
		return entry.event;
	});
}

describe("successful evaluation", () => {
	it("completes the handshake before calling the tool and returns validated answers", async () => {
		const server = createFakeServer("ok-slow-init");
		const outcome = await run(server);

		assert.equal(outcome.model, JEV_MODEL);
		assert.equal(outcome.structured, true);
		assert.equal(outcome.protocolVersion, "2024-11-05");
		assert.equal(outcome.serverVersion, "0.1.0-test");
		assert.deepEqual(JSON.parse(JSON.stringify(outcome.answers)), sampleAnswers());
		assert.deepEqual(outcome.usage, { input_tokens: 312, output_tokens: 48 });

		const order = eventOrder(server.readLog());
		const initializeReceived = order.indexOf("recv:initialize");
		const initializeAnswered = order.indexOf("sent:id1");
		const initializedNotification = order.indexOf("recv:notifications/initialized");
		const callReceived = order.indexOf("recv:tools/call");

		assert.ok(initializeReceived >= 0 && initializeAnswered > initializeReceived);
		assert.ok(
			initializedNotification > initializeAnswered,
			`notifications/initialized must follow the initialize response: ${order.join(" -> ")}`,
		);
		assert.ok(
			callReceived > initializedNotification,
			`tools/call must follow notifications/initialized: ${order.join(" -> ")}`,
		);
	});

	it("sends the official example request unchanged and accepts its answers", async () => {
		const server = createFakeServer("ok");
		const example = officialExampleRequest();
		const outcome = await run(server, { request: example });

		assert.deepEqual(Object.keys(outcome.answers), ["refund_request", "department", "urgency"]);
		const callArgs = server.readLog().find((entry) => entry.event === "call-args");
		assert.equal(callArgs?.name, "evaluate");
		assert.deepEqual(callArgs?.arguments, example);
	});

	it("accepts a result carried only in structuredContent, with a note as the text", async () => {
		const server = createFakeServer("ok-structured-only-note");
		const outcome = await run(server);
		assert.equal(outcome.structured, true);
		assert.equal(outcome.model, JEV_MODEL);
	});

	it("accepts a result carried only as text", async () => {
		const server = createFakeServer("ok-text-only");
		const outcome = await run(server);
		assert.equal(outcome.structured, false);
		assert.equal(outcome.model, JEV_MODEL);
	});

	it("strips undocumented members instead of forwarding them", async () => {
		const server = createFakeServer("unknown-fields-secret");
		const outcome = await run(server);
		const serialized = JSON.stringify(outcome);
		assert.ok(!serialized.includes(SECRET_SENTINEL), "an unknown result member must not survive");
		assert.deepEqual(Object.keys(outcome.answers.refund_request as object), ["type", "noul"]);
		assert.deepEqual(Object.keys(outcome.usage).sort(), ["input_tokens", "output_tokens"]);
	});

	it("drops a server version that is not a plain version string", async () => {
		const server = createFakeServer("server-version-hostile");
		const outcome = await run(server);
		assert.equal(outcome.serverVersion, undefined);
		assert.ok(!JSON.stringify(outcome).includes(SECRET_SENTINEL));
	});

	it("gives the child only an allowlisted environment and a path-only key argument", async () => {
		const server = createFakeServer("env-dump");
		const keyFile = join(server.home, "jev-pi.key");
		writeFileSync(keyFile, "not-a-real-key\n", { mode: 0o600 });

		await run(server, { env: { JEV_MCP_KEY_FILE: keyFile } });

		const start = server.readLog().find((entry) => entry.event === "start");
		assert.ok(start);
		const childEnv = start?.env as Record<string, string>;
		// macOS adds __CF_USER_TEXT_ENCODING to every process it starts; the adapter
		// itself must contribute nothing beyond the allowlist.
		const allowed = new Set(["HOME", "PATH", "LANG", "__CF_USER_TEXT_ENCODING"]);
		const unexpected = Object.keys(childEnv).filter((key) => !allowed.has(key));
		assert.deepEqual(unexpected, [], `unexpected child environment: ${unexpected.join(", ")}`);
		assert.equal(childEnv.PATH, "/usr/bin:/bin");
		assert.equal(childEnv.HOME, server.home);
		const serialized = JSON.stringify(childEnv);
		assert.ok(!serialized.includes("sk-ambient-must-not-leak"));
		assert.ok(!serialized.includes("ts-ambient-must-not-leak"));
		assert.ok(!serialized.includes("proxy.invalid"));
		assert.deepEqual(start?.argv, ["--key-file", keyFile]);
		assert.equal(start?.cwd, realpathSync(server.home));
	});
});

describe("response checking fails closed", () => {
	it("rejects a result that reports another model", async () => {
		await expectFailure("bad-model", "protocol", /model other than jev-1\.13\.0/);
	});

	it("rejects a legend that does not match the rubric that was sent", async () => {
		await expectFailure("bad-legend", "protocol", /does not match its question/);
	});

	it("rejects a distribution that does not sum to 1", async () => {
		await expectFailure("bad-sum", "protocol", /does not match its question/);
	});

	it("rejects unusable token counts", async () => {
		await expectFailure("bad-usage", "protocol", /token usage/);
	});

	it("rejects an extra or missing answer", async () => {
		await expectFailure("extra-answer", "protocol", /exactly the questions/);
		await expectFailure("missing-answer", "protocol", /exactly the questions/);
	});

	it("rejects an answer of the wrong type or with an option that was not offered", async () => {
		await expectFailure("wrong-answer-type", "protocol", /does not match its question/);
		await expectFailure("option-not-offered", "protocol", /does not match its question/);
	});

	it("does not fall back to the text copy when structuredContent is invalid", async () => {
		await expectFailure("structured-invalid-text-valid", "protocol", /model other than jev-1\.13\.0/);
	});
});

describe("handshake checking", () => {
	it("rejects an unsupported protocol version", async () => {
		await expectFailure("bad-protocol", "protocol", /unsupported protocol version/);
	});

	it("rejects malformed tool capabilities", async () => {
		await expectFailure("bad-capabilities", "protocol", /tools capability/);
	});

	it("rejects a server that identifies itself as something else", async () => {
		await expectFailure("server-name-mismatch", "protocol", /identify itself as jev-mcp/);
	});
});

describe("transport failures", () => {
	it("fails when the server binary is missing", async () => {
		const server = createFakeServer("ok");
		await expectJevError(
			run(server, { deadlineMs: 5000, env: { JEV_MCP_BIN: join(server.home, "absent-binary") } }),
			(error) => error.code === "config" && /was not found/.test(error.message),
			"a missing binary must fail before any spawn",
		);
		assert.deepEqual(server.readLog(), [], "no child may be started");
	});

	it("fails when the configured path is not executable", async () => {
		const server = createFakeServer("ok");
		const notExecutable = join(server.home, "not-executable");
		writeFileSync(notExecutable, "#!/bin/sh\n", { mode: 0o600 });
		await expectJevError(
			run(server, { deadlineMs: 5000, env: { JEV_MCP_BIN: notExecutable } }),
			(error) => error.code === "config" && /not executable/.test(error.message),
			"a non-executable binary must be rejected",
		);
	});

	it("never spawns after cancellation, even before the handshake", async () => {
		const server = createFakeServer("ok");
		const controller = new AbortController();
		controller.abort();
		await expectJevError(
			run(server, { deadlineMs: 5000, signal: controller.signal }),
			(error) => error.code === "aborted",
			"an already-cancelled call must not start a process",
		);
		assert.deepEqual(server.readLog(), [], "no child may be started once the call is cancelled");
	});

	it("applies the deadline from the start of the call, covering the preflight", async () => {
		const server = createFakeServer("ok");
		const registry = new ChildRegistry();
		await expectJevError(
			run(server, { deadlineMs: 1, registry }),
			(error) => error.code === "timeout",
			"a one millisecond deadline must surface as a timeout wherever it lands",
		);
		assert.equal(registry.size, 0, "anything that was started must still be reaped");
		assert.equal(isAlive(server.pid()), false);
	});

	it("enforces the total deadline when the handshake never answers", async () => {
		const server = createFakeServer("hang-init");
		const registry = new ChildRegistry();
		await expectJevError(
			run(server, { deadlineMs: 300, registry }),
			(error) => error.code === "timeout",
			"a hanging handshake must hit the deadline",
		);
		assert.equal(registry.size, 0, "the child must be reaped after a timeout");
		assert.equal(isAlive(server.pid()), false);
	});

	it("treats an exit before the response as a failure", async () => {
		await expectFailure("exit-before-response", "exit", /exited before answering/);
		await expectFailure("exit-on-start", "exit", /exited before answering/);
	});

	it("rejects malformed and oversized protocol output", async () => {
		await expectFailure("malformed-line", "protocol", /not valid JSON/);
		await expectFailure("oversized-frame", "protocol", /oversized/);
		await expectFailure("oversized-stream", "protocol", /more output/);
	});

	it("rejects unexpected ids, notification floods, and malformed notifications", async () => {
		await expectFailure("wrong-id", "protocol", /never sent/);
		await expectFailure("notification-flood", "protocol", /too many notifications/);
		await expectFailure("malformed-notification", "protocol", /malformed notification/);
	});

	it("rejects a response that carries both a result and an error", async () => {
		await expectFailure("result-and-error", "protocol", /both a result and an error/);
	});

	it("does not report success when a protocol violation arrives with the result", async () => {
		await expectFailure("result-then-garbage", "protocol", /not valid JSON/);
	});

	it("rejects protocol corruption emitted only during cleanup", async () => {
		await expectFailure("garbage-on-close", "protocol", /not valid JSON/);
		await expectFailure("fragment-on-close", "protocol", /incomplete protocol/);
		await expectFailure("nonzero-on-close", "exit", /closing cleanly/);
	});

	it("reaps the child even if closing feedback throws", async () => {
		const server = createFakeServer("ok");
		await expectJevError(run(server, {
			onPhase: (phase) => { if (phase === "closing") throw new Error(SECRET_SENTINEL); },
		}), (error) => error.code === "protocol", "feedback failure must still reap the child");
		assert.equal(isAlive(server.pid()), false);
	});

	it("honors cancellation that arrives during cleanup", async () => {
		const server = createFakeServer("ok");
		const controller = new AbortController();
		await expectJevError(run(server, {
			signal: controller.signal,
			onPhase: (phase) => { if (phase === "closing") controller.abort(); },
		}), (error) => error.code === "aborted", "cleanup cancellation must not become success");
		assert.equal(isAlive(server.pid()), false);
	});

	it("abandons a child that floods stderr instead of draining it", async () => {
		await expectFailure("stderr-flood", "protocol", /more diagnostics/);
	});
});

describe("nothing from the child is echoed", () => {
	it("maps a JSON-RPC error to a fixed message", async () => {
		const server = createFakeServer("rpc-error-secret");
		const error = await expectJevError(
			run(server, { deadlineMs: 5000 }),
			(candidate) => candidate.code === "server",
			"a JSON-RPC error must map to a fixed message",
		);
		assert.equal(error.message, "the Jev MCP server reported an internal error");
	});

	it("maps an isError tool result to a fixed message and ignores its text", async () => {
		const server = createFakeServer("tool-error-secret");
		const error = await expectJevError(
			run(server, { deadlineMs: 5000 }),
			(candidate) => candidate.code === "server",
			"an isError result must map to a fixed message",
		);
		assert.equal(error.message, "the Jev MCP server reported that the evaluation did not complete");
	});

	it("accepts the exact versioned provider failure envelope without reading its text", async () => {
		const server = createFakeServer("provider-failure-rate-limited");
		const error = await expectJevError(
			run(server, { deadlineMs: 5000 }),
			(candidate) => candidate.code === "server",
			"a valid provider failure envelope must remain a server failure",
		);
		assert.equal(error.message, "the Jev MCP server reported that the evaluation did not complete");
	});

	it("rejects malformed provider failure envelopes without reading their text", async () => {
		for (const mode of ["provider-failure-malformed", "provider-failure-wrong-retry"] as const) {
			const server = createFakeServer(mode);
			await expectJevError(
				run(server, { deadlineMs: 5000 }),
				(candidate) => candidate.code === "protocol" && /invalid provider failure envelope/.test(candidate.message),
				"a provider failure envelope must have only the documented fixed values",
			);
		}
	});

	it("never surfaces a sentinel written to the child's stderr", async () => {
		const server = createFakeServer("stderr-secret");
		const error = await expectJevError(
			run(server, { deadlineMs: 5000 }),
			(candidate) => candidate.code === "server",
			"the run must fail",
		);
		assert.ok(!error.message.includes(SECRET_SENTINEL));
		assert.ok(!JSON.stringify(error.stack ?? "").includes(SECRET_SENTINEL));
	});
});

describe("cancellation and cleanup", () => {
	it("cancels an in-flight call, reaps the child, and returns promptly", async () => {
		const server = createFakeServer("hang-call");
		const registry = new ChildRegistry();
		const controller = new AbortController();
		let abortedAt = 0;

		const promise = run(server, {
			deadlineMs: 15000,
			registry,
			signal: controller.signal,
			onPhase: (phase) => {
				if (phase === "evaluating") {
					abortedAt = Date.now();
					controller.abort();
				}
			},
		});

		await expectJevError(promise, (error) => error.code === "aborted", "cancellation must abort the run");
		const elapsed = Date.now() - abortedAt;
		assert.ok(elapsed < 1500, `cleanup after cancellation took ${elapsed}ms, which is too long`);
		assert.equal(registry.size, 0);
		assert.equal(isAlive(server.pid()), false, "the cancelled child must not survive");
	});

	it("fails and kills a child that ignores SIGTERM and stdin EOF, within about a second", async () => {
		const server = createFakeServer("ignore-term");
		const registry = new ChildRegistry();
		const started = Date.now();
		await expectJevError(run(server, { registry }),
			(error) => error.code === "exit", "forced termination must not report a clean success");
		const elapsed = Date.now() - started;

		assert.equal(registry.size, 0);
		assert.equal(isAlive(server.pid()), false, "SIGKILL must follow an ignored SIGTERM");
		assert.ok(
			server.readLog().some((entry) => entry.event === "sigterm-ignored"),
			"the test child must actually have ignored SIGTERM",
		);
		assert.ok(elapsed < 3000, `escalation took ${elapsed}ms, which is longer than the bounded cleanup allows`);
	});
});

describe("ChildRegistry", () => {
	it("bounds a sweep and reports children it could not reap", async () => {
		const registry = new ChildRegistry();
		const stubborn = {
			hasClosed: false,
			terminate: async () => {
				await new Promise((resolve) => setTimeout(resolve, 5000));
				return false;
			},
		};
		registry.add(stubborn);

		const started = Date.now();
		const survivors = await registry.terminateAll(200);
		const elapsed = Date.now() - started;

		assert.equal(survivors, 1, "a child that did not close must still be counted");
		assert.equal(registry.size, 1, "an unreaped child stays tracked");
		assert.ok(elapsed < 1500, `the sweep took ${elapsed}ms and should be bounded`);
	});

	it("drops children once they report closed", async () => {
		const registry = new ChildRegistry();
		const child = { hasClosed: false, terminate: async () => true };
		registry.add(child);
		assert.equal(registry.prune(), 1);
		child.hasClosed = true;
		assert.equal(registry.prune(), 0);
	});
});

describe("JevRunner", () => {
	it("refuses a second concurrent evaluation instead of queueing it", async () => {
		const server = createFakeServer("hang-call");
		const runner = new JevRunner();
		const { request, specs } = validated();
		const options = { env: fakeServerEnv(server), home: server.home, deadlineMs: 600 };

		const first = runner.evaluate(request, specs, options);
		await expectJevError(
			runner.evaluate(request, specs, options),
			(error) => error.code === "busy" && error.message === BUSY_MESSAGE,
			"the second call must be refused",
		);
		await expectJevError(first, (error) => error.code === "timeout", "the first call still times out");

		assert.equal(runner.isBusy(), false);
		assert.equal(runner.activeChildCount(), 0);
		const starts = server.readLog().filter((entry) => entry.event === "start");
		assert.equal(starts.length, 1, "a refused call must not spawn a second child");
	});

	it("aborts in-flight work and reaps owned children on shutdown", async () => {
		const server = createFakeServer("hang-call");
		const runner = new JevRunner();
		const { request, specs } = validated();
		let reachedCall = false;

		const inFlight = runner.evaluate(request, specs, {
			env: fakeServerEnv(server),
			home: server.home,
			deadlineMs: 20000,
			onPhase: (phase) => {
				if (phase === "evaluating") reachedCall = true;
			},
		});

		while (!reachedCall) await new Promise((resolve) => setTimeout(resolve, 20));

		const [, survivors] = await Promise.all([
			expectJevError(inFlight, (error) => error.code === "shutdown", "shutdown must abort in-flight work"),
			runner.shutdown(),
		]);

		assert.equal(survivors, 0, "shutdown must reap the owned child");
		assert.equal(runner.activeChildCount(), 0);
		assert.equal(runner.isShutDown(), true);
		assert.equal(isAlive(server.pid()), false);

		await expectJevError(
			runner.evaluate(request, specs, { env: fakeServerEnv(server), home: server.home }),
			(error) => error.code === "shutdown",
			"a shut-down runner must not start new work",
		);
	});

	it("is idempotent on shutdown", async () => {
		const runner = new JevRunner();
		assert.equal(await runner.shutdown(), 0);
		assert.equal(await runner.shutdown(), 0);
	});
});

describe("evaluate tool execution", () => {
	const dummyContext = {} as never;
	type ToolParams = Parameters<ReturnType<typeof createEvaluateTool>["execute"]>[1];

	/** Point the Pi process environment at a fake server, the way a staged test would. */
	async function withServerEnv<T>(server: FakeServer, body: () => Promise<T>): Promise<T> {
		const previous = { home: process.env.HOME, bin: process.env.JEV_MCP_BIN };
		process.env.HOME = server.home;
		process.env.JEV_MCP_BIN = server.binPath;
		try {
			return await body();
		} finally {
			if (previous.home === undefined) delete process.env.HOME;
			else process.env.HOME = previous.home;
			if (previous.bin === undefined) delete process.env.JEV_MCP_BIN;
			else process.env.JEV_MCP_BIN = previous.bin;
		}
	}

	it("returns a JSON payload and validated details on success", async () => {
		const server = createFakeServer("ok");
		const runner = new JevRunner();
		const tool = createEvaluateTool(runner);
		const updates: unknown[] = [];

		const result = await withServerEnv(server, () =>
			tool.execute(
				"call-1",
				sampleRequest() as ToolParams,
				undefined,
				(partial) => updates.push(partial.details),
				dummyContext,
			),
		);

		const text = result.content[0];
		assert.ok(text && text.type === "text");
		const payload = JSON.parse(text.text) as {
			model: string;
			answers: Record<string, unknown>;
			usage: Record<string, number>;
			note: string;
		};
		assert.equal(payload.model, JEV_MODEL);
		assert.deepEqual(payload.answers, sampleAnswers());
		assert.deepEqual(payload.usage, { input_tokens: 312, output_tokens: 48 });
		assert.match(payload.note, /not a test result/i);

		const details = result.details as Record<string, unknown>;
		assert.equal(details.kind, "jev-result");
		assert.equal(details.model, JEV_MODEL);
		assert.deepEqual(details.questionIds, ["refund_request", "department", "urgency"]);

		const phases = updates.map((update) => (update as { phase: string }).phase);
		assert.ok(phases.includes("handshake") && phases.includes("evaluating"), phases.join(","));

		await runner.shutdown();
	});

	it("throws a one-line sanitized failure that mentions the duration", async () => {
		const server = createFakeServer("tool-error-secret");
		const runner = new JevRunner();
		const tool = createEvaluateTool(runner);

		await assert.rejects(
			withServerEnv(server, () =>
				tool.execute("call-2", sampleRequest() as ToolParams, undefined, undefined, dummyContext),
			),
			(error: unknown) => {
				assert.ok(error instanceof Error);
				assert.match(error.message, /^jev evaluate failed after /);
				assert.ok(!error.message.includes(SECRET_SENTINEL));
				assert.equal(error.message.includes("\n"), false, "the failure must stay on one line");
				return true;
			},
		);
		await runner.shutdown();
	});

	it("rejects invalid input before spawning anything", async () => {
		const runner = new JevRunner();
		const tool = createEvaluateTool(runner);
		await assert.rejects(
			tool.execute(
				"call-3",
				{ state: "x", questions: { q: { type: "choice", instructions: "pick", criteria: ["a", "b"] } } } as ToolParams,
				undefined,
				undefined,
				dummyContext,
			),
			/rejected the request: choice criteria .* must be an object mapping each option/,
		);
		assert.equal(runner.activeChildCount(), 0);
	});

	it("reports a busy runner as a tool error", async () => {
		const server = createFakeServer("hang-call");
		const runner = new JevRunner();
		const tool = createEvaluateTool(runner);
		const { request, specs } = validated();
		const first = runner.evaluate(request, specs as Record<string, QuestionSpec>, {
			env: fakeServerEnv(server),
			home: server.home,
			deadlineMs: 400,
		});
		await assert.rejects(
			tool.execute("call-4", sampleRequest() as ToolParams, undefined, undefined, dummyContext),
			/already running/,
		);
		await expectJevError(first, (error) => error.code === "timeout", "the first call still times out");
	});
});
