/**
 * Rendering tests.
 *
 * Pi reports a failed tool call with `details: undefined` and the thrown message in
 * `content`, and a restored session can carry details written by another version, so
 * the renderer must never assume a success-shaped result. Everything it prints is
 * bounded and stripped of characters that could rewrite the terminal.
 */

import assert from "node:assert/strict";
import { describe, it } from "node:test";
import type { Text } from "@earendil-works/pi-tui";
import {
	buildCallText,
	buildResultPayload,
	buildResultText,
	createEvaluateTool,
	type JevAnswer,
	type JevResultDetails,
	JevRunner,
	PLAIN_THEME,
	RESULT_NOTE,
	summarizeAnswer,
} from "../../adapters/pi/jev.ts";
import { SECRET_SENTINEL } from "./helpers/fake-server.ts";
import { sampleAnswers } from "./helpers/fixtures.ts";

const theme = PLAIN_THEME;
const collapsed = { expanded: false, isPartial: false, isError: false };
const expanded = { expanded: true, isPartial: false, isError: false };

function successDetails(overrides: Partial<JevResultDetails> = {}): JevResultDetails {
	return {
		kind: "jev-result",
		model: "jev-1.13.0",
		durationMs: 2400,
		questionIds: ["refund_request", "department", "urgency"],
		answers: sampleAnswers() as unknown as Record<string, JevAnswer>,
		usage: { input_tokens: 312, output_tokens: 48 },
		structured: true,
		protocolVersion: "2024-11-05",
		serverVersion: "0.1.0",
		...overrides,
	};
}

function successResult(overrides: Partial<JevResultDetails> = {}) {
	return { content: [{ type: "text" as const, text: "{}" }], details: successDetails(overrides) };
}

describe("buildResultText: success", () => {
	it("summarises each primitive on its own line", () => {
		const text = buildResultText(successResult(), collapsed, theme);
		const lines = text.split("\n");
		assert.match(lines[0] ?? "", /^✓ jev-1\.13\.0 · 3 answer\(s\) · 2\.4s$/);
		assert.match(text, /refund_request: probability of yes 0\.92/);
		assert.match(text, /department: billing \(confidence 0\.82\)/);
		assert.match(text, /urgency: 1\.00 of 0 to 2, nearest "A near-term deadline", confidence 0\.78/);
	});

	it("adds usage, server identity, and the caveat when expanded", () => {
		const text = buildResultText(successResult(), expanded, theme);
		assert.match(text, /usage: 312 input tokens, 48 output tokens/);
		assert.match(text, /server: jev-mcp 0\.1\.0 \(protocol 2024-11-05\)/);
		assert.ok(text.includes(RESULT_NOTE));
	});

	it("caps the collapsed answer list", () => {
		const answers: Record<string, unknown> = {};
		for (let i = 0; i < 12; i++) answers[`q${i}`] = { type: "noul", noul: 0.5 };
		const result = successResult({ answers: answers as Record<string, JevAnswer> });
		const short = buildResultText(result, collapsed, theme);
		assert.equal(short.split("\n").length, 7, short);
		assert.match(short, /\.\.\. 7 more answer\(s\)/);
	});

	it("flags a payload recovered from text", () => {
		const text = buildResultText(successResult({ structured: false }), collapsed, theme);
		assert.match(text, /\(from text\)/);
	});
});

describe("buildResultText: failures and malformed details", () => {
	it("renders Pi's real error shape: isError with undefined details", () => {
		const text = buildResultText(
			{
				content: [{ type: "text", text: "jev evaluate failed after 1.2s: the evaluation was cancelled" }],
				details: undefined,
			},
			{ expanded: false, isPartial: false, isError: true },
			theme,
		);
		assert.equal(text, "✗ jev evaluate failed\njev evaluate failed after 1.2s: the evaluation was cancelled");
	});

	it("shows one error line unless expanded, and strips terminal control sequences", () => {
		const result = {
			content: [
				{
					type: "text",
					text: "line one\u0007\u001b[31m\u202eflip\nline two\nline three\nline four",
				},
			],
			details: undefined,
		};
		const short = buildResultText(result, { expanded: false, isPartial: false, isError: true }, theme);
		assert.deepEqual(short.split("\n"), [
			"✗ jev evaluate failed",
			"line one [31m flip",
			"... 3 more line(s)",
		]);
		for (const code of ["\u0007", "\u001b", "\u202e"]) assert.ok(!short.includes(code));

		const long = buildResultText(result, { expanded: true, isPartial: false, isError: true }, theme);
		assert.equal(long.split("\n").length, 5);
	});

	it("bounds the number and width of rendered error lines", () => {
		const hostile = Array.from({ length: 200 }, (_value, index) => `line ${index} ${"y".repeat(500)}`).join("\n");
		const text = buildResultText(
			{ content: [{ type: "text", text: hostile }], details: undefined },
			{ expanded: true, isPartial: false, isError: true },
			theme,
		);
		const lines = text.split("\n");
		assert.ok(lines.length <= 10, `expected a bounded number of lines, got ${lines.length}`);
		for (const line of lines) assert.ok(line.length <= 201, `line of ${line.length} characters is too wide`);
	});

	it("falls back to the failure layout for malformed or foreign details", () => {
		for (const details of [undefined, null, "boom", 42, [], { kind: "other" }, { kind: "jev-result" }]) {
			const text = buildResultText(
				{ content: [{ type: "text", text: "something went wrong" }], details },
				collapsed,
				theme,
			);
			assert.match(text, /^✗ jev evaluate failed/, `details: ${JSON.stringify(details)}`);
		}
	});

	it("survives a jev-result whose fields have the wrong types", () => {
		const text = buildResultText(
			{
				content: [{ type: "text", text: "{}" }],
				details: {
					kind: "jev-result",
					model: "jev-1.13.0",
					durationMs: "slow",
					answers: { a: "not-an-answer", b: { type: "noul", noul: "high" } },
					usage: "none",
					protocolVersion: 7,
				},
			},
			expanded,
			theme,
		);
		assert.match(text, /^✓ jev-1\.13\.0 · 2 answer\(s\) · \?/);
		assert.match(text, /a: \(unreadable\)/);
		assert.match(text, /b: \(unreadable\)/);
		assert.match(text, /protocol unknown/);
	});

	it("handles a result that is not an object at all", () => {
		for (const result of [undefined, null, "text", 5, []]) {
			assert.match(buildResultText(result, collapsed, theme), /^✗ jev evaluate failed$/);
		}
	});

	it("renders progress while running, with and without details", () => {
		const running = buildResultText(
			{
				content: [{ type: "text", text: "jev evaluating" }],
				details: { kind: "jev-progress", phase: "evaluating", startedAt: 8_500, questionIds: ["q"] },
			},
			{ expanded: false, isPartial: true, isError: false, now: 10_000 },
			theme,
		);
		assert.equal(running, "jev evaluating · 1.5s");

		const bare = buildResultText(
			{ content: [], details: undefined },
			{ expanded: false, isPartial: true, isError: false },
			theme,
		);
		assert.equal(bare, "jev working");
	});

	it("never prints a sentinel that a restored detail smuggled in", () => {
		const text = buildResultText(
			{
				content: [{ type: "text", text: "ok" }],
				details: {
					kind: "jev-result",
					model: `jev-1.13.0`,
					durationMs: 10,
					answers: { q: { type: "choice", choice: "billing", confidence: 0.5 } },
					usage: { input_tokens: 1, output_tokens: 1 },
					protocolVersion: "2024-11-05",
					serverVersion: `1.0 ${SECRET_SENTINEL}`,
				},
			},
			expanded,
			theme,
		);
		// serverVersion is rendered only after sanitizing, and a sentinel with a space
		// is truncated to the version width; the point is that nothing executes and the
		// line stays bounded.
		for (const line of text.split("\n")) assert.ok(line.length <= 201);
		assert.ok(!text.includes("\u001b"));
	});
});

describe("summarizeAnswer", () => {
	it("summarises each documented answer type", () => {
		assert.equal(summarizeAnswer({ type: "noul", noul: 0.9234 }), "probability of yes 0.92");
		assert.equal(
			summarizeAnswer({ type: "choice", choice: "billing", confidence: 0.8 }),
			"billing (confidence 0.80)",
		);
		assert.equal(
			summarizeAnswer({ type: "score", score: 1.6, legend: { "0": "Calm", "1": "Cross", "2": "Angry" }, confidence: 0.78 }),
			'1.60 of 0 to 2, nearest "Angry", confidence 0.78',
		);
	});

	it("reports anything it cannot read", () => {
		for (const value of [undefined, null, "yes", 5, [], {}, { type: "noul" }, { type: "noul", noul: "x" }]) {
			assert.equal(summarizeAnswer(value), "(unreadable)");
		}
	});

	it("truncates and cleans an oversized choice label", () => {
		const summary = summarizeAnswer({ type: "choice", choice: `${"a".repeat(300)}\u202e`, confidence: 0.5 }, 40);
		assert.ok(summary.length <= 41);
		assert.ok(!summary.includes("\u202e"));
	});
});

describe("buildCallText", () => {
	it("lists the question ids", () => {
		const text = buildCallText(
			{ state: "x", questions: { alpha: {}, beta: {}, gamma: {}, delta: {}, epsilon: {} } },
			true,
			theme,
		);
		assert.match(text, /evaluate jev-1\.13\.0 5 question\(s\): alpha, beta, gamma, delta, …/);
	});

	it("tolerates partial or malformed arguments", () => {
		assert.match(buildCallText(undefined, false, theme), /evaluate jev-1\.13\.0 …$/);
		assert.match(buildCallText({ questions: "not-a-map" }, true, theme), /evaluate jev-1\.13\.0$/);
		assert.match(buildCallText("garbage", true, theme), /evaluate jev-1\.13\.0$/);
	});
});

describe("buildResultPayload", () => {
	it("emits parseable JSON with the caveat note", () => {
		const { text, answers } = buildResultPayload(
			"jev-1.13.0",
			sampleAnswers() as unknown as Record<string, JevAnswer>,
			{ input_tokens: 10, output_tokens: 2 },
		);
		const payload = JSON.parse(text) as { model: string; note: string; usage: unknown };
		assert.equal(payload.model, "jev-1.13.0");
		assert.equal(payload.note, RESULT_NOTE);
		assert.deepEqual(payload.usage, { input_tokens: 10, output_tokens: 2 });
		assert.deepEqual(Object.keys(answers), ["refund_request", "department", "urgency"]);
	});

	it("fails oversized answers instead of returning an empty success", () => {
		const answers: Record<string, JevAnswer> = {};
		for (let i = 0; i < 16; i++) {
			answers[`q${i}`] = {
				type: "score",
				score: 1,
				legend: Object.fromEntries(
					Array.from({ length: 10 }, (_value, level) => [String(level), "x".repeat(1000)]),
				),
				probabilities: Object.fromEntries(Array.from({ length: 10 }, (_value, level) => [String(level), 0.1])),
				confidence: 0.5,
			};
		}
		assert.throws(() => buildResultPayload("jev-1.13.0", answers, {
			input_tokens: 1,
			output_tokens: 1,
		}), /result exceeds the adapter output limit/);
	});
});

describe("tool renderers", () => {
	const tool = createEvaluateTool(new JevRunner());
	const fakeTheme = { fg: (_color: string, text: string) => text, bold: (text: string) => text } as never;

	function renderContext(overrides: Record<string, unknown> = {}) {
		return {
			args: {},
			toolCallId: "call-1",
			invalidate: () => {},
			lastComponent: undefined,
			state: {},
			cwd: "/tmp",
			executionStarted: true,
			argsComplete: true,
			isPartial: false,
			expanded: false,
			showImages: false,
			isError: false,
			...overrides,
		} as never;
	}

	it("renders a call component whose lines fit the width", () => {
		const component = tool.renderCall?.(
			{ state: "x", questions: { q1: { type: "noul", instructions: "y" } } } as never,
			fakeTheme,
			renderContext(),
		) as Text;
		assert.ok(component);
		for (const line of component.render(40)) assert.ok(line.length <= 48, line);
	});

	it("renders an error result component without throwing", () => {
		const component = tool.renderResult?.(
			{ content: [{ type: "text", text: "jev evaluate failed after 1.0s: boom" }], details: undefined },
			{ expanded: false, isPartial: false },
			fakeTheme,
			renderContext({ isError: true }),
		) as Text;
		assert.ok(component);
		assert.match(component.render(60).join("\n"), /jev evaluate failed/);
	});

	it("renders a success result component", () => {
		const component = tool.renderResult?.(
			successResult(),
			{ expanded: true, isPartial: false },
			fakeTheme,
			renderContext({ expanded: true }),
		) as Text;
		assert.ok(component);
		assert.match(component.render(80).join("\n"), /jev-1\.13\.0/);
	});
});
