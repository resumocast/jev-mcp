# Jev MCP

A community experimental MCP server for the TypeSafe Jev API, with a native Pi adapter.

**Status: implemented with offline regression coverage and a verified Pi-to-API path. Workflow quality, cost savings, and useful tool selection are not established by connectivity tests.**


This project is not affiliated with, sponsored by, or endorsed by TypeSafe AI. It is a bounded third-party client: one fixed endpoint, one pinned model (`jev-1.13.0`), and no claim of official support or production readiness.

## Quickstart

These steps install only for the current user. There is no installer, self-update, or remote-install command. Review hashes before replacing files. Never put an API key on the command line or in the environment.

1. **Build** (macOS; needs the verification toolchain below):

```sh
npm ci --ignore-scripts
make build
```

2. **Install the binary and adapter** (back up any existing files first):

```sh
mkdir -p ~/.local/bin ~/.pi/agent/extensions ~/.config/typesafe
cp dist/jev-mcp-darwin-$(uname -m | sed 's/x86_64/amd64/') ~/.local/bin/jev-mcp
chmod 755 ~/.local/bin/jev-mcp
cp adapters/pi/jev.ts ~/.pi/agent/extensions/jev.ts
```

Ensure `~/.local/bin` is on your `PATH`.

3. **Credential file** (mode `0600`, owned regular file only):

```sh
# Place your TypeSafe Jev API key as the sole contents of this file, then:
chmod 600 ~/.config/typesafe/jev-pi.key
```

4. **Load in Pi**: start a **new** Pi session so the extension is discovered, or run `/reload` in an existing session. Do not overwrite `herdr-agent-state.ts` or force-reload unrelated active agents.

5. **Smoke check offline** (optional; never calls TypeSafe):

```sh
make test test-binary
```

Paid calls go only to `https://api.typesafe.ai/v1/systemone` and can incur charges.

### Opt-out / remove

```sh
rm -f ~/.pi/agent/extensions/jev.ts ~/.local/bin/jev-mcp
# Credential removal is separate and intentional:
# rm -f ~/.config/typesafe/jev-pi.key
```

Staged testing may set path-only `JEV_MCP_BIN` and `JEV_MCP_KEY_FILE` instead of the default paths. Relative key paths are rejected.

## Support matrix (exact, not broad)

| Component | What this tree actually pins or observed |
| --- | --- |
| Go module language | `go 1.26` in `go.mod` (minimum language version) |
| Go verification / `make` | **Go 1.27.1** required by `Makefile` (`GOTOOLCHAIN=local`) |
| Node | `>=22.19.0` (`package.json` engines); lockfile used for adapter tests |
| Pi adapter test dependencies | `@earendil-works/pi-*` **0.85.1** (devDependencies) |
| Observed installed Pi runtime | **0.86.1** used in separate offline lifecycle checks; not a general compatibility claim |
| OS build artifacts | `make build` emits `darwin/arm64` and `darwin/amd64` only |
| MCP clients | Native Pi adapter exercised; other clients need their own configuration and verification |
| Python | 3.9+ for `tools/export_public.py` and export tests only |

No other Go, Node, Pi, OS, or client versions are claimed.

## Scope

```text
Pi model chooses evaluate
  -> Pi adapter (TypeScript)
  -> local MCP server (Go, stdio)
  -> official TypeSafe API
  -> structured result and visible Pi feedback
```

The default tool, `evaluate`, supports `noul` (yes/no probability), `choice` (a named option and its distribution), and `score` (a probability-weighted position on ordered levels). Callers cannot select another model or endpoint.

This is not automatic model routing, an interceptor for other tools, a replacement for Herdr, or a separate evaluation CLI. Use Jev for bounded semantic uncertainty, not arithmetic, counting, date calculations, generation, deterministic tests, authorization, or security decisions. Supply evidence rather than an asserted conclusion. Selection by the host model is probabilistic. Confidence does not prove correctness or resistance to prompt injection.

## Source layout

| Path | Purpose |
| --- | --- |
| `cmd/jev-mcp/` | Stdio executable and argument handling |
| `internal/mcp/` | Bounded JSON-RPC transport, handshake, dispatch, cancellation |
| `internal/typesafe/` | Credential loading, API validation, HTTP client, retries |
| `internal/jsonfields/` | Canonical JSON field-name guards |
| `adapters/pi/jev.ts` | Standalone extension (copy into Pi extensions; not auto-installed by this repo) |
| `tests/pi/` | Synthetic subprocess, validation, rendering, and Pi SDK tests |
| `tests/*smoke.*` | Offline native-binary protocol and adapter checks |
| `examples/evaluate.json` | Fictional schema example, not workflow evidence |
| `tools/export_public.py` | Local allowlisted export with no Git history |
| `public-files.json` | Explicit file list for a history-free public snapshot |

## Tool input

Pass `state` as text, a JSON object, or an array, plus a `questions` map. Each question has `type`, `instructions`, and an optional or required `criteria` field:

| Type | Criteria |
| --- | --- |
| `noul` | Optional object with `true` and/or `false` string descriptions |
| `choice` | Required object mapping 1-255 option names to string descriptions or `null` |
| `score` | Required array of 2-10 string level descriptions, ordered low to high |

Version one supports string rubric descriptions, not the structured descriptions available in some TypeSafe examples. Nested standard JSON values in state and instructions are supported. See [`examples/evaluate.json`](examples/evaluate.json).

Results contain the model, one answer per question, and token usage. Choice and score carry confidence; noul returns its yes/no probability. Score levels start at zero. Invalid or inconsistent responses are rejected rather than reported as successful judgments.

## Experimental evidence selection

Starting the server with `--enable-selection` additionally exposes `select_evidence`; without that flag, discovery and `evaluate` behavior are unchanged. This is experimental, not an installed automatic filter or a one-command installer. The native Pi adapter exposes only `evaluate` by default; an explicit project-root flag additionally enables `select_context`.

The tool accepts a `task` string and 1-16 `items`, each with a unique `id` and source `text`. A fixed `evidence-selection-v1` rubric submits one existing choice question per item in a single bounded API request. It drops an item only when the validated floating-point probability of `irrelevant` is at least 0.90; other items remain selected, with explicit review and no-match states. The threshold uses the existing client's normalized float64 representation, not exact decimal API lexemes, and is a policy choice rather than a calibrated guarantee.

Results include ordered selected IDs, per-item classifications/probabilities, original-text SHA256 hashes, model/rubric versions, and usage. Originals stay with the caller. API failures remain errors rather than successful empty selections. The tool does not fetch URLs, read source files, generate code, or authorize actions. Existing API request/state bounds can reject a batch even when every item fits its individual limit.

To reduce host input, an explicitly enabled caller-side preprocessing step must supply the source text **before the main assistant reads it**, then pass selected material to the assistant while retaining recovery access to originals. Calling selection on text already in the main model's context does not recover those input tokens. That sequence is a design option, not evidence of automatic tool adoption or production savings.

### Opt-in native committed context

Load exactly one copy of the adapter and use `--jev-select-root /canonical/absolute/project/root` from that same Git top-level directory. A matching server build with selection support is required. This registers `select_context` alongside `evaluate`; it does not intercept existing tools or read arbitrary repository content.

Supply `task`, `sources: [{path, start_line, end_line}]`, optional `expected_commit`, and `mode: "select"` or `"read"`. Sources must be regular committed files in the current HEAD's `public-files.json` allowlist. `expected_commit` only checks HEAD equality; it cannot select history. Working-tree edits, symlinks, protected context/configuration paths, Git filters and lazy fetching are not source inputs. Each result includes commit, blob/excerpt hashes and retrieval ranges. IDs are assigned in request order and restart on each call.

Select mode privately collects the ranges, submits source IDs/text and the task to the unchanged selection policy, and returns text only for keep/review decisions. Read mode returns every requested original without an API call. Retrieval paths and line metadata are not added to the external request, but paths already present in task or source text are transmitted. Neither allowlisting nor credential-pattern checks prove confidentiality.

Limits are 16 ranges, 200 lines and 8 KiB per excerpt, 16 KiB aggregate source text, 256 KiB per blob, a 4,096-byte task, 240-byte paths, 32 KiB output, and a 35-second call deadline. Conservative output preflight can reject otherwise valid inputs. There is no silent fallback or truncation.

Paid selection requires a flushed persistent Pi session. It reserves a slot before execution; failures after reservation can consume that slot even if no API request succeeds. The default cap is three reservations per session, reduced with `--jev-select-budget 1` or `2`. Read mode and pre-reservation input rejection consume no slot. Reopening or branching does not refund recorded reservations. Concurrent processes writing one session are unsupported.

Offline regressions exercise collection, recovery, persistence, and lifecycle binding with the Pi **0.85.1** test dependencies; separate offline lifecycle checks were also run against an installed Pi **0.86.1** runtime. These checks do not establish task quality, natural adoption, or savings.

## Safety and resource policy

- **CONTROL-1: Dedicated credential file.** Default `~/.config/typesafe/jev-pi.key`; absolute path override via `--key-file`. It must be an owned regular file with mode `0600`. Final-component symlinks, FIFOs, malformed contents, and oversized files are rejected. Keys are not accepted through tool arguments, command-line values, or environment variables. File checks use the opened descriptor. Parent directories and macOS ACLs are not a same-user isolation boundary.
- **CONTROL-2: Fixed network destination.** Only `https://api.typesafe.ai/v1/systemone`; no production endpoint override, OpenRouter, redirects, or ambient HTTP proxy configuration. Calls send data to TypeSafe and can incur charges.
- **CONTROL-3: Bounded work.** At most 16 questions, a 64 KiB API request, a 128 KiB API response, and a 128 KiB MCP frame. Fields have additional bounds; value nesting is limited to eight levels. These are resource limits, not token-budget guarantees. Oversized results are errors, not silently incomplete answers.
- **CONTROL-4: Deadlines and retries.** A 30-second API deadline covers attempts, backoff, and body reads. At most two retries, only for 429/529. Ambiguous network failures of a POST are not retried. The adapter has its own deadline and bounded cleanup.
- **CONTROL-5: Process ownership.** One active evaluation per runner instance, with excess calls rejected rather than queued. The adapter waits for initialization, forwards only `HOME`, a fixed `PATH`, and valid `LANG`, and reaps its owned child. Late protocol errors or an unclean exit fail the call even after an answer arrives. This is not a machine-wide spending cap. Native context collection also owns its Git children. Incomplete shutdown is reported, but Pi can catch lifecycle errors and continue runtime replacement; the error is not proof that replacement was prevented.
- **CONTROL-6: Output boundaries.** Stdout carries only MCP messages. Errors exclude raw HTTP bodies and child diagnostics. Fields are checked against the request, unknown fields dropped, and terminal rendering treats text as untrusted. Tool text has a separate size limit.

Mode `0600` does not isolate processes running as the same user. Never put credentials in state or questions. Pi transcripts can retain arguments and results; this project does not promise their disappearance or zero retention by providers.

## Development and verification

Use the **Support matrix** pins above. The Go server uses only the standard library. Substantial builds and race-enabled tests are intended for a machine with that toolchain installed.

```sh
npm ci --ignore-scripts
make test test-binary
```

`make test` checks Go formatting, runs `go vet`, race-enabled Go tests, TypeScript checking, Pi tests, and export-boundary tests. `make build` produces macOS ARM64/AMD64 binaries and `dist/SHA256SUMS`, with CGO disabled and build paths/VCS metadata excluded. Go targets select `cmd/` and `internal/` explicitly so temporary toolchain sources cannot enter package discovery.

`make test-binary` rebuilds and tests the native artifact, including the actual Pi adapter talking to the server. Only invalid evaluations are submitted in that offline integration check, so it never calls TypeSafe. ARM64 and Intel artifacts require native smoke checks on their respective machines.

Provider-facing instructions keep question formats in the parameter schema rather than repeating them in the main description. Regression checks cover both interface size and retained evidence, probability, and safety guidance. Smaller instructions alone do not establish better answers, lower bills, or faster workflows.

Regression coverage includes request/response validation, canonical and duplicate JSON fields, credential permissions, retry ceilings, cancellation, stdout backpressure, half-close flushing, subprocess cleanup, malformed output, UTF-8 framing, terminal sanitation, and actual Pi SDK error conversion. Synthetic fixtures exercise these boundaries; they do not establish judgment quality or workflow value.

## Global user install paths

| Path | Role |
| --- | --- |
| `~/.local/bin/jev-mcp` | Stdio MCP server binary |
| `~/.pi/agent/extensions/jev.ts` | Pi extension (user-global discovery) |
| `~/.config/typesafe/jev-pi.key` | Mode-`0600` TypeSafe credential (not shipped) |

Installation is manual copy only. Verify checksums from `dist/SHA256SUMS` when using release binaries. Prefer backing up existing extension files outside discovery paths before replacing them. New Pi sessions load the installed adapter; existing sessions need an explicit reload. Rollback restores previous code files or removes only the paths above after checking hashes. Credential removal is a separate decision.

## History-free public snapshots

Product regression tests use synthetic fixtures. Research tooling, real datasets, labels, prompts, transcripts, experimental results, operational receipts, and review archives belong outside this product tree. Git ignores are a convenience, not a confidentiality guarantee.

**Never publish this repository's existing private Git history.** A public tree must start from reviewed files produced by a clean allowlisted export, not from `git push --mirror` or a visibility change on the private remote:

```sh
python3 tools/export_public.py /absolute/path/to/new-export --commit <reviewed-commit>
```

The exporter reads only the explicit `public-files.json` entries from that commit, rejects research/credential paths and symlinks, checks a narrow set of credential patterns, and produces hashes in `EXPORT.json`. It excludes `.git`, untracked files, and all unlisted paths. It does not push, publish, or alter repository visibility. Human content review is still required: an allowlisted file can contain sensitive text that a pattern scanner cannot recognize.

## Product roadmap

**Product direction:** an add-on for existing coding assistants that offloads routine judgments, aiming to reduce expensive host-model work without degrading task outcomes. One-command installation and longer-lasting usage allowances are targets, not current capabilities or established results. Subscription limits are provider-specific; lower token counts do not by themselves prove a longer allowance.

Build on the existing server, TypeSafe client, validation, native Pi adapter, and regression suite. A rewrite would discard useful boundaries without addressing the missing workflow benefit. Preserve `evaluate` as the low-level interface while adding a small task-oriented layer above it.

### Existing foundation

- [x] **ACTION-1: Establish the product tree.** Bounded MCP server, Pi adapter, offline regressions, and provenance notices.
- [x] **ACTION-2: Preserve provenance.** Retain upstream MIT attribution; exclude installer, updater, and alternate-provider behavior.
- [x] **ACTION-3: Verify the bounded MCP server offline.** Input/output, credentials, retries, deadlines, framing, cancellation, and JSON boundaries.
- [x] **ACTION-4: Verify the Pi adapter offline.** Schema validation, handshake, environment restriction, concurrency, cleanup, and SDK error behavior.
- [x] **ACTION-5: Verify feedback components.** Running, success, failure, duration, bounded expansion, and malformed metadata handling.
- [x] **ACTION-6: Verify build artifacts.** Pinned toolchain, tests, cross-compilation, checksums, and native smoke tests.
- [ ] **ACTION-7: Establish workflow value.** Connectivity alone is not evidence of usefulness; quality and cost must be measured separately.
- [x] **ACTION-8: Define installation boundaries.** Explicit paths, hashes, backup, rollback, and manual install only.
- [x] **ACTION-9: Exercise manual installation.** Verified code deployment and offline startup without reloading unrelated agents.
- [x] **ACTION-10: Separate research from product.** Keep research material out of the allowlisted export; never publish private Git history.

### Next product increment

These stages guide the next version. Experimental selection and the opt-in collector cover part of ACTION-17; they do not complete the workflow-value gate. Planning does not authorize unrelated data collection, credential changes, or automatic installation into active harnesses.

| Stage | Work | User outcome and evidence needed |
| --- | --- | --- |
| **ACTION-16: Choose one valuable routine task.** | Start with selecting relevant material from a caller-supplied batch of file excerpts or tool-output sections. Obtain independently adjudicated cases and freeze the comparison criteria before optimization. | The main agent should read less material without missing evidence needed to finish the task. Compare against ordinary host reasoning and a cheap deterministic filter, not only against our previous version. |
| **ACTION-17: Add a bounded batch workflow.** | Reuse the existing provider/validation path. A caller-side adapter or explicitly authorized source collector must supply the batch before the main model reads all its contents; passing already-read text to Jev does not save those input tokens. Add reusable versioned rubrics, stable item IDs, explicit unknown/no-match outcomes, per-item failures, source revisions, and request/time/size budgets. Start with the minimum rubric shape needed rather than implementing every candidate feature. | One host request replaces repeated per-item judgment work. Originals remain available; the service neither generates code nor approves actions. No arbitrary filesystem access or hidden provider switching. |
| **ACTION-18: Establish net workflow value.** | Measure end-to-end workflows with separate development and holdout cases. Include collection, host continuations, tool descriptions, cached input, Jev calls, retries, errors, retrieval and re-reads. Measure natural tool adoption separately from forced-use diagnostics or explicitly enabled preprocessing. Keep costs with missing usage marked unknown. | Retain the workflow only when it meets predeclared quality and cost/latency criteria. Report inference estimates separately from actual subscription allowances. If it fails, revise or stop rather than build an installer around an unproven benefit. |
| **ACTION-19: Verify additional harnesses.** | Keep the tested Pi path and exercise actual Claude Code and Codex clients next. Implement and test needed protocol compatibility, including modern discovery and per-request semantics where required; do not merely add advertised version strings. Expand the support matrix only with named client/version/OS evidence. | The same useful service works across tested assistants. MCP connectivity is distinct from native access to their context or tool lifecycle. Preserve the current credential, framing, cancellation, and cleanup boundaries. |
| **ACTION-20: Make setup simple and reversible.** | After the value and compatibility stages, provide an explicit one-command installer with verified artifacts, a configuration preview, opt-in client registration, an offline diagnostic, and rollback. TypeSafe account/key setup and billing disclosure remain separate onboarding requirements. | A user can add or remove the integration without manual file copying or losing unrelated settings. Never accept a key as a command-line value, replace Herdr, or silently reload active sessions. |
| **ACTION-21: Expand only demonstrated savings.** | Next consider reversible context selection, revision-aware exact caching, and thin opt-in native adapters. Add source-linked claim assessment or progressive discovery only when a real task or catalog size justifies them. | Reduce repeated reading and judgment while keeping original evidence recoverable. Automatic application requires supported, runtime-tested harness hooks and separate consent; a standalone MCP server cannot intercept every host. |
| **ACTION-22: Distribute only reviewed snapshots.** | Review licenses and notices, create a fresh allowlisted history-free export, and obtain explicit approval before registry packages or any new public remote. | A reproducible installation with accurate compatibility and benefit claims, without exposing research or historical private material. Never change the visibility of a private remote that still contains non-public history. |

The first useful increment is ACTION-16 through ACTION-18, not all proposed ecosystem features at once. Compatibility review can proceed alongside that work; broad distribution follows demonstrated value. Alternative engines, model training, automatic model routing, a general-purpose gateway, and an autonomous agent manager are not part of this increment.

## Source and license provenance

The initial API/tool design derives from [itsmostafa/typesafe-mcp](https://github.com/itsmostafa/typesafe-mcp), revision `0c9f35d9b1859189fc7e7d01947061f311ca6dde` (v0.4.1). The client, validation, transport, and adapter were rewritten for the boundaries above rather than importing upstream host-configuration behavior.

See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE). Upstream MIT notices must accompany derived code.

## Primary references

- [TypeSafe API](https://docs.typesafe.ai/api.md)
- [Models and limits](https://docs.typesafe.ai/models.md)
- [Structured state](https://docs.typesafe.ai/concepts/state.md)
- [Score levels](https://docs.typesafe.ai/primitives/score.md)
- [Confidence semantics](https://docs.typesafe.ai/confidence.md)
- [Known Jev limitations](https://docs.typesafe.ai/model-jaggedness/jev-1.13.md)
