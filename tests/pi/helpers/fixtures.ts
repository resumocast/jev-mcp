/**
 * Shared fixtures: the official example request that ships in examples/evaluate.json,
 * and the answer shapes the API reference documents for the three primitives.
 */

import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

/** The example request committed at examples/evaluate.json, parsed fresh each call. */
export function officialExampleRequest(): Record<string, unknown> {
	const path = resolve(here, "../../../examples/evaluate.json");
	return JSON.parse(readFileSync(path, "utf8")) as Record<string, unknown>;
}

/** A small request covering all three primitives, including a null choice description. */
export function sampleRequest() {
	return {
		state: "The customer writes that a duplicate charge must be refunded today.",
		questions: {
			refund_request: {
				type: "noul" as const,
				instructions: "Does the message explicitly request a refund?",
				criteria: { true: "A refund is requested", false: "No refund is requested" },
			},
			department: {
				type: "choice" as const,
				instructions: "Which department should review this message?",
				criteria: { billing: "Charges and refunds", technical: null },
			},
			urgency: {
				type: "score" as const,
				instructions: "How urgent is the request?",
				criteria: ["No time pressure", "A near-term deadline", "An immediate deadline"],
			},
		},
	};
}

/** The answers the fake server produces for sampleRequest(), as the adapter should keep them. */
export function sampleAnswers() {
	return {
		refund_request: { type: "noul", noul: 0.92 },
		department: {
			type: "choice",
			choice: "billing",
			probabilities: { billing: 0.5, technical: 0.5 },
			confidence: 0.82,
		},
		urgency: {
			type: "score",
			score: 1,
			legend: { "0": "No time pressure", "1": "A near-term deadline", "2": "An immediate deadline" },
			probabilities: { "0": 1 / 3, "1": 1 / 3, "2": 1 - (1 / 3) * 2 },
			confidence: 0.78,
		},
	};
}
