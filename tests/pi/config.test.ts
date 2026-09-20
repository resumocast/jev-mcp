import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
	buildChildEnv,
	DEFAULT_BIN_RELATIVE_PATH,
	encodeFrame,
	FrameReader,
	isUnsafeCodePoint,
	JevError,
	MAX_FRAME_BYTES,
	resolveServerConfig,
	safePathLabel,
	sanitizeOneLine,
} from "../../adapters/pi/jev.ts";

describe("resolveServerConfig", () => {
	it("defaults to the documented absolute path under HOME", () => {
		const config = resolveServerConfig({}, "/home/tester");
		assert.equal(config.binPath, `/home/tester/${DEFAULT_BIN_RELATIVE_PATH}`);
		assert.deepEqual(config.args, []);
		assert.equal(config.binSource, "default");
		assert.equal(config.keyFile, undefined);
	});

	it("accepts an absolute JEV_MCP_BIN override", () => {
		const config = resolveServerConfig({ JEV_MCP_BIN: "/opt/staging/jev-mcp" }, "/home/tester");
		assert.equal(config.binPath, "/opt/staging/jev-mcp");
		assert.equal(config.binSource, "env");
	});

	it("passes a key file as a path-only argument", () => {
		const config = resolveServerConfig({ JEV_MCP_KEY_FILE: "/etc/keys/jev.key" }, "/home/tester");
		assert.deepEqual(config.args, ["--key-file", "/etc/keys/jev.key"]);
		assert.equal(config.keyFile, "/etc/keys/jev.key");
	});

	it("never accepts a key through the environment", () => {
		const config = resolveServerConfig(
			{ TYPESAFE_API_KEY: "secret", JEV_MCP_KEY: "secret", JEV_MCP_KEY_FILE: "/etc/keys/jev.key" },
			"/home/tester",
		);
		assert.deepEqual(config.args, ["--key-file", "/etc/keys/jev.key"]);
		assert.ok(!JSON.stringify(config).includes("secret"));
	});

	it("rejects relative, traversing, and malformed paths", () => {
		for (const value of ["relative/jev-mcp", "~/jev-mcp", "/opt/../etc/jev-mcp", "/opt/jev\nmcp", "/opt/jev\0mcp"]) {
			assert.throws(
				() => resolveServerConfig({ JEV_MCP_BIN: value }, "/home/tester"),
				(error: unknown) => error instanceof JevError && error.code === "config",
				`expected rejection for ${JSON.stringify(value)}`,
			);
		}
	});

	it("treats an empty override as unset", () => {
		const config = resolveServerConfig({ JEV_MCP_BIN: "   ", JEV_MCP_KEY_FILE: "" }, "/home/tester");
		assert.equal(config.binPath, `/home/tester/${DEFAULT_BIN_RELATIVE_PATH}`);
		assert.deepEqual(config.args, []);
	});

	it("rejects a relative key file path", () => {
		assert.throws(
			() => resolveServerConfig({ JEV_MCP_KEY_FILE: "keys/jev.key" }, "/home/tester"),
			(error: unknown) => error instanceof JevError && /absolute/.test(error.message),
		);
	});
});

describe("buildChildEnv", () => {
	it("forwards only HOME, a fixed PATH, and LANG", () => {
		const childEnv = buildChildEnv(
			{
				HOME: "/home/tester",
				PATH: "/usr/local/bin:/usr/bin",
				LANG: "pt_BR.UTF-8",
				ANTHROPIC_API_KEY: "sk-secret",
				TYPESAFE_API_KEY: "ts-secret",
				HTTPS_PROXY: "http://proxy.invalid:8080",
				http_proxy: "http://proxy.invalid:8080",
				NODE_OPTIONS: "--inspect",
				PI_SESSION_ID: "abc",
			},
			"/fallback",
		);
		assert.deepEqual(childEnv, { HOME: "/home/tester", PATH: "/usr/bin:/bin", LANG: "pt_BR.UTF-8" });
	});

	it("falls back to the process home and drops a malformed LANG", () => {
		const childEnv = buildChildEnv({ HOME: "not-absolute", LANG: "en_US.UTF-8; rm -rf /" }, "/fallback");
		assert.deepEqual(childEnv, { HOME: "/fallback", PATH: "/usr/bin:/bin" });
	});
});

describe("FrameReader", () => {
	it("splits LF-delimited frames and skips blank lines", () => {
		const reader = new FrameReader();
		assert.deepEqual(reader.push('{"a":1}\n\n{"b":'), ['{"a":1}']);
		assert.deepEqual(reader.push('2}\n'), ['{"b":2}']);
		assert.equal(reader.pendingBytes(), 0);
	});

	it("keeps partial frames buffered across chunks", () => {
		const reader = new FrameReader();
		assert.deepEqual(reader.push("{\"a\":"), []);
		assert.ok(reader.pendingBytes() > 0);
		assert.deepEqual(reader.push("1}\n"), ['{"a":1}']);
	});

	it("rejects a single oversized frame", () => {
		const reader = new FrameReader({ maxFrameBytes: 64, maxTotalBytes: 1024 });
		assert.throws(
			() => reader.push(`${"x".repeat(100)}\n`),
			(error: unknown) => error instanceof JevError && error.code === "protocol",
		);
	});

	it("rejects an unterminated oversized frame before it completes", () => {
		const reader = new FrameReader({ maxFrameBytes: 32, maxTotalBytes: 4096 });
		assert.throws(
			() => reader.push("y".repeat(64)),
			(error: unknown) => error instanceof JevError && /oversized/.test(error.message),
		);
	});

	it("rejects a stream larger than the total cap", () => {
		const reader = new FrameReader({ maxFrameBytes: 1024, maxTotalBytes: 2048 });
		assert.throws(
			() => {
				for (let i = 0; i < 10; i++) reader.push(`${"z".repeat(500)}\n`);
			},
			(error: unknown) => error instanceof JevError && /more output/.test(error.message),
		);
	});
});

describe("FrameReader with raw buffers", () => {
	it("rejects malformed UTF-8 and unfinished characters at EOF", () => {
		assert.throws(() => new FrameReader().push(Buffer.from([0xff])), /encoded data/);
		const reader = new FrameReader();
		reader.push(Buffer.from([0xe2]));
		assert.throws(() => reader.finish(), /encoded data/);
	});

	it("keeps a multi-byte character split across two chunks intact", () => {
		const reader = new FrameReader();
		const payload = JSON.stringify({ text: "Tem café e naïve emoji 😀 aqui" });
		const bytes = Buffer.from(`${payload}\n`, "utf8");

		// Split inside the multi-byte sequences, one byte at a time.
		const frames: string[] = [];
		for (const byte of bytes) frames.push(...reader.push(Buffer.from([byte])));

		assert.equal(frames.length, 1);
		assert.deepEqual(JSON.parse(frames[0] as string), JSON.parse(payload));
	});

	it("counts bytes rather than characters against the caps", () => {
		const reader = new FrameReader({ maxFrameBytes: 8, maxTotalBytes: 64 });
		assert.throws(
			() => reader.push(Buffer.from("ééééé\n", "utf8")),
			(error: unknown) => error instanceof JevError && /oversized/.test(error.message),
		);
	});
});

describe("sanitizeOneLine", () => {
	it("removes control characters, bidi overrides, and invisible formatting", () => {
		const hostile = [
			"visible",
			"\u0007bell",
			"\u001b[31mred",
			"\u009bcsi",
			"\u202eflip",
			"\u2066isolate\u2069",
			"\u200bzero",
			"\ufeffbom",
		].join(" ");
		const clean = sanitizeOneLine(hostile, 200);
		for (const code of [0x07, 0x1b, 0x9b, 0x202e, 0x2066, 0x2069, 0x200b, 0xfeff]) {
			assert.ok(!clean.includes(String.fromCodePoint(code)), `code point ${code} must be removed`);
		}
		assert.equal(clean, "visible bell [31mred csi flip isolate zero bom");
	});

	it("collapses whitespace, including newlines and tabs", () => {
		assert.equal(sanitizeOneLine("a\n\n b\t\tc  d"), "a b c d");
		assert.equal(sanitizeOneLine("   "), "");
	});

	it("truncates to the requested width and marks it", () => {
		const truncated = sanitizeOneLine("x".repeat(500), 20);
		assert.equal(truncated.length, 21);
		assert.ok(truncated.endsWith("…"));
	});

	it("returns an empty string for values that are not strings", () => {
		for (const value of [undefined, null, 5, {}, [], Symbol("x")]) {
			assert.equal(sanitizeOneLine(value as unknown), "");
		}
	});

	it("classifies the dangerous code points", () => {
		for (const code of [0x00, 0x1f, 0x7f, 0x85, 0x9f, 0x061c, 0x200f, 0x2028, 0x202a, 0x2069, 0xd800, 0xfeff]) {
			assert.equal(isUnsafeCodePoint(code), true, `expected ${code} to be unsafe`);
		}
		for (const code of [0x20, 0x41, 0xe9, 0x1f600]) {
			assert.equal(isUnsafeCodePoint(code), false, `expected ${code} to be safe`);
		}
	});
});

describe("safePathLabel", () => {
	it("passes an ordinary path through and hides an unusual one", () => {
		assert.equal(safePathLabel("/home/tester/.local/bin/jev-mcp"), "/home/tester/.local/bin/jev-mcp");
		assert.equal(safePathLabel("/home/tester/bin/jev mcp"), "the configured path");
		assert.equal(safePathLabel(`/home/${"x".repeat(300)}`), "the configured path");
	});
});

describe("encodeFrame", () => {
	it("produces exactly one LF-terminated line", () => {
		const line = encodeFrame({ jsonrpc: "2.0", id: 1, method: "initialize" });
		assert.equal(line.endsWith("\n"), true);
		assert.equal(line.slice(0, -1).includes("\n"), false);
		assert.deepEqual(JSON.parse(line), { jsonrpc: "2.0", id: 1, method: "initialize" });
	});

	it("escapes embedded newlines instead of emitting them raw", () => {
		const line = encodeFrame({ text: "a\nb" });
		assert.equal(line.split("\n").length, 2);
	});

	it("rejects an oversized message", () => {
		assert.throws(
			() => encodeFrame({ blob: "x".repeat(MAX_FRAME_BYTES) }),
			(error: unknown) => error instanceof JevError && error.code === "input",
		);
	});
});
