/**
 * Response validation. The adapter fails closed: a payload is accepted only when the
 * model, the answer ids, the answer types, the option set, the score legend, the
 * probability distributions, and the usage counters all match the request that
 * produced it. Anything the API did not document is dropped rather than forwarded.
 */

import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
	JEV_MODEL,
	type QuestionSpec,
	validateEvaluateInput,
	validateEvaluationPayload,
} from "../../adapters/pi/jev.ts";
import { SECRET_SENTINEL } from "./helpers/fake-server.ts";
import { sampleRequest } from "./helpers/fixtures.ts";

function specsFor(request: unknown): Record<string, QuestionSpec> {
	const outcome = validateEvaluateInput(request);
	assert.equal(outcome.ok, true, outcome.ok ? "" : outcome.error);
	if (!outcome.ok) throw new Error("unreachable");
	return outcome.specs;
}

const specs = specsFor(sampleRequest());

/** The documented answer shapes for the sample request. */
function officialPayload(): Record<string, unknown> {
	return {
		model: JEV_MODEL,
		answers: {
			refund_request: { type: "noul", noul: 0.92 },
			department: {
				type: "choice",
				choice: "billing",
				probabilities: { billing: 0.85, technical: 0.15 },
				confidence: 0.82,
			},
			urgency: {
				type: "score",
				score: 1.6,
				legend: { "0": "No time pressure", "1": "A near-term deadline", "2": "An immediate deadline" },
				probabilities: { "0": 0.05, "1": 0.3, "2": 0.65 },
				confidence: 0.78,
			},
		},
		usage: { input_tokens: 312, output_tokens: 48 },
	};
}

function expectRejected(mutate: (payload: Record<string, any>) => void, fragment: string): string {
	const payload = officialPayload();
	mutate(payload);
	const outcome = validateEvaluationPayload(payload, specs);
	assert.equal(outcome.ok, false, "expected the payload to be rejected");
	if (outcome.ok) return "";
	assert.match(outcome.reason, new RegExp(fragment, "i"));
	return outcome.reason;
}

describe("validateEvaluationPayload: accepted", () => {
	it("accepts the documented answers for all three primitives", () => {
		const outcome = validateEvaluationPayload(officialPayload(), specs);
		assert.equal(outcome.ok, true, outcome.ok ? "" : outcome.reason);
		if (!outcome.ok) return;

		assert.equal(outcome.payload.model, JEV_MODEL);
		assert.deepEqual(JSON.parse(JSON.stringify(outcome.payload.answers.refund_request)), {
			type: "noul",
			noul: 0.92,
		});
		assert.deepEqual(JSON.parse(JSON.stringify(outcome.payload.answers.department)), {
			type: "choice",
			choice: "billing",
			probabilities: { billing: 0.85, technical: 0.15 },
			confidence: 0.82,
		});
		assert.deepEqual(JSON.parse(JSON.stringify(outcome.payload.answers.urgency)), {
			type: "score",
			score: 1.6,
			legend: { "0": "No time pressure", "1": "A near-term deadline", "2": "An immediate deadline" },
			probabilities: { "0": 0.05, "1": 0.3, "2": 0.65 },
			confidence: 0.78,
		});
		assert.deepEqual(outcome.payload.usage, { input_tokens: 312, output_tokens: 48 });
	});

	it("allows bounded distribution rounding without changing probabilities", () => {
		const outcome = validateEvaluationPayload(
			(() => {
				const payload = officialPayload();
				(payload as any).answers.refund_request.noul = 1;
				(payload as any).answers.department.probabilities = { billing: 0.8505, technical: 0.1499 };
				return payload;
			})(),
			specs,
		);
		assert.equal(outcome.ok, true, outcome.ok ? "" : outcome.reason);
		if (!outcome.ok) return;
		assert.equal((outcome.payload.answers.refund_request as { noul: number }).noul, 1);
	});

	it("strips every member the API does not document", () => {
		const payload = officialPayload() as any;
		payload.debug = SECRET_SENTINEL;
		payload.answers.refund_request.explanation = SECRET_SENTINEL;
		payload.answers.refund_request.confidence = 0.4;
		payload.answers.department.rationale = SECRET_SENTINEL;
		payload.usage.internal_key = SECRET_SENTINEL;

		const outcome = validateEvaluationPayload(payload, specs);
		assert.equal(outcome.ok, true, outcome.ok ? "" : outcome.reason);
		if (!outcome.ok) return;

		const serialized = JSON.stringify(outcome.payload);
		assert.ok(!serialized.includes(SECRET_SENTINEL), "unknown members must not survive validation");
		assert.deepEqual(Object.keys(outcome.payload.answers.refund_request ?? {}), ["type", "noul"]);
		assert.deepEqual(Object.keys(outcome.payload.usage).sort(), ["input_tokens", "output_tokens"]);
	});

	it("rebuilds the score legend from the request rather than the response", () => {
		const payload = officialPayload();
		const outcome = validateEvaluationPayload(payload, specs);
		assert.equal(outcome.ok, true);
		if (!outcome.ok) return;
		const legend = (outcome.payload.answers.urgency as { legend: Record<string, string> }).legend;
		assert.deepEqual(Object.values(legend), specs.urgency?.levels);
	});
});

describe("validateEvaluationPayload: fails closed", () => {
	it("rejects a model other than the pinned one", () => {
		const reason = expectRejected((payload) => {
			payload.model = `jev-9.9.9 ${SECRET_SENTINEL}`;
		}, `a model other than ${JEV_MODEL}`);
		assert.ok(!reason.includes(SECRET_SENTINEL));
	});

	it("rejects a missing, non-string, or oversized model", () => {
		expectRejected((payload) => {
			delete payload.model;
		}, "usable model");
		expectRejected((payload) => {
			payload.model = 7;
		}, "usable model");
		expectRejected((payload) => {
			payload.model = "x".repeat(100);
		}, "usable model");
	});

	it("rejects answers that are not exactly the questions that were asked", () => {
		expectRejected((payload) => {
			delete payload.answers.urgency;
		}, "exactly the questions");
		expectRejected((payload) => {
			payload.answers.unasked = { type: "noul", noul: 0.5 };
		}, "exactly the questions");
		expectRejected((payload) => {
			payload.answers = [];
		}, "answers object");
		expectRejected((payload) => {
			delete payload.answers;
		}, "answers object");
	});

	it("rejects an answer whose type does not match its question", () => {
		expectRejected((payload) => {
			payload.answers.urgency = { type: "noul", noul: 0.5 };
		}, 'answer for question "urgency"');
	});

	it("rejects a noul value outside 0 to 1, or missing", () => {
		expectRejected((payload) => {
			payload.answers.refund_request.noul = 1.5;
		}, "refund_request");
		expectRejected((payload) => {
			payload.answers.refund_request.noul = "0.9";
		}, "refund_request");
		expectRejected((payload) => {
			delete payload.answers.refund_request.noul;
		}, "refund_request");
	});

	it("rejects a choice that was never offered", () => {
		expectRejected((payload) => {
			payload.answers.department.choice = "sales";
		}, "department");
	});

	it("rejects a choice inconsistent with the probability argmax", () => {
		expectRejected((payload) => { payload.answers.department.choice = "technical"; }, "department");
	});

	it("rejects a score inconsistent with the distribution mean", () => {
		expectRejected((payload) => { payload.answers.urgency.score = 0.5; }, "urgency");
	});

	it("rejects even small out-of-range probability and score values", () => {
		for (const value of [-0.0000004, 1.0000004]) {
			expectRejected((payload) => { payload.answers.refund_request.noul = value; }, "refund_request");
		}
		expectRejected((payload) => { payload.answers.urgency.score = 2.0000004; }, "urgency");
	});

	it("rejects distributions that miss a key, add a key, or fail to sum to 1", () => {
		expectRejected((payload) => {
			payload.answers.department.probabilities = { billing: 1 };
		}, "department");
		expectRejected((payload) => {
			payload.answers.department.probabilities = { billing: 0.5, sales: 0.5 };
		}, "department");
		expectRejected((payload) => {
			payload.answers.department.probabilities = { billing: 0.9, technical: 0.9 };
		}, "department");
		expectRejected((payload) => {
			payload.answers.urgency.probabilities = { "0": 0.1, "1": 0.2, "2": 0.3 };
		}, "urgency");
	});

	it("rejects a non-finite probability written as a huge exponent", () => {
		const payload = JSON.parse(
			JSON.stringify(officialPayload()).replace('"noul":0.92', '"noul":1e999'),
		);
		const outcome = validateEvaluationPayload(payload, specs);
		assert.equal(outcome.ok, false);
	});

	it("rejects a legend that does not match the rubric that was sent", () => {
		const reason = expectRejected((payload) => {
			payload.answers.urgency.legend["0"] = `rewritten ${SECRET_SENTINEL}`;
		}, "urgency");
		assert.ok(!reason.includes(SECRET_SENTINEL), "a rejected legend must not be echoed");

		expectRejected((payload) => {
			delete payload.answers.urgency.legend["2"];
		}, "urgency");
		expectRejected((payload) => {
			payload.answers.urgency.legend.extra = "another";
		}, "urgency");
	});

	it("rejects a score outside the range of its levels", () => {
		expectRejected((payload) => {
			payload.answers.urgency.score = 2.5;
		}, "urgency");
		expectRejected((payload) => {
			payload.answers.urgency.score = -1;
		}, "urgency");
	});

	it("rejects a missing confidence on choice and score", () => {
		expectRejected((payload) => {
			delete payload.answers.department.confidence;
		}, "department");
		expectRejected((payload) => {
			delete payload.answers.urgency.confidence;
		}, "urgency");
	});

	it("rejects unusable usage counters", () => {
		expectRejected((payload) => {
			delete payload.usage;
		}, "token usage");
		expectRejected((payload) => {
			payload.usage = { input_tokens: 1.5, output_tokens: 2 };
		}, "token usage");
		expectRejected((payload) => {
			payload.usage = { input_tokens: -1, output_tokens: 2 };
		}, "token usage");
		expectRejected((payload) => {
			payload.usage = { input_tokens: 2 };
		}, "token usage");
		expectRejected((payload) => {
			payload.usage = { input_tokens: 2 ** 41, output_tokens: 2 };
		}, "token usage");
	});

	it("rejects a payload that is not an object", () => {
		for (const value of [undefined, null, "text", 5, []]) {
			const outcome = validateEvaluationPayload(value, specs);
			assert.equal(outcome.ok, false);
		}
	});

	it("does not read inherited members as answers", () => {
		const payload = officialPayload() as any;
		payload.answers.refund_request = Object.create({ type: "noul", noul: 0.9 });
		const outcome = validateEvaluationPayload(payload, specs);
		assert.equal(outcome.ok, false);
	});
});
