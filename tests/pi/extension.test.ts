/**
 * Runtime tests against the installed Pi SDK.
 *
 * The adapter is loaded the way Pi loads an extension, in an isolated agent directory
 * and working directory, and the tool it registers is then driven through Pi's own
 * tool-call path: Pi's argument validator, Pi's execution wrapper, and Pi's
 * conversion of a thrown error into an isError result. No provider is contacted and
 * no global Pi configuration is read: the credential store, model store, settings,
 * and session all point at temporary locations, and PI_OFFLINE disables model network
 * access.
 */

import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, realpathSync, rmSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { after, before, describe, it } from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";
import {
	createAgentSession,
	createAgentSessionFromServices,
	createAgentSessionRuntime,
	createAgentSessionServices,
	DefaultResourceLoader,
	ModelRuntime,
	SessionManager,
	SettingsManager,
} from "@earendil-works/pi-coding-agent";
import {
	buildResultText,
	createPersistentSelectionBudget,
	PLAIN_THEME,
	SELECTION_RESERVATION_ENTRY,
	shutdownOwnedChildren,
	UNREAPED_SHUTDOWN_MESSAGE,
} from "../../adapters/pi/jev.ts";
import { createFakeServer, isAlive, SECRET_SENTINEL } from "./helpers/fake-server.ts";
import { officialExampleRequest, sampleRequest } from "./helpers/fixtures.ts";

const here = dirname(fileURLToPath(import.meta.url));
const adapterPath = resolve(here, "../../adapters/pi/jev.ts");
const harnessDir = resolve(
	here,
	"../../node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-agent-core/dist/harness",
);

let agentDir = "";
let workDir = "";
const previousOffline = process.env.PI_OFFLINE;
const BASELINE_VISIBLE_INTERFACE_BYTES = 3787;
const MAX_COMPACT_INTERFACE_BYTES = Math.floor(BASELINE_VISIBLE_INTERFACE_BYTES * 0.85);

before(() => {
	process.env.PI_OFFLINE = "1";
	const root = resolve("temp");
	mkdirSync(root, { recursive: true });
	agentDir = mkdtempSync(join(root, "pi-agent-"));
	workDir = mkdtempSync(join(root, "pi-cwd-"));
});

after(() => {
	if (previousOffline === undefined) delete process.env.PI_OFFLINE;
	else process.env.PI_OFFLINE = previousOffline;
	for (const dir of [agentDir, workDir]) {
		if (dir) rmSync(dir, { recursive: true, force: true });
	}
});

async function loadAdapterSession(options: {
	cwd?: string;
	flags?: Record<string, string>;
	sessionManager?: SessionManager;
} = {}) {
	const cwd = options.cwd ?? workDir;
	const settingsManager = SettingsManager.inMemory({});
	const modelRuntime = await ModelRuntime.create({
		authPath: join(agentDir, "auth.json"),
		modelsPath: join(agentDir, "models.json"),
		modelsStorePath: join(agentDir, "models-store.json"),
	});
	const loader = new DefaultResourceLoader({
		cwd,
		agentDir,
		settingsManager,
		additionalExtensionPaths: [adapterPath],
		extensionsOverride: (base) => {
			for (const [name, value] of Object.entries(options.flags ?? {})) base.runtime.flagValues.set(name, value);
			return base;
		},
	});
	await loader.reload();

	return createAgentSession({
		cwd,
		agentDir,
		resourceLoader: loader,
		settingsManager,
		modelRuntime,
		sessionManager: options.sessionManager ?? SessionManager.inMemory(cwd),
		noTools: "builtin",
	});
}

function flushPersistedSession(manager: SessionManager): void {
	manager.appendMessage({
		role: "assistant",
		content: [{ type: "text", text: "fixture" }],
		api: "fixture",
		provider: "fixture",
		model: "fixture",
		usage: {
			input: 0,
			output: 0,
			cacheRead: 0,
			cacheWrite: 0,
			totalTokens: 0,
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
		},
		stopReason: "stop",
		timestamp: Date.now(),
	});
}

async function createAdapterRuntimeHarness(cwd: string, sessionDir: string) {
	const flags = new Map<string, boolean | string>([
		["jev-select-root", cwd],
		["jev-select-budget", "1"],
	]);
	const createRuntime = async (options: {
		cwd: string;
		agentDir: string;
		sessionManager: SessionManager;
		sessionStartEvent?: any;
	}) => {
		const settingsManager = SettingsManager.inMemory({});
		const modelRuntime = await ModelRuntime.create({
			authPath: join(options.agentDir, "runtime-auth.json"),
			modelsPath: join(options.agentDir, "runtime-models.json"),
			modelsStorePath: join(options.agentDir, "runtime-models-store.json"),
		});
		const services = await createAgentSessionServices({
			cwd: options.cwd,
			agentDir: options.agentDir,
			settingsManager,
			modelRuntime,
			extensionFlagValues: flags,
			resourceLoaderOptions: {
				additionalExtensionPaths: [adapterPath],
				noContextFiles: true,
				noSkills: true,
				noPromptTemplates: true,
				noThemes: true,
			},
		});
		return {
			...(await createAgentSessionFromServices({
				services,
				sessionManager: options.sessionManager,
				sessionStartEvent: options.sessionStartEvent,
				noTools: "builtin",
			})),
			services,
			diagnostics: services.diagnostics,
		};
	};
	const sessionManager = SessionManager.create(cwd, sessionDir);
	flushPersistedSession(sessionManager);
	sessionManager.appendCustomEntry("lifecycle-fixture", {});
	const runtime = await createAgentSessionRuntime(createRuntime, {
		cwd,
		agentDir,
		sessionManager,
		sessionStartEvent: { type: "session_start", reason: "startup" },
	});
	await runtime.session.bindExtensions({});
	runtime.setRebindSession((session) => session.bindExtensions({}));
	return runtime;
}

/** Point the Pi process environment at a fake server, the way a staged test would. */
async function withServerEnv<T>(home: string, binPath: string, body: () => Promise<T>): Promise<T> {
	const previous = { home: process.env.HOME, bin: process.env.JEV_MCP_BIN };
	process.env.HOME = home;
	process.env.JEV_MCP_BIN = binPath;
	try {
		return await body();
	} finally {
		if (previous.home === undefined) delete process.env.HOME;
		else process.env.HOME = previous.home;
		if (previous.bin === undefined) delete process.env.JEV_MCP_BIN;
		else process.env.JEV_MCP_BIN = previous.bin;
	}
}

describe("loading the adapter with the Pi SDK", () => {
	it("registers exactly one tool, named evaluate, with a provider-friendly schema", async () => {
		const { session, extensionsResult } = await loadAdapterSession();
		try {
			assert.deepEqual(extensionsResult.errors, [], "the adapter must load without errors");

			const tools = session.agent.state.tools;
			assert.deepEqual(
				tools.map((tool) => tool.name),
				["evaluate"],
			);

			const tool = tools[0] as { description: string; parameters: Record<string, unknown> };
			assert.match(tool.description, /bounded typed judgment about evidence you already have/i);
			assert.match(tool.description, /do not use it to authorize an action/i);

			const schema = JSON.stringify(tool.parameters);
			assert.ok(!schema.includes("patternProperties"));
			const properties = (tool.parameters as { properties: Record<string, unknown> }).properties;
			assert.deepEqual(Object.keys(properties).sort(), ["questions", "state"]);
		} finally {
			session.dispose();
		}
	});

	it("exposes select_context only for an explicitly validated project root and bounded budget", async () => {
		const projectRoot = realpathSync(resolve(here, "../.."));
		const { session, extensionsResult } = await loadAdapterSession({
			cwd: projectRoot,
			flags: { "jev-select-root": projectRoot, "jev-select-budget": "1" },
		});
		try {
			assert.deepEqual(extensionsResult.errors, []);
			const extension = extensionsResult.extensions[0];
			const startHandler = extension?.handlers.get("session_start")?.[0];
			assert.ok(startHandler);
			await startHandler(
				{ type: "session_start", reason: "startup" },
				{
					cwd: projectRoot,
					sessionManager: { getEntries: () => [] },
					ui: { notify: () => {} },
				},
			);
			assert.deepEqual([...extension?.tools.keys() ?? []], ["evaluate", "select_context"]);
			assert.deepEqual(session.agent.state.tools.map((tool) => tool.name), ["evaluate", "select_context"]);
			const tool = extension?.tools.get("select_context")?.definition;
			assert.ok(tool);
			assert.match(tool.description, /limited to 1 paid-call reservation/);
			assert.match(tool.description, /Mode read returns every requested original range without an API call/);
			assert.match(tool.description, /transmits the task and source text/);
			assert.match(tool.description, /retrieval paths and line metadata are omitted/);
			assert.match(tool.description, /16384 aggregate source bytes/);
			assert.match(tool.description, /32768-byte output/);
			const schemaText = JSON.stringify(tool.parameters);
			for (const limit of [240, 200, 4096, 8192, 16384, 262144, 32768]) assert.ok(schemaText.includes(String(limit)));
			assert.deepEqual(Object.keys((tool.parameters as { properties: object }).properties).sort(), [
				"expected_commit",
				"mode",
				"sources",
				"task",
			]);
		} finally {
			session.dispose();
		}
	});

	it("reconstructs paid reservations from all session entries before exposing the tool", async () => {
		const projectRoot = realpathSync(resolve(here, "../.."));
		const { session, extensionsResult } = await loadAdapterSession({
			cwd: projectRoot,
			flags: { "jev-select-root": projectRoot, "jev-select-budget": "3" },
		});
		try {
			const extension = extensionsResult.extensions[0];
			const startHandler = extension?.handlers.get("session_start")?.[0];
			assert.ok(startHandler);
			await startHandler(
				{ type: "session_start", reason: "reload" },
				{
					cwd: projectRoot,
					sessionManager: {
						getEntries: () => Array.from({ length: 3 }, (_, index) => ({
							type: "custom",
							customType: SELECTION_RESERVATION_ENTRY,
							data: { version: 1, index },
						})),
					},
					ui: { notify: () => {} },
				},
			);
			const tool = extension?.tools.get("select_context")?.definition;
			assert.ok(tool);
			await assert.rejects(
				tool.execute("budget-reload", {
					mode: "select",
					task: "Find adapter provenance",
					sources: [{ path: "adapters/pi/jev.ts", start_line: 1, end_line: 1 }],
				} as never, undefined, undefined, {} as never),
				/3 paid-call reservation/,
			);
		} finally {
			session.dispose();
		}
	});

	it("persists reservations across reload and inactive session branches", () => {
		const projectRoot = realpathSync(resolve(here, "../.."));
		const sessionDir = mkdtempSync(join(agentDir, "reservation-session-"));
		try {
			const manager = SessionManager.create(projectRoot, sessionDir);
			flushPersistedSession(manager);
			const firstBudget = createPersistentSelectionBudget(
				manager,
				() => { manager.appendCustomEntry(SELECTION_RESERVATION_ENTRY, { version: 1 }); },
				3,
			);
			firstBudget.reserve();
			const firstReservation = manager.getEntries().at(-1);
			assert.ok(firstReservation?.id);
			manager.appendCustomEntry("fixture-marker", {});
			manager.branch(firstReservation.id);
			firstBudget.reserve();
			assert.equal(firstBudget.used(), 2);

			const sessionFile = manager.getSessionFile();
			assert.ok(sessionFile);
			const reopened = SessionManager.open(sessionFile, sessionDir);
			const reloadedBudget = createPersistentSelectionBudget(
				reopened,
				() => { reopened.appendCustomEntry(SELECTION_RESERVATION_ENTRY, { version: 1 }); },
				3,
			);
			assert.equal(reloadedBudget.used(), 2);
			assert.equal(reopened.getBranch().filter((entry) => entry.type === "custom" && entry.customType === "fixture-marker").length, 0);
			assert.equal(reopened.getEntries().filter((entry) => entry.type === "custom" && entry.customType === "fixture-marker").length, 1);
		} finally {
			rmSync(sessionDir, { recursive: true, force: true });
		}
	});

	it("keeps the complete model-facing contract compact without duplicating schema formats", async () => {
		const { session, extensionsResult } = await loadAdapterSession();
		try {
			const definition = extensionsResult.extensions[0]?.tools.get("evaluate")?.definition;
			assert.ok(definition);
			const visibleInterface = {
				description: definition.description,
				parameters: definition.parameters,
				promptSnippet: definition.promptSnippet,
				promptGuidelines: definition.promptGuidelines,
			};
			const serialized = JSON.stringify(visibleInterface);
			assert.doesNotMatch(definition.description, /criteria is|required (?:map|array|object)/i);
			assert.ok(
				Buffer.byteLength(serialized, "utf8") <= MAX_COMPACT_INTERFACE_BYTES,
				`model-visible interface exceeds ${MAX_COMPACT_INTERFACE_BYTES} bytes`,
			);

			const text = serialized.toLowerCase();
			for (const phrase of [
				"bounded typed judgment about evidence you already have",
				"bounded judgment whose uncertainty matters",
				"state the condition to test rather than the conclusion you expect",
				"noul: yes-probability",
				"selected criteria option with distribution and confidence",
				"probability-weighted position with distribution and confidence",
				"1-255 options to a string description or null",
				"2-10 string descriptions",
				"indices are 0 to n-1",
				"map of 1-16 questions",
				"classification, routing, triage, rubric scoring, and extraction",
				"answer space can be written down in advance",
				"batch independent questions",
				"raw evidence, uncertainty, and counterevidence",
				"preserve claims already present in source evidence",
				"not your own verdict",
				"a verdict you add biases the answer",
				"credentials or secrets",
				"paid external",
				"prose, code, or open-ended text",
				"arithmetic, counting, or date calculations",
				"deterministic verification",
				"type checks",
				"authorize an action",
				"security or safety guarantee",
				"high confidence means a concentrated probability distribution",
				"probability with the evidence behind it",
				"not as a settled fact",
				"say so when a decision rests on a judgment",
			]) {
				assert.ok(text.includes(phrase), `model-visible interface is missing ${JSON.stringify(phrase)}`);
			}
			assert.deepEqual(definition.promptGuidelines, [
				"Use evaluate only for a bounded judgment whose uncertainty matters, and state the condition to test rather than the conclusion you expect.",
				"Never use evaluate in place of tests, type checks, builds, or other deterministic verification, and never treat its output as authorization or as a security or safety guarantee.",
				"Report a evaluate answer as a probability with the evidence behind it, not as a settled fact, and say so when a decision rests on a judgment.",
			]);
		} finally {
			session.dispose();
		}
	});

	it("keeps the context collector opt-in and registers no command, shortcut, renderer, or provider", async () => {
		const { session, extensionsResult } = await loadAdapterSession();
		try {
			assert.equal(extensionsResult.extensions.length, 1);
			const extension = extensionsResult.extensions[0];
			assert.ok(extension);
			assert.deepEqual([...extension.tools.keys()], ["evaluate"]);
			assert.deepEqual([...extension.commands.keys()], []);
			assert.deepEqual([...extension.shortcuts.keys()], []);
			assert.deepEqual([...extension.flags.keys()].sort(), ["jev-select-budget", "jev-select-root"]);
			assert.deepEqual([...extension.messageRenderers.keys()], []);
			assert.deepEqual([...extension.handlers.keys()], ["session_start", "session_shutdown"]);
			assert.deepEqual(extensionsResult.runtime.pendingProviderRegistrations, []);
			assert.deepEqual(extensionsResult.runtime.pendingNativeProviderRegistrations, []);
		} finally {
			session.dispose();
		}
	});

	it("runs automatic startup, new-session, and resume lifecycles with both tools available", async () => {
		const projectRoot = realpathSync(resolve(here, "../.."));
		const sessionDir = mkdtempSync(join(agentDir, "runtime-session-"));
		const server = createFakeServer("ok");
		const runtime = await createAdapterRuntimeHarness(projectRoot, sessionDir);
		try {
			const initialFile = runtime.session.sessionFile;
			assert.ok(initialFile);
			for (const stage of ["startup", "new", "resume"] as const) {
				const extensions = runtime.services.resourceLoader.getExtensions();
				const extension = extensions.extensions[0];
				assert.deepEqual([...extension?.tools.keys() ?? []], ["evaluate", "select_context"], stage);
				assert.deepEqual(runtime.session.agent.state.tools.map((tool) => tool.name), ["evaluate", "select_context"], stage);
				const select = extension?.tools.get("select_context")?.definition;
				const evaluate = extension?.tools.get("evaluate")?.definition;
				assert.ok(select && evaluate);
				const readResult = await select.execute(`read-${stage}`, {
					mode: "read",
					task: "Read the project heading",
					sources: [{ path: "README.md", start_line: 1, end_line: 1 }],
				} as never, undefined, undefined, {} as never);
				assert.match((readResult.content[0] as { text: string }).text, /# Jev MCP/);
				if (stage === "startup") {
					const selected = await withServerEnv(server.home, server.binPath, () => select.execute(
						"select-startup",
						{
							mode: "select",
							task: "Select the project heading",
							sources: [{ path: "README.md", start_line: 1, end_line: 1 }],
						} as never,
						undefined,
						undefined,
						{} as never,
					));
					assert.match((selected.content[0] as { text: string }).text, /"disposition":"keep"/);
					const persisted = SessionManager.open(runtime.session.sessionFile as string, sessionDir);
					assert.equal(persisted.getEntries().filter(
						(entry) => entry.type === "custom" && entry.customType === SELECTION_RESERVATION_ENTRY,
					).length, 1);
				}
				const evaluation = await withServerEnv(server.home, server.binPath, () =>
					evaluate.execute(
						`evaluate-${stage}`,
						officialExampleRequest() as never,
						undefined,
						undefined,
						{} as never,
					),
				);
				assert.match((evaluation.content[0] as { text: string }).text, /"model":"jev-1.13.0"/);
				if (stage === "startup") {
					assert.deepEqual(await runtime.newSession(), { cancelled: false });
				} else if (stage === "new") {
					assert.deepEqual(await runtime.switchSession(initialFile), { cancelled: false });
				}
			}
		} finally {
			await runtime.dispose();
			rmSync(sessionDir, { recursive: true, force: true });
		}
	});

	it("checks both bounded shutdown counts and reports one fixed lifecycle error", async () => {
		const successfulOwner = { shutdown: async () => 0 };
		await assert.doesNotReject(shutdownOwnedChildren(successfulOwner, successfulOwner));

		for (const [mcpCount, gitCount] of [[1, 0], [0, 1], [1, 1]] as const) {
			const calls: string[] = [];
			const shutdown = shutdownOwnedChildren(
				{ shutdown: async () => { calls.push("mcp"); return mcpCount; } },
				{ shutdown: async () => { calls.push("git"); return gitCount; } },
			);
			assert.deepEqual(calls, ["mcp", "git"], "both bounded shutdown sweeps must start before either result is checked");
			await assert.rejects(
				shutdown,
				(error: unknown) => error instanceof Error && error.message === UNREAPED_SHUTDOWN_MESSAGE,
			);
		}
	});

	it("runs the registered tool against a fake server and reaps it on session_shutdown", async () => {
		const server = createFakeServer("ok");
		const { session, extensionsResult } = await loadAdapterSession();
		const extension = extensionsResult.extensions[0];
		assert.ok(extension);
		const tool = extension.tools.get("evaluate")?.definition;
		assert.ok(tool);

		try {
			const result = await withServerEnv(server.home, server.binPath, () =>
				tool.execute("call-sdk-1", officialExampleRequest() as never, undefined, undefined, {} as never),
			);
			const content = result.content[0];
			assert.ok(content && content.type === "text");
			const payload = JSON.parse(content.text) as { model: string; answers: Record<string, unknown> };
			assert.equal(payload.model, "jev-1.13.0");
			assert.deepEqual(Object.keys(payload.answers), ["refund_request", "department", "urgency"]);

			// The shutdown handler is what Pi calls on quit, reload, and session switch.
			const handlers = extension.handlers.get("session_shutdown") ?? [];
			assert.equal(handlers.length, 1);
			await handlers[0]?.({ type: "session_shutdown", reason: "quit" }, {});
			await handlers[0]?.({ type: "session_shutdown", reason: "quit" }, {});

			assert.equal(isAlive(server.pid()), false, "the evaluation child must not outlive the session");
		} finally {
			session.dispose();
		}
	});
});

/**
 * Pi's own tool-call path, imported from the agent core that ships with the installed
 * SDK. `prepareToolCall` runs the real argument validator against the adapter's
 * schema, and `executeToolCall` performs the throw-to-isError conversion that the
 * renderer has to cope with.
 */
describe("Pi tool-call path", () => {
	const available = existsSync(join(harnessDir, "execution", "tools.js"));

	async function loadHarness() {
		const tools = (await import(pathToFileURL(join(harnessDir, "execution", "tools.js")).href)) as {
			prepareToolCall: (call: unknown, tools: unknown[]) => any;
			executeToolCall: (
				call: unknown,
				gate: unknown,
				onUpdate: unknown,
				toolContext: unknown,
				invocation: unknown,
				context: unknown,
			) => Promise<{ result: { content: unknown[]; details?: unknown }; isError: boolean }>;
		};
		const gates = (await import(pathToFileURL(join(harnessDir, "execution", "effect-gate.js")).href)) as {
			createGate: () => { gate: unknown };
		};
		const context = (await import(pathToFileURL(join(harnessDir, "context.js")).href)) as {
			BACKGROUND_CONTEXT: unknown;
		};
		return { tools, gates, context };
	}

	/** Adapt the extension tool to the harness tool signature. */
	function harnessTool(definition: any) {
		return {
			...definition,
			execute: (
				toolCallId: string,
				args: unknown,
				onUpdate: unknown,
				_toolContext: unknown,
				_invocation: unknown,
				ctx: { abortSignal?: AbortSignal },
			) => definition.execute(toolCallId, args, ctx?.abortSignal, onUpdate, {}),
		};
	}

	it(
		"accepts the official example through Pi's argument validator",
		{ skip: available ? false : "the installed Pi SDK does not expose its harness modules" },
		async () => {
			const { tools } = await loadHarness();
			const { session, extensionsResult } = await loadAdapterSession();
			try {
				const definition = extensionsResult.extensions[0]?.tools.get("evaluate")?.definition;
				assert.ok(definition);
				const prepared = tools.prepareToolCall(
					{ id: "call-1", name: "evaluate", arguments: officialExampleRequest() },
					[harnessTool(definition)],
				);
				assert.notEqual(prepared.kind, "immediate", `Pi rejected the official example: ${JSON.stringify(prepared.result)}`);
				assert.deepEqual(Object.keys(prepared.args.questions), ["refund_request", "department", "urgency"]);
			} finally {
				session.dispose();
			}
		},
	);

	it(
		"turns a thrown tool error into an isError result with no details, which the renderer survives",
		{ skip: available ? false : "the installed Pi SDK does not expose its harness modules" },
		async () => {
			const { tools, gates, context } = await loadHarness();
			const server = createFakeServer("tool-error-secret");
			const { session, extensionsResult } = await loadAdapterSession();
			try {
				const definition = extensionsResult.extensions[0]?.tools.get("evaluate")?.definition;
				assert.ok(definition);
				const prepared = tools.prepareToolCall(
					{ id: "call-2", name: "evaluate", arguments: sampleRequest() },
					[harnessTool(definition)],
				);
				assert.notEqual(prepared.kind, "immediate");

				const { gate } = gates.createGate();
				const executed = await withServerEnv(server.home, server.binPath, () =>
					tools.executeToolCall(prepared, gate, () => {}, undefined, {}, context.BACKGROUND_CONTEXT),
				);

				assert.equal(executed.isError, true, "a thrown tool error must become an isError result");
				assert.equal(executed.result.details, undefined, "Pi does not attach details to an error result");
				const text = (executed.result.content[0] as { text: string }).text;
				assert.match(text, /^jev evaluate failed after /);
				assert.ok(!text.includes(SECRET_SENTINEL));

				// This is exactly the shape the TUI renderer receives for a failed call.
				const rendered = buildResultText(
					executed.result,
					{ expanded: false, isPartial: false, isError: executed.isError },
					PLAIN_THEME,
				);
				assert.match(rendered, /^✗ jev evaluate failed\n/);
				assert.ok(!rendered.includes(SECRET_SENTINEL));
			} finally {
				session.dispose();
			}
		},
	);

	it(
		"reports a schema violation through Pi's validator rather than executing",
		{ skip: available ? false : "the installed Pi SDK does not expose its harness modules" },
		async () => {
			const { tools } = await loadHarness();
			const { session, extensionsResult } = await loadAdapterSession();
			try {
				const definition = extensionsResult.extensions[0]?.tools.get("evaluate")?.definition;
				assert.ok(definition);
				const prepared = tools.prepareToolCall(
					{
						id: "call-3",
						name: "evaluate",
						arguments: { state: "x", questions: { q: { type: "bogus", instructions: "y" } } },
					},
					[harnessTool(definition)],
				);
				assert.equal(prepared.kind, "immediate");
				assert.equal(prepared.isError, true);
			} finally {
				session.dispose();
			}
		},
	);

	it(
		"catches what Pi's validator repairs instead of rejecting",
		{ skip: available ? false : "the installed Pi SDK does not expose its harness modules" },
		async () => {
			// Pi runs Value.Convert before checking, so a number in a string-or-object
			// field is coerced to a string and an empty question map passes the schema.
			// This is exactly why the adapter validates again before spawning anything.
			const { tools, gates, context } = await loadHarness();
			const { session, extensionsResult } = await loadAdapterSession();
			try {
				const definition = extensionsResult.extensions[0]?.tools.get("evaluate")?.definition;
				assert.ok(definition);
				const prepared = tools.prepareToolCall(
					{ id: "call-4", name: "evaluate", arguments: { state: 5, questions: {} } },
					[harnessTool(definition)],
				);
				assert.notEqual(prepared.kind, "immediate", "Pi's validator accepts the converted arguments");

				const { gate } = gates.createGate();
				const executed = await tools.executeToolCall(
					prepared,
					gate,
					() => {},
					undefined,
					{},
					context.BACKGROUND_CONTEXT,
				);
				assert.equal(executed.isError, true);
				assert.match(
					(executed.result.content[0] as { text: string }).text,
					/rejected the request: at least one question is required/,
				);
			} finally {
				session.dispose();
			}
		},
	);
});
