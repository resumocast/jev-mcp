/**
 * Test helper: a real fake MCP server process.
 *
 * The adapter spawns a real child with an allowlisted environment, so the fake server
 * cannot be configured through the environment. Its behaviour is baked into the
 * generated script, and it reports what it saw by appending JSON lines to
 * `$HOME/received.log` (HOME is the per-test temporary directory).
 *
 * Answers are built from the request the adapter actually sent, so the success modes
 * produce the official answer shapes for the questions that were asked, and the
 * failure modes are one deliberate deviation from them.
 */

import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { after } from "node:test";

const fixtures: FakeServer[] = [];
after(() => {
	for (const server of fixtures) {
		if (isAlive(server.pid())) throw new Error("Test child still alive; fixture retained for inspection");
		rmSync(server.home, { recursive: true, force: true });
	}
});

export type FakeServerMode =
	/* success paths */
	| "ok"
	| "ok-slow-init"
	| "ok-text-only"
	| "ok-structured-only-note"
	| "selection-bad-hash"
	| "selection-bad-status"
	| "selection-extra-field"
	| "selection-iserror-string"
	| "selection-missing-content"
	| "selection-mismatched-copies"
	/* environment reporting */
	| "env-dump"
	/* transport failures */
	| "hang-init"
	| "hang-call"
	| "exit-before-response"
	| "exit-on-start"
	| "malformed-line"
	| "oversized-frame"
	| "oversized-stream"
	| "wrong-id"
	| "notification-flood"
	| "malformed-notification"
	| "result-and-error"
	| "ignore-term"
	/* handshake failures */
	| "bad-protocol"
	| "bad-capabilities"
	| "server-name-mismatch"
	| "server-version-hostile"
	/* payload failures, each carrying the sentinel where it can */
	| "rpc-error-secret"
	| "tool-error-secret"
	| "stderr-secret"
	| "stderr-flood"
	| "unknown-fields-secret"
	| "bad-model"
	| "bad-legend"
	| "bad-sum"
	| "bad-usage"
	| "extra-answer"
	| "missing-answer"
	| "wrong-answer-type"
	| "option-not-offered"
	| "structured-invalid-text-valid"
	| "result-then-garbage"
	| "garbage-on-close"
	| "fragment-on-close"
	| "nonzero-on-close";

/** Sentinel stood in for a credential. It must never resurface in any output. */
export const SECRET_SENTINEL = "SENTINEL-SECRET-e3b0c44298fc";

export interface FakeServer {
	/** Absolute path of the executable to point `JEV_MCP_BIN` at. */
	binPath: string;
	/** Temporary HOME for the child; also where the log is written. */
	home: string;
	/** Parse `$HOME/received.log`. */
	readLog(): LogEntry[];
	/** The pid the child reported at startup, when it got that far. */
	pid(): number | undefined;
}

export interface LogEntry {
	event: string;
	[key: string]: unknown;
}

const SCRIPT_TEMPLATE = String.raw`
import { appendFileSync } from "node:fs";
import { createHash } from "node:crypto";
import { join } from "node:path";

const MODE = __MODE__;
const SECRET = __SECRET__;
const home = process.env.HOME || "/tmp";
const logPath = join(home, "received.log");

function record(entry) {
	try {
		appendFileSync(logPath, JSON.stringify(entry) + "\n");
	} catch {
		// The log is best effort.
	}
}

function send(message) {
	process.stdout.write(JSON.stringify(message) + "\n");
	record({ event: "sent", id: message.id ?? null, method: message.method ?? null });
}

record({
	event: "start",
	mode: MODE,
	pid: process.pid,
	argv: process.argv.slice(2),
	env: process.env,
	cwd: process.cwd(),
});

if (MODE === "exit-on-start") process.exit(7);

if (MODE === "ignore-term") {
	process.on("SIGTERM", () => record({ event: "sigterm-ignored" }));
	process.on("SIGINT", () => record({ event: "sigint-ignored" }));
	setInterval(() => {}, 1000);
}

if (MODE === "stderr-secret") {
	process.stderr.write("server log: using key " + SECRET + "\n");
}

if (MODE === "stderr-flood") {
	for (let i = 0; i < 40; i++) process.stderr.write("noise ".repeat(1000) + "\n");
}

function serverName() {
	if (MODE === "server-name-mismatch") return "not-jev-mcp";
	return "jev-mcp";
}

function serverVersion() {
	if (MODE === "server-version-hostile") return "1.0 \u001b[31m" + SECRET;
	return "0.1.0-test";
}

const initializeResult = {
	protocolVersion: MODE === "bad-protocol" ? "1999-01-01" : "2024-11-05",
	capabilities: MODE === "bad-capabilities" ? { tools: "yes" } : { tools: {} },
	serverInfo: { name: serverName(), version: serverVersion() },
	instructions: "fake server used by the adapter tests",
};

/** Build the official answer shape for one question. */
function answerFor(question) {
	if (question.type === "noul") {
		return { type: "noul", noul: 0.92 };
	}
	if (question.type === "choice") {
		const options = Object.keys(question.criteria ?? {});
		const probabilities = {};
		const even = 1 / options.length;
		options.forEach((option, index) => {
			probabilities[option] = index === options.length - 1 ? 1 - even * (options.length - 1) : even;
		});
		return { type: "choice", choice: options[0], probabilities, confidence: 0.82 };
	}
	const levels = question.criteria ?? [];
	const legend = {};
	const probabilities = {};
	const even = 1 / levels.length;
	levels.forEach((text, index) => {
		legend[String(index)] = text;
		probabilities[String(index)] = index === levels.length - 1 ? 1 - even * (levels.length - 1) : even;
	});
	return { type: "score", score: 1, legend, probabilities, confidence: 0.78 };
}

function buildPayload(args) {
	const questions = args?.questions ?? {};
	const answers = {};
	for (const id of Object.keys(questions)) answers[id] = answerFor(questions[id]);
	const payload = { model: "jev-1.13.0", answers, usage: { input_tokens: 312, output_tokens: 48 } };

	const ids = Object.keys(answers);
	const first = ids[0];

	switch (MODE) {
		case "bad-model":
			payload.model = "jev-9.9.9";
			break;
		case "bad-legend":
			for (const id of ids) {
				if (answers[id].type === "score") answers[id].legend["0"] = "rewritten " + SECRET;
			}
			break;
		case "bad-sum":
			for (const id of ids) {
				if (answers[id].probabilities) {
					for (const key of Object.keys(answers[id].probabilities)) answers[id].probabilities[key] = 0.9;
				}
			}
			break;
		case "bad-usage":
			payload.usage = { input_tokens: 1.5, output_tokens: -3 };
			break;
		case "extra-answer":
			answers["unasked_question"] = { type: "noul", noul: 0.5 };
			break;
		case "missing-answer":
			if (first) delete answers[first];
			break;
		case "wrong-answer-type":
			if (first) {
				answers[first] =
					answers[first].type === "noul"
						? { type: "choice", choice: "billing", probabilities: { billing: 1 }, confidence: 0.5 }
						: { type: "noul", noul: 0.5 };
			}
			break;
		case "option-not-offered":
			for (const id of ids) {
				if (answers[id].type === "choice") answers[id].choice = "not-an-option";
			}
			break;
		case "unknown-fields-secret":
			payload.debug = { note: SECRET };
			payload.usage.internal_key = SECRET;
			for (const id of ids) answers[id].explanation = "reasoning: " + SECRET;
			break;
		default:
			break;
	}
	return payload;
}

function onInitialize(message) {
	if (MODE === "hang-init") return;
	if (MODE === "malformed-line") {
		process.stdout.write("this is not json\n");
		return;
	}
	if (MODE === "oversized-frame") {
		process.stdout.write("x".repeat(600000) + "\n");
		return;
	}
	if (MODE === "oversized-stream") {
		const chunk = JSON.stringify({ jsonrpc: "2.0", method: "notifications/noise", params: { blob: "y".repeat(100000) } }) + "\n";
		for (let i = 0; i < 20; i++) process.stdout.write(chunk);
		return;
	}
	if (MODE === "wrong-id") {
		send({ jsonrpc: "2.0", id: 99, result: initializeResult });
		return;
	}
	if (MODE === "notification-flood") {
		for (let i = 0; i < 80; i++) send({ jsonrpc: "2.0", method: "notifications/progress", params: { i } });
		return;
	}
	if (MODE === "malformed-notification") {
		send({ jsonrpc: "2.0", params: { i: 1 } });
		return;
	}
	if (MODE === "rpc-error-secret") {
		send({
			jsonrpc: "2.0",
			id: message.id,
			error: { code: -32603, message: "internal failure while reading key " + SECRET },
		});
		return;
	}
	if (MODE === "result-and-error") {
		send({ jsonrpc: "2.0", id: message.id, result: initializeResult, error: { code: -1, message: SECRET } });
		return;
	}
	if (MODE === "ok-slow-init") {
		setTimeout(() => send({ jsonrpc: "2.0", id: message.id, result: initializeResult }), 150);
		return;
	}
	send({ jsonrpc: "2.0", id: message.id, result: initializeResult });
}

function buildSelectionPayload(args) {
	const items = args?.items ?? [];
	const resultItems = items.map((item, index) => {
		let classification = "relevant";
		let disposition = "keep";
		let probabilities = { relevant: 0.8, irrelevant: 0.1, uncertain: 0.1 };
		if (index === 1) {
			classification = "irrelevant";
			disposition = "drop";
			probabilities = { relevant: 0.05, irrelevant: 0.9, uncertain: 0.05 };
		} else if (index === 2) {
			classification = "uncertain";
			disposition = "review";
			probabilities = { relevant: 0.1, irrelevant: 0.2, uncertain: 0.7 };
		}
		return {
			id: item.id,
			disposition,
			classification,
			probabilities,
			confidence: 0.8,
			source_text_sha256: createHash("sha256").update(item.text).digest("hex"),
		};
	});
	const selected_ids = resultItems.filter((item) => item.disposition !== "drop").map((item) => item.id);
	const payload = {
		status: selected_ids.length === 0 ? "no_match" : resultItems.some((item) => item.disposition === "review") ? "review" : "selected",
		selected_ids,
		items: resultItems,
		rubric_version: "evidence-selection-v1",
		model: "jev-1.13.0",
		usage: { input_tokens: 321, output_tokens: 45 },
	};
	if (MODE === "selection-bad-hash" && payload.items[0]) payload.items[0].source_text_sha256 = "0".repeat(64);
	if (MODE === "selection-bad-status") payload.status = "no_match";
	if (MODE === "selection-extra-field") payload.debug = SECRET;
	return payload;
}

function onCall(message) {
	if (MODE === "hang-call") return;
	if (MODE === "exit-before-response") {
		record({ event: "exiting" });
		process.exit(3);
	}
	if (MODE === "tool-error-secret" || MODE === "stderr-secret") {
		if (MODE === "stderr-secret") process.stderr.write("server log: request failed with key " + SECRET + "\n");
		send({
			jsonrpc: "2.0",
			id: message.id,
			result: { content: [{ type: "text", text: "the evaluation failed while using key " + SECRET }], isError: true },
		});
		return;
	}

	const payload = message.params?.name === "select_evidence"
		? buildSelectionPayload(message.params?.arguments)
		: buildPayload(message.params?.arguments);
	const text = JSON.stringify(payload);

	if (MODE === "selection-iserror-string") {
		send({ jsonrpc: "2.0", id: message.id, result: {
			content: [{ type: "text", text }], structuredContent: payload, isError: "false",
		} });
		return;
	}
	if (MODE === "selection-missing-content") {
		send({ jsonrpc: "2.0", id: message.id, result: { structuredContent: payload } });
		return;
	}
	if (MODE === "selection-mismatched-copies") {
		const other = JSON.parse(JSON.stringify(payload));
		other.status = "no_match";
		send({ jsonrpc: "2.0", id: message.id, result: {
			content: [{ type: "text", text }], structuredContent: other,
		} });
		return;
	}

	if (MODE === "ok-text-only") {
		send({ jsonrpc: "2.0", id: message.id, result: { content: [{ type: "text", text }] } });
		return;
	}
	if (MODE === "ok-structured-only-note") {
		send({
			jsonrpc: "2.0",
			id: message.id,
			result: {
				content: [{ type: "text", text: "the evaluation succeeded; its answers were too large to repeat as text and are in structuredContent" }],
				structuredContent: payload,
			},
		});
		return;
	}
	if (MODE === "structured-invalid-text-valid") {
		const valid = buildPayload(message.params?.arguments);
		const invalid = JSON.parse(JSON.stringify(valid));
		invalid.model = "jev-9.9.9";
		invalid.leak = SECRET;
		send({
			jsonrpc: "2.0",
			id: message.id,
			result: { content: [{ type: "text", text: JSON.stringify(valid) }], structuredContent: invalid },
		});
		return;
	}

	if (MODE === "result-then-garbage") {
		// One write, so the valid result and the protocol violation reach the adapter in
		// the same chunk and are processed before it inspects the answer.
		const line = JSON.stringify({
			jsonrpc: "2.0",
			id: message.id,
			result: { content: [{ type: "text", text }], structuredContent: payload },
		});
		process.stdout.write(line + "\nthis trailing line is not json\n");
		return;
	}

	send({
		jsonrpc: "2.0",
		id: message.id,
		result: { content: [{ type: "text", text }], structuredContent: payload },
	});
}

let buffer = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
	buffer += chunk;
	let index = buffer.indexOf("\n");
	while (index !== -1) {
		const line = buffer.slice(0, index);
		buffer = buffer.slice(index + 1);
		if (line.trim().length > 0) handleLine(line);
		index = buffer.indexOf("\n");
	}
});

process.stdin.on("end", () => {
	record({ event: "stdin-end" });
	if (MODE === "garbage-on-close" || MODE === "fragment-on-close") {
		process.stdout.write(MODE === "garbage-on-close" ? "late garbage\n" : "unfinished", () => process.exit(0));
		return;
	}
	if (MODE === "nonzero-on-close") process.exit(7);
	if (MODE !== "ignore-term") process.exit(0);
});

function handleLine(line) {
	let message;
	try {
		message = JSON.parse(line);
	} catch {
		record({ event: "unparsable" });
		return;
	}
	record({ event: "recv", method: message.method ?? null, id: message.id ?? null });
	if (message.method === "initialize") return onInitialize(message);
	if (message.method === "notifications/initialized") return;
	if (message.method === "tools/call") {
		record({
			event: "call-args",
			name: message.params?.name ?? null,
			questionIds: Object.keys(message.params?.arguments?.questions ?? {}),
			arguments: message.params?.arguments ?? null,
		});
		return onCall(message);
	}
}
`;

/** Create a temporary directory tree holding a fake server executable. */
export function createFakeServer(mode: FakeServerMode): FakeServer {
	const root = resolve("temp");
	mkdirSync(root, { recursive: true });
	const home = mkdtempSync(join(root, "pi-subprocess-"));
	const binPath = join(home, "fake-jev-mcp.mjs");
	const script = `#!${process.execPath}\n${SCRIPT_TEMPLATE.replace("__MODE__", JSON.stringify(mode)).replace(
		"__SECRET__",
		JSON.stringify(SECRET_SENTINEL),
	)}`;
	writeFileSync(binPath, script, "utf8");
	chmodSync(binPath, 0o755);

	const readLog = (): LogEntry[] => {
		let raw: string;
		try {
			raw = readFileSync(join(home, "received.log"), "utf8");
		} catch {
			return [];
		}
		return raw
			.split("\n")
			.filter((line) => line.trim().length > 0)
			.map((line) => JSON.parse(line) as LogEntry);
	};

	const fixture: FakeServer = {
		binPath,
		home,
		readLog,
		pid(): number | undefined {
			const start = readLog().find((entry) => entry.event === "start");
			const pid = start?.pid;
			return typeof pid === "number" ? pid : undefined;
		},
	};
	fixtures.push(fixture);
	return fixture;
}

/** Environment handed to the adapter for a fake-server run (not the child environment). */
export function fakeServerEnv(
	server: FakeServer,
	extra: Record<string, string> = {},
): NodeJS.ProcessEnv {
	return {
		HOME: server.home,
		PATH: "/usr/bin:/bin:/usr/local/bin",
		LANG: "en_US.UTF-8",
		JEV_MCP_BIN: server.binPath,
		// Ambient values that must never reach the child.
		ANTHROPIC_API_KEY: "sk-ambient-must-not-leak",
		HTTPS_PROXY: "http://proxy.invalid:8080",
		TYPESAFE_API_KEY: "ts-ambient-must-not-leak",
		...extra,
	};
}

/** Whether a pid is still alive. Used to confirm children are reaped. */
export function isAlive(pid: number | undefined): boolean {
	if (pid === undefined) return false;
	try {
		process.kill(pid, 0);
		return true;
	} catch {
		return false;
	}
}
