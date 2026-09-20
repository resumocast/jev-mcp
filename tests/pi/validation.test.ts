/**
 * Request validation. The shapes under test come from https://docs.typesafe.ai/api.md:
 * noul takes an optional {true,false} criteria object, choice requires an option map
 * whose values are descriptions or null, and score requires an ordered array of 2 to
 * 10 level descriptions.
 */

import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { Compile } from "typebox/compile";
import { Value } from "typebox/value";
import {
	EvaluateParams,
	MAX_CHOICE_OPTIONS,
	MAX_QUESTIONS,
	MAX_REQUEST_BYTES,
	MAX_SCORE_LEVELS,
	validateEvaluateInput,
} from "../../adapters/pi/jev.ts";
import { officialExampleRequest, sampleRequest } from "./helpers/fixtures.ts";
import { SECRET_SENTINEL } from "./helpers/fake-server.ts";

function expectAccepted(input: unknown): void {
	const outcome = validateEvaluateInput(input);
	assert.equal(outcome.ok, true, outcome.ok ? "" : `unexpected rejection: ${outcome.error}`);
}

function expectRejected(input: unknown, fragment: string): string {
	const outcome = validateEvaluateInput(input);
	assert.equal(outcome.ok, false, "expected a rejection");
	if (outcome.ok) return "";
	assert.match(outcome.error, new RegExp(fragment, "i"));
	return outcome.error;
}

describe("validateEvaluateInput: accepted shapes", () => {
	it("accepts the official example request committed at examples/evaluate.json", () => {
		const example = officialExampleRequest();
		const outcome = validateEvaluateInput(example);
		assert.equal(outcome.ok, true, outcome.ok ? "" : `official example rejected: ${outcome.error}`);
		if (!outcome.ok) return;
		assert.deepEqual(Object.keys(outcome.request.questions), ["refund_request", "department", "urgency"]);
		assert.deepEqual(outcome.specs.department?.options, ["billing", "technical", "sales"]);
		assert.deepEqual(outcome.specs.urgency?.levels, [
			"No time pressure is stated",
			"A near-term deadline is stated",
			"An immediate deadline or ongoing serious impact is stated",
		]);
		assert.equal(outcome.specs.refund_request?.type, "noul");
		assert.equal(outcome.specs.refund_request?.options, undefined);
	});

	it("accepts a noul question with and without criteria", () => {
		expectAccepted({ state: "x", questions: { q: { type: "noul", instructions: "Is it urgent?" } } });
		expectAccepted({
			state: "x",
			questions: {
				q: {
					type: "noul",
					instructions: "Is it urgent?",
					criteria: { true: "Explicitly time-sensitive", false: "No urgency expressed" },
				},
			},
		});
		expectAccepted({
			state: "x",
			questions: { q: { type: "noul", instructions: "Is it urgent?", criteria: { true: "yes only" } } },
		});
	});

	it("accepts choice criteria with null descriptions", () => {
		const outcome = validateEvaluateInput({
			state: "x",
			questions: {
				q: { type: "choice", instructions: "Which team?", criteria: { billing: "Refunds", technical: null } },
			},
		});
		assert.equal(outcome.ok, true);
		if (!outcome.ok) return;
		assert.deepEqual(
			JSON.parse(JSON.stringify(outcome.request.questions.q?.criteria)),
			{ billing: "Refunds", technical: null },
		);
		assert.deepEqual(outcome.specs.q?.options, ["billing", "technical"]);
	});

	it("accepts a single choice option and the documented ceiling", () => {
		expectAccepted({ state: "x", questions: { q: { type: "choice", instructions: "pick", criteria: { only: null } } } });
		const many: Record<string, null> = {};
		for (let i = 0; i < MAX_CHOICE_OPTIONS; i++) many[`o${i}`] = null;
		expectAccepted({ state: "x", questions: { q: { type: "choice", instructions: "pick", criteria: many } } });
	});

	it("accepts 2 to 10 score levels", () => {
		expectAccepted({ state: "x", questions: { q: { type: "score", instructions: "rate", criteria: ["low", "high"] } } });
		const ten = Array.from({ length: MAX_SCORE_LEVELS }, (_value, index) => `level ${index}`);
		expectAccepted({ state: "x", questions: { q: { type: "score", instructions: "rate", criteria: ten } } });
	});

	it("accepts structured state and instructions", () => {
		expectAccepted({
			state: { ticket: { body: "text" }, history: ["a", "b"] },
			questions: { q: { type: "noul", instructions: { question: "Is it urgent?", context: ["x"] } } },
		});
		expectAccepted({ state: ["a", "b"], questions: { q: { type: "noul", instructions: ["ask", "this"] } } });
	});

	it("preserves the caller's values exactly", () => {
		const request = sampleRequest();
		const outcome = validateEvaluateInput(structuredClone(request));
		assert.equal(outcome.ok, true);
		if (!outcome.ok) return;
		assert.deepEqual(JSON.parse(JSON.stringify(outcome.request)), JSON.parse(JSON.stringify(request)));
	});
});

describe("validateEvaluateInput: criteria rules per type", () => {
	it("rejects a choice question whose criteria are an array", () => {
		expectRejected(
			{ state: "x", questions: { q: { type: "choice", instructions: "pick", criteria: ["a", "b"] } } },
			"must be an object mapping each option",
		);
	});

	it("rejects a score question whose criteria are an object", () => {
		expectRejected(
			{ state: "x", questions: { q: { type: "score", instructions: "rate", criteria: { low: "bad", high: "good" } } } },
			"must be an array of level descriptions",
		);
	});

	it("requires criteria for choice and score", () => {
		expectRejected({ state: "x", questions: { q: { type: "choice", instructions: "pick" } } }, "are required");
		expectRejected({ state: "x", questions: { q: { type: "score", instructions: "rate" } } }, "are required");
	});

	it("rejects noul criteria that are not true or false members", () => {
		expectRejected(
			{ state: "x", questions: { q: { type: "noul", instructions: "ask", criteria: { yes: "a", no: "b" } } } },
			'only "true" and "false"',
		);
		expectRejected(
			{ state: "x", questions: { q: { type: "noul", instructions: "ask", criteria: {} } } },
			"must describe",
		);
		expectRejected(
			{ state: "x", questions: { q: { type: "noul", instructions: "ask", criteria: ["a", "b"] } } },
			"must be an object",
		);
	});

	it("rejects criteria sent as null rather than omitted", () => {
		expectRejected(
			{ state: "x", questions: { q: { type: "noul", instructions: "ask", criteria: null } } },
			"omit criteria rather than send null",
		);
	});

	it("rejects a null description for score and for noul", () => {
		expectRejected(
			{ state: "x", questions: { q: { type: "score", instructions: "rate", criteria: ["low", null] } } },
			"descriptions must be strings",
		);
		expectRejected(
			{ state: "x", questions: { q: { type: "noul", instructions: "ask", criteria: { true: null } } } },
			"descriptions must be strings",
		);
	});

	it("rejects score level counts outside 2 to 10", () => {
		expectRejected(
			{ state: "x", questions: { q: { type: "score", instructions: "rate", criteria: ["only"] } } },
			"at least 2 levels",
		);
		const eleven = Array.from({ length: MAX_SCORE_LEVELS + 1 }, (_value, index) => `level ${index}`);
		expectRejected(
			{ state: "x", questions: { q: { type: "score", instructions: "rate", criteria: eleven } } },
			"at most 10 levels",
		);
	});

	it("rejects too many choice options and unusable option names", () => {
		const many: Record<string, null> = {};
		for (let i = 0; i <= MAX_CHOICE_OPTIONS; i++) many[`o${i}`] = null;
		expectRejected(
			{ state: "x", questions: { q: { type: "choice", instructions: "pick", criteria: many } } },
			"more than 255 options",
		);
		expectRejected(
			{ state: "x", questions: { q: { type: "choice", instructions: "pick", criteria: { "": null, b: null } } } },
			"empty or oversized option name",
		);
		expectRejected(
			{ state: "x", questions: { q: { type: "choice", instructions: "pick", criteria: { "a\u0007b": null } } } },
			"control characters",
		);
	});

	it("rejects descriptions with control characters or excessive length", () => {
		expectRejected(
			{ state: "x", questions: { q: { type: "choice", instructions: "pick", criteria: { a: "line\u202eflip" } } } },
			"control characters",
		);
		expectRejected(
			{ state: "x", questions: { q: { type: "score", instructions: "rate", criteria: ["a".repeat(2000), "b"] } } },
			"longer than 1024 bytes",
		);
	});
});

describe("validateEvaluateInput: rejected shapes", () => {
	it("rejects non-object arguments", () => {
		expectRejected("nope", "must be a JSON object");
		expectRejected(["a"], "must be a JSON object");
		expectRejected(null, "must be a JSON object");
	});

	it("rejects unsupported fields without echoing their names", () => {
		const topLevel = expectRejected(
			{ state: "x", questions: { q: { type: "noul", instructions: "y" } }, [SECRET_SENTINEL]: 1 },
			"accept only state and questions",
		);
		assert.ok(!topLevel.includes(SECRET_SENTINEL), "an unknown argument name must not be echoed");

		const perQuestion = expectRejected(
			{ state: "x", questions: { q: { type: "noul", instructions: "y", [SECRET_SENTINEL]: 1 } } },
			"accepts only type, instructions, and criteria",
		);
		assert.ok(!perQuestion.includes(SECRET_SENTINEL), "an unknown question field must not be echoed");
	});

	it("reports an unusable question id by position and never echoes it", () => {
		const hostileId = `bad id ${SECRET_SENTINEL}\u001b[31m`;
		const message = expectRejected(
			{ state: "x", questions: { [hostileId]: { type: "noul", instructions: "y" } } },
			"the id of question 1",
		);
		assert.ok(!message.includes(SECRET_SENTINEL));
		assert.ok(!message.includes("bad id"));

		const second = expectRejected(
			{
				state: "x",
				questions: { good: { type: "noul", instructions: "y" }, "bad!": { type: "noul", instructions: "y" } },
			},
			"the id of question 2",
		);
		assert.ok(!second.includes("bad!"));
	});

	it("rejects invalid state values", () => {
		expectRejected({ state: 5, questions: { q: { type: "noul", instructions: "y" } } }, "string, object, or array");
		expectRejected({ state: "   ", questions: { q: { type: "noul", instructions: "y" } } }, "state is empty");
		expectRejected({ state: "x".repeat(40000), questions: { q: { type: "noul", instructions: "y" } } }, "larger than");
		expectRejected(
			{ state: { value: Number.POSITIVE_INFINITY }, questions: { q: { type: "noul", instructions: "y" } } },
			"not finite",
		);
	});

	it("rejects state nested deeper than the scanner allows, without building a path string", () => {
		let nested: unknown = "leaf";
		for (let i = 0; i < 12; i++) nested = { [`k${i}`]: nested };
		const message = expectRejected({ state: nested, questions: { q: { type: "noul", instructions: "y" } } }, "nested deeper");
		assert.ok(message.length < 120, `error should stay short, got ${message.length} characters`);
	});

	it("rejects malformed question maps", () => {
		expectRejected({ state: "x" }, "questions must be an object");
		expectRejected({ state: "x", questions: [] }, "questions must be an object");
		expectRejected({ state: "x", questions: {} }, "at least one question");
		const many: Record<string, unknown> = {};
		for (let i = 0; i <= MAX_QUESTIONS; i++) many[`q${i}`] = { type: "noul", instructions: "y" };
		expectRejected({ state: "x", questions: many }, `at most ${MAX_QUESTIONS}`);
		expectRejected({ state: "x", questions: { q: "nope" } }, "must be an object");
	});

	it("rejects invalid question types and instructions", () => {
		expectRejected({ state: "x", questions: { q: { type: "bogus", instructions: "y" } } }, "must have type");
		expectRejected({ state: "x", questions: { q: { type: "noul", instructions: "  " } } }, "is empty");
		expectRejected({ state: "x", questions: { q: { type: "noul", instructions: 3 } } }, "must be a string");
	});

	it("rejects a request larger than the server request limit", () => {
		const questions: Record<string, unknown> = {};
		for (let i = 0; i < MAX_QUESTIONS; i++) {
			questions[`q${i}`] = { type: "noul", instructions: "y".repeat(8000) };
		}
		expectRejected({ state: "x", questions }, `limit is ${MAX_REQUEST_BYTES} bytes`);
	});

	it("counts the model the server adds when measuring the request", () => {
		// A request just under the limit on its own fields is still measured with the
		// pinned model included, the way the server will build it.
		const filler = "y".repeat(MAX_REQUEST_BYTES - 120);
		const outcome = validateEvaluateInput({
			state: "x",
			questions: { q: { type: "noul", instructions: filler } },
		});
		if (outcome.ok) {
			assert.ok(outcome.requestBytes > filler.length, "the measured size must include the envelope");
			assert.ok(JSON.stringify({ state: "x" }).length < outcome.requestBytes);
		}
	});
});

describe("validateEvaluateInput: prototype safety", () => {
	const reserved = ["__proto__", "constructor", "prototype", "toString"];

	it("keeps reserved question ids as data and does not touch any prototype", () => {
		// Built from JSON text on purpose: an object literal with a "__proto__" key sets
		// the prototype instead of creating a member, while JSON.parse creates the own
		// property that a provider's tool arguments would actually carry.
		const questionEntries = reserved
			.map((id) => `${JSON.stringify(id)}:{"type":"noul","instructions":${JSON.stringify(`ask about ${id}`)}}`)
			.join(",");
		const outcome = validateEvaluateInput(JSON.parse(`{"state":"x","questions":{${questionEntries}}}`));

		assert.equal(outcome.ok, true, outcome.ok ? "" : outcome.error);
		if (!outcome.ok) return;

		assert.deepEqual(Object.keys(outcome.request.questions).sort(), [...reserved].sort());
		assert.deepEqual(Object.keys(outcome.specs).sort(), [...reserved].sort());
		for (const id of reserved) {
			const question: unknown = outcome.request.questions[id];
			assert.equal(typeof question, "object", `question "${id}" must survive as data`);
			assert.equal((question as { type?: string } | undefined)?.type, "noul");
		}
		assert.equal(Object.getPrototypeOf(outcome.request.questions), null);
		assert.equal(Object.getPrototypeOf(outcome.specs), null);
		assert.equal(({} as Record<string, unknown>).polluted, undefined);
		assert.equal(JSON.stringify(outcome.request.questions.__proto__), '{"type":"noul","instructions":"ask about __proto__"}');
	});

	it("does not let a reserved criteria key change an object's prototype", () => {
		const outcome = validateEvaluateInput(
			JSON.parse(
				'{"state":"x","questions":{"q":{"type":"choice","instructions":"pick","criteria":{"__proto__":"a","other":null}}}}',
			),
		);
		assert.equal(outcome.ok, true, outcome.ok ? "" : outcome.error);
		if (!outcome.ok) return;
		assert.deepEqual(outcome.specs.q?.options, ["__proto__", "other"]);
		assert.equal(Object.getPrototypeOf(outcome.request.questions.q?.criteria), null);
		assert.equal(({} as Record<string, unknown>).polluted, undefined);
	});

	it("does not read inherited members as question fields", () => {
		const question = Object.create({ type: "noul", instructions: "inherited" }) as Record<string, unknown>;
		expectRejected({ state: "x", questions: { q: question } }, "must have type");
	});
});

describe("provider-facing parameter schema", () => {
	const validator = Compile(EvaluateParams);

	function check(input: unknown): boolean {
		const cloned = structuredClone(input);
		Value.Convert(EvaluateParams, cloned);
		return validator.Check(cloned);
	}

	it("emits plain JSON Schema without pattern properties or regexes", () => {
		const json = JSON.stringify(EvaluateParams);
		assert.ok(!json.includes("patternProperties"), "schema must not use patternProperties");
		assert.ok(!json.includes('"pattern"'), "schema must not use regex patterns");
	});

	it("documents the real criteria rules", () => {
		const schema = EvaluateParams as unknown as {
			properties: { questions: { additionalProperties: { properties: { criteria: { description: string } } } } };
		};
		const description = schema.properties.questions.additionalProperties.properties.criteria.description;
		assert.match(description, /noul: optional object with "true"\/"false" string descriptions/);
		assert.match(description, /choice: required map of 1-255 options to a string description or null/);
		assert.match(description, /score: required array of 2-10 string descriptions/);
		assert.match(description, /indices are 0 to N-1/);
	});

	it("accepts the official example and the documented shapes", () => {
		assert.ok(check(officialExampleRequest()));
		assert.ok(check(sampleRequest()));
		assert.ok(check({ state: "text", questions: { q: { type: "noul", instructions: "is it ok?" } } }));
	});

	it("rejects shapes the adapter would refuse anyway", () => {
		assert.ok(!check({ state: 5, questions: {} }));
		assert.ok(!check({ state: "x", questions: { q: { type: "other", instructions: "y" } } }));
		assert.ok(!check({ state: "x", questions: { q: { instructions: "y" } } }));
		assert.ok(!check({ state: "x", questions: { q: { type: "noul", instructions: "y" } }, extra: true }));
		assert.ok(!check({ state: "x", questions: { q: { type: "noul", instructions: "y", temperature: 1 } } }));
	});
});
