import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { after, before, describe, it } from "node:test";
import {
	assertSelectedResultFits,
	buildContextResultText,
	collectCommittedContext,
	conservativeSelectedResultBytes,
	ContextCollector,
	createPersistentSelectionBudget,
	createSelectContextTool,
	JevRunner,
	PLAIN_THEME,
	SELECT_CONTEXT_TOOL_NAME,
	SELECTION_RESERVATION_ENTRY,
	validateSelectContextInput,
	validateSelectionRoot,
} from "../../adapters/pi/jev.ts";
import { createFakeServer, SECRET_SENTINEL } from "./helpers/fake-server.ts";

let repo = "";
let oldCommit = "";
let currentCommit = "";

function git(...args: string[]): string {
	return execFileSync("git", ["-C", repo, ...args], { encoding: "utf8" }).trim();
}

function commit(message: string): string {
	git("add", ".");
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", message);
	return git("rev-parse", "HEAD");
}

before(() => {
	const root = resolve("temp");
	mkdirSync(root, { recursive: true });
	repo = realpathSync(mkdtempSync(join(root, "context-repo-")));
	git("init", "-q");
	mkdirSync(join(repo, "src"), { recursive: true });
	writeFileSync(join(repo, "src", "one.ts"), "alpha\nbeta\ngamma\ndelta\n", "utf8");
	writeFileSync(join(repo, "src", "two.ts"), "unrelated\nmaterial\n", "utf8");
	writeFileSync(join(repo, "src", "three.ts"), "mixed\nevidence\n", "utf8");
	writeFileSync(join(repo, ".env"), "PUBLIC_FIXTURE=true\n", "utf8");
	symlinkSync("one.ts", join(repo, "src", "linked.ts"));
	writeFileSync(
		join(repo, "public-files.json"),
		JSON.stringify(["public-files.json", "src/one.ts", "src/two.ts", "src/three.ts", "src/linked.ts", ".env"]),
		"utf8",
	);
	oldCommit = commit("fixture one");
	writeFileSync(join(repo, "marker.txt"), "second commit\n", "utf8");
	currentCommit = commit("fixture two");
});

after(() => {
	if (repo) rmSync(repo, { recursive: true, force: true });
});

function request(overrides: Record<string, unknown> = {}) {
	return {
		mode: "select",
		task: "Find source relevant to cancellation cleanup",
		expected_commit: currentCommit,
		sources: [{ path: "src/one.ts", start_line: 2, end_line: 3 }],
		...overrides,
	};
}

async function withServerEnv<T>(server: ReturnType<typeof createFakeServer>, body: () => Promise<T>): Promise<T> {
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

function gitWrapper(name: string, body: string): string {
	const path = join(repo, ".git", name);
	writeFileSync(path, `#!/bin/sh\n${body}\n`, { encoding: "utf8", mode: 0o700 });
	return path;
}

function realGitPath(): string {
	return execFileSync("/usr/bin/which", ["git"], { encoding: "utf8" }).trim();
}

function budget(limit = 3) {
	let count = 0;
	return {
		state: {
			limit,
			used: () => count,
			reserve: () => { count += 1; },
		},
		used: () => count,
	};
}

describe("committed source collection", () => {
	it("reads exact ranges from current HEAD rather than the dirty working tree", async () => {
		writeFileSync(join(repo, "src", "one.ts"), "DIRTY WORKTREE\n", "utf8");
		try {
			const root = await validateSelectionRoot(repo, repo);
			const collected = await collectCommittedContext(root, validateSelectContextInput(request()));
			assert.equal(collected.commit, currentCommit);
			assert.equal(collected.sources[0]?.text, "beta\ngamma\n");
			assert.equal(collected.sources[0]?.excerptSHA256, createHash("sha256").update("beta\ngamma\n").digest("hex"));
			assert.equal(collected.sources[0]?.blobSHA256, createHash("sha256").update("alpha\nbeta\ngamma\ndelta\n").digest("hex"));
			assert.deepEqual(collected.sources[0]?.retrieval, { path: "src/one.ts", start_line: 2, end_line: 3 });
			assert.equal(collected.sources[0]?.id, "source_01");
		} finally {
			git("checkout", "--", "src/one.ts");
		}
	});

	it("uses expected_commit only as an equality guard", async () => {
		await assert.rejects(
			collectCommittedContext(repo, validateSelectContextInput(request({ expected_commit: oldCommit }))),
			/does not equal current HEAD/,
		);
		await assert.rejects(
			Promise.resolve().then(() => validateSelectContextInput(request({ expected_commit: "HEAD~1" }))),
			/full lowercase Git commit id/,
		);
	});

	it("refuses non-allowlisted, linked, sensitive, traversal, and out-of-range sources", async () => {
		for (const [source, pattern] of [
			[{ path: "marker.txt", start_line: 1, end_line: 1 }, /not in committed public-files/],
			[{ path: "src/linked.ts", start_line: 1, end_line: 1 }, /linked, or nonregular/],
			[{ path: ".env", start_line: 1, end_line: 1 }, /credential, configuration, or context store/],
			[{ path: "../src/one.ts", start_line: 1, end_line: 1 }, /invalid repository-relative path/],
			[{ path: "src/one.ts", start_line: 99, end_line: 99 }, /beyond the committed file/],
		] as const) {
			await assert.rejects(
				Promise.resolve().then(async () => {
					const validated = validateSelectContextInput(request({ sources: [source] }));
					return collectCommittedContext(repo, validated);
				}),
				pattern,
			);
		}
	});

	it("rejects credential-pattern source text as defense in depth", async () => {
		writeFileSync(join(repo, "src", "token.ts"), `const value = "sk-${"A".repeat(40)}";\n`, "utf8");
		const allowlist = JSON.parse(readFileSync(join(repo, "public-files.json"), "utf8")) as string[];
		allowlist.push("src/token.ts");
		writeFileSync(join(repo, "public-files.json"), JSON.stringify(allowlist), "utf8");
		currentCommit = commit("credential fixture");
		await assert.rejects(
			collectCommittedContext(repo, validateSelectContextInput(request({
				expected_commit: currentCommit,
				sources: [{ path: "src/token.ts", start_line: 1, end_line: 1 }],
			}))),
			/matches a credential pattern/,
		);
	});

	it("cancels and reaps a stalled root-validation Git child at session shutdown", async () => {
		const stalledGit = gitWrapper("stalled-git", "exec /bin/sleep 30");
		const collector = new ContextCollector({ gitBinary: stalledGit });
		const validation = collector.validateRoot(repo, repo);
		for (let attempt = 0; attempt < 100 && collector.activeChildCount() === 0; attempt++) {
			await new Promise((resolve) => setTimeout(resolve, 10));
		}
		assert.equal(collector.activeChildCount(), 1);
		assert.equal(await collector.shutdown(), 0);
		await assert.rejects(validation, /shutting down/);
		assert.equal(collector.activeChildCount(), 0);
	});

	it("size-checks blobs and rejects malformed UTF-8 or a byte-order mark before hashing text", async () => {
		writeFileSync(join(repo, "src", "invalid.txt"), Buffer.from([0xff, 0x0a]));
		writeFileSync(join(repo, "src", "bom.txt"), Buffer.from([0xef, 0xbb, 0xbf, 0x61, 0x0a]));
		writeFileSync(join(repo, "src", "huge.txt"), Buffer.alloc(262145, 0x61));
		const allowlist = JSON.parse(readFileSync(join(repo, "public-files.json"), "utf8")) as string[];
		allowlist.push("src/invalid.txt", "src/bom.txt", "src/huge.txt");
		writeFileSync(join(repo, "public-files.json"), JSON.stringify(allowlist), "utf8");
		currentCommit = commit("bounded blob fixtures");
		for (const [path, pattern] of [
			["src/invalid.txt", /not valid UTF-8/],
			["src/bom.txt", /byte-order mark/],
			["src/huge.txt", /exceeds the collector byte limit/],
		] as const) {
			await assert.rejects(
				collectCommittedContext(repo, validateSelectContextInput(request({
					expected_commit: currentCommit,
					sources: [{ path, start_line: 1, end_line: 1 }],
				}))),
				pattern,
			);
		}
	});
});

describe("select_context tool", () => {
	function threeSourceRequest(mode: "select" | "read" = "select") {
		return {
			mode,
			task: "Find relevant evidence",
			expected_commit: currentCommit,
			sources: [
				{ path: "src/one.ts", start_line: 1, end_line: 1 },
				{ path: "src/two.ts", start_line: 1, end_line: 2 },
				{ path: "src/three.ts", start_line: 1, end_line: 2 },
			],
		};
	}

	it("applies one cumulative deadline to slow successful Git commands in read mode", async () => {
		const slowGit = gitWrapper("slow-git", `/bin/sleep 0.08\nexec ${JSON.stringify(realGitPath())} "$@"`);
		const collector = new ContextCollector({ gitBinary: slowGit });
		const runner = new JevRunner();
		const calls = budget();
		const tool = createSelectContextTool(runner, repo, calls.state, collector, 180);
		await assert.rejects(
			tool.execute("slow-read", threeSourceRequest("read"), undefined, undefined, {} as never),
			/end-to-end deadline/,
		);
		assert.equal(calls.used(), 0);
		assert.equal(collector.activeChildCount(), 0);
		await collector.shutdown();
	});

	it("returns every requested original in read mode without a reservation or server spawn", async () => {
		const runner = new JevRunner();
		const calls = budget();
		const tool = createSelectContextTool(runner, repo, calls.state);
		const result = await tool.execute("read-1", threeSourceRequest("read"), undefined, undefined, {} as never);
		const content = result.content[0];
		assert.ok(content?.type === "text");
		const payload = JSON.parse(content.text) as { mode: string; status: string; sources: Array<{ disposition: string; text: string }> };
		assert.equal(payload.mode, "read");
		assert.equal(payload.status, "read");
		assert.equal(payload.sources.length, 3);
		assert.ok(payload.sources.every((source) => source.disposition === "read" && typeof source.text === "string"));
		assert.equal(calls.used(), 0);
		assert.equal(runner.activeChildCount(), 0);
	});

	it("calls select_evidence once and releases text only for keep and review", async () => {
		const server = createFakeServer("ok");
		const runner = new JevRunner();
		const calls = budget();
		const tool = createSelectContextTool(runner, repo, calls.state);
		const result = await withServerEnv(server, () =>
			tool.execute("select-1", threeSourceRequest(), undefined, undefined, {} as never),
		);
		const content = result.content[0];
		assert.ok(content?.type === "text");
		const payload = JSON.parse(content.text) as {
			status: string;
			sources: Array<{ disposition: string; text?: string; retrieval: { path: string }; excerpt_sha256: string }>;
		};
		assert.equal(payload.status, "review");
		assert.deepEqual(payload.sources.map((source) => source.disposition), ["keep", "drop", "review"]);
		assert.equal(payload.sources[0]?.text, "alpha\n");
		assert.equal(payload.sources[1]?.text, undefined);
		assert.equal(payload.sources[2]?.text, "mixed\nevidence\n");
		assert.ok(payload.sources.every((source) => source.retrieval.path && /^[0-9a-f]{64}$/.test(source.excerpt_sha256)));
		assert.equal(calls.used(), 1);

		const start = server.readLog().find((entry) => entry.event === "start");
		assert.deepEqual(start?.argv, ["--enable-selection"]);
		const call = server.readLog().find((entry) => entry.event === "call-args");
		assert.equal(call?.name, "select_evidence");
		const serialized = JSON.stringify(call?.arguments);
		assert.ok(!serialized.includes("src/one.ts"));
		assert.ok(!serialized.includes(repo));
		assert.deepEqual(Object.keys(call?.arguments as object).sort(), ["items", "task"]);
		await runner.shutdown();
	});

	it("requires a persisted reservation before spawning the paid selection server", async () => {
		const server = createFakeServer("ok");
		const runner = new JevRunner();
		let appendAttempts = 0;
		const calls = createPersistentSelectionBudget(
			{ getSessionFile: () => undefined, getEntries: () => [] },
			() => { appendAttempts += 1; },
			3,
		);
		const tool = createSelectContextTool(runner, repo, calls);
		await assert.rejects(
			withServerEnv(server, () => tool.execute("unpersisted", threeSourceRequest(), undefined, undefined, {} as never)),
			/requires a persisted Pi session/,
		);
		assert.equal(appendAttempts, 0);
		const sessionFile = join(repo, ".git", "session.jsonl");
		writeFileSync(sessionFile, "fixture\n", "utf8");
		const failedWrite = createPersistentSelectionBudget(
			{ getSessionFile: () => sessionFile, getEntries: () => [] },
			() => { throw new Error("disk failure"); },
			3,
		);
		await assert.rejects(
			withServerEnv(server, () => createSelectContextTool(runner, repo, failedWrite).execute(
				"failed-persistence",
				threeSourceRequest(),
				undefined,
				undefined,
				{} as never,
			)),
			/reservation could not be persisted/,
		);
		const memoryEntries: Array<{ type: string; customType: string }> = [];
		const memoryOnlyAppend = createPersistentSelectionBudget(
			{ getSessionFile: () => sessionFile, getEntries: () => memoryEntries },
			() => { memoryEntries.push({ type: "custom", customType: SELECTION_RESERVATION_ENTRY }); },
			3,
		);
		await assert.rejects(
			withServerEnv(server, () => createSelectContextTool(runner, repo, memoryOnlyAppend).execute(
				"memory-only-persistence",
				threeSourceRequest(),
				undefined,
				undefined,
				{} as never,
			)),
			/reservation was not persisted/,
		);
		assert.equal(server.readLog().filter((entry) => entry.event === "start").length, 0);
	});

	it("rejects malformed selection envelopes, hashes, and metadata without releasing source", async () => {
		for (const mode of [
			"selection-bad-hash",
			"selection-bad-status",
			"selection-extra-field",
			"selection-iserror-string",
			"selection-missing-content",
			"selection-mismatched-copies",
		] as const) {
			const server = createFakeServer(mode);
			const runner = new JevRunner();
			const calls = budget();
			const tool = createSelectContextTool(runner, repo, calls.state);
			await assert.rejects(
				withServerEnv(server, () => tool.execute("bad", threeSourceRequest(), undefined, undefined, {} as never)),
				(error: unknown) => error instanceof Error && /jev select_context failed/.test(error.message) && !error.message.includes("alpha"),
			);
			assert.equal(calls.used(), 1);
			await runner.shutdown();
		}
	});

	it("enforces a downward budget before a fourth spawn while read remains free", async () => {
		const server = createFakeServer("ok");
		const runner = new JevRunner();
		const calls = budget(3);
		const tool = createSelectContextTool(runner, repo, calls.state);
		for (let index = 0; index < 3; index++) {
			await withServerEnv(server, () => tool.execute(`paid-${index}`, threeSourceRequest(), undefined, undefined, {} as never));
		}
		await assert.rejects(
			withServerEnv(server, () => tool.execute("paid-4", threeSourceRequest(), undefined, undefined, {} as never)),
			/3 paid-call reservation/,
		);
		await tool.execute("free-read", threeSourceRequest("read"), undefined, undefined, {} as never);
		assert.equal(calls.used(), 3);
		assert.equal(server.readLog().filter((entry) => entry.event === "start").length, 3);
		await runner.shutdown();
	});

	it("uses a conservative numeric expansion bound before a paid call", async () => {
		const context = await collectCommittedContext(repo, validateSelectContextInput(threeSourceRequest()));
		const bound = conservativeSelectedResultBytes(context);
		assert.ok(bound <= 32768);
		assert.doesNotThrow(() => assertSelectedResultFits(context, bound));
		assert.throws(() => assertSelectedResultFits(context, bound - 1), /output limit/);
	});

	it("renders compact progress, success, and fixed failure output", () => {
		assert.equal(
			buildContextResultText(
				{ content: [], details: { kind: "jev-context-progress", phase: "collecting" } },
				{ expanded: false, isPartial: true, isError: false },
				PLAIN_THEME,
			),
			"jev context collecting",
		);
		const rendered = buildContextResultText(
			{
				content: [{ type: "text", text: "{}" }],
				details: {
					kind: "jev-context-result",
					mode: "select",
					status: "selected",
					source_commit: currentCommit,
					sources: [{ retrieval: { path: "src/one.ts", start_line: 1, end_line: 1 }, disposition: "keep" }],
				},
			},
			{ expanded: true, isPartial: false, isError: false },
			PLAIN_THEME,
		);
		assert.match(rendered, /context select · selected · 1 source/);
		assert.match(rendered, /src\/one\.ts:1-1 keep/);
		const malformedLines = buildContextResultText(
			{
				content: [{ type: "text", text: "{}" }],
				details: {
					kind: "jev-context-result",
					mode: "read",
					status: "read",
					sources: [{
						retrieval: { path: "src/one.ts", start_line: `1${SECRET_SENTINEL}\n`, end_line: -1 },
						disposition: "read",
					}],
				},
			},
			{ expanded: true, isPartial: false, isError: false },
			PLAIN_THEME,
		);
		assert.match(malformedLines, /src\/one\.ts:\?-\? read/);
		assert.ok(!malformedLines.includes(SECRET_SENTINEL));
		const failure = buildContextResultText(
			{ content: [{ type: "text", text: `failed ${SECRET_SENTINEL}\u001b[31m` }] },
			{ expanded: false, isPartial: false, isError: true },
			PLAIN_THEME,
		);
		assert.match(failure, /^✗ jev select_context failed/);
		assert.ok(!failure.includes("\u001b"));
	});
});

assert.equal(SELECT_CONTEXT_TOOL_NAME, "select_context");
