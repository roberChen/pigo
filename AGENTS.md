# AGENTS.md

pigo is a Go re-implementation of the [pi](https://pi.dev) coding agent: a CLI
coding assistant with a headless mode, a line REPL, a full-screen TUI, an
embeddable SDK, plus a Chinese-language book about its internals (`book/`).

Module: `github.com/smallnest/pigo`. Single Go module, no submodules, no
Makefile, no linter config, no code generation (`go:generate` appears nowhere).

## Commands

```bash
go build ./...                 # what CI does first
go vet ./...                   # CI gate; the only static check in the repo
go test -race ./...            # CI gate; whole suite runs in ~20s
go test ./internal/runtime/... # fastest loop for loop/prompt/compaction work

go build ./cmd/pigo            # produces ./pigo (gitignored) — same dir as the repo
go run ./cmd/pigo -p "prompt"  # headless run
go run ./cmd/pigo --no-tui     # line REPL instead of the Bubble Tea TUI
```

Requires Go 1.27+ (`go.mod` pins `go 1.27rc1`). Releases are cut by goreleaser
on `v*` tags (`.goreleaser.yaml`), which injects `main.version/commit/date` via
`-ldflags`; `go build` from source reports `dev`.

Book PDF: `bash book/build_pdf.sh` (needs pandoc + tectonic or a working
xelatex + rsvg-convert). Output and `book/images/*.pdf` are gitignored build
products; `book/README.md` documents the pipeline and its known engine issues.

## Layout and control flow

| Path | Role |
|---|---|
| `cmd/pigo/main.go` | Flag parsing and dispatch only. All real logic lives under `internal/cli/*`. |
| `internal/cli/run` | Run assembly: `SetupEnv` resolves provider, builds tools, discovers skills/plugins/memory, builds the system prompt, installs hooks. Both REPL and headless consume this one API — do not duplicate wiring in a driver. |
| `internal/cli/{repl,tui,headless}` | The three front-ends. `tui` is Bubble Tea v2; `repl` is a hand-written line editor; `headless` covers `-p`, `stream-json`, session resume and the sub-agent RPC server. |
| `internal/runtime` | The agent loop (`loop.go`: inner turn loop + outer follow-up loop, all exits through `finish()`), system-prompt assembly (`prompt.go`), layered config (`config.go`), compaction wiring, sub-agents (`subagent.go`), skills, slash commands, telemetry. |
| `internal/agentcore` | Core types shared by every layer: messages, content blocks, events, `AgentTool`, `AgentContext`, tool execution modes. Leaf-ish; keep it dependency-light. |
| `internal/agenttool` | Built-in tools + the registry/executor (three-phase execution, JSON-Schema validation, batching, retries, output clipping). |
| `internal/provider` | Provider interface, OpenAI and Anthropic wire protocols, SSE transport, auth, preset catalog, model-id → provider inference. |
| `internal/compaction` | Cut-point selection, summarization, token budget. |
| `internal/session` | Append-only JSONL session persistence (schema v3, id/parentId tree), export, share. |
| `internal/trust` | Tri-state directory trust (Trusted/Untrusted/Undecided) gating side-effect tools. |
| `internal/hooks` | User shell hooks (9 events), matcher, runner, protocol. |
| `internal/plugin`, `internal/pkgmgr`, `internal/pihost` | External plugins, `pigo install` of pi-ecosystem packages, and the embedded Node host (`pihost.mjs`) that runs real pi extensions. |
| `internal/memory`, `internal/dream` | Persistent memory (SQLite + FTS5, pure Go) and the LLM consolidation pass. |
| `agent/` | The only importable package: public SDK. Everything else is `internal/` on purpose. |
| `tasks/` | PRDs (`prd-*.md`) and technical specs (`spec-*.md`); `docs/harness-capability-matrix.md` is a living status doc. |
| `docs/` | Static HTML site plus generated-looking `issue#NNNN.html` pages; `docs/agent-loop-flowchart.html` and `docs/pigo-runtime.html` explain the loop. |

Data flow of one run: flags + `config.toml` → `cmd/pigo` dispatch →
`run.SetupEnv` (provider, tools, prompt, hooks) → `runtime` loop streams
assistant messages → tool calls go through `agenttool.BatchConfig` (registry
validation → `BeforeToolCall`/trust/hooks → execute → `AfterToolCall`) →
results are appended and the inner loop repeats until no tool calls, then
`GetFollowUpMessages` decides whether the outer loop runs again.

## Conventions that are not obvious from the code

- **Every file/package opens with a doc comment naming what it implements**, in
  the form `// This file implements X (US-014, #123)` or `// Package y ...`. The
  `US-*` / `FR-*` ids map to a PRD in `tasks/`. New features are expected to
  follow this; it is the primary way to find the rationale for a seam.
- **Language split**: Go code, comments, and commit messages are English; the
  `README.md`, `tasks/` PRDs, `book/` chapters, CI workflow comments and
  `.goreleaser.yaml` comments are Chinese. `CHANGELOG.md` is English. Match the
  file you are editing rather than translating it.
- **Commits** are Conventional Commits, English, with the landing PR number in
  parens when applicable (`feat(provider): ... (#574)`).
- **`internal/` is enforced by design.** The `agent` package is a deliberate
  narrow surface: no exported SDK signature may mention an `internal` type.
  Adding SDK capability means adding SDK-owned option/callback types, not
  re-exporting internals.
- **Tests live in the same package** (`package runtime`, not `runtime_test`) so
  unexported seams are testable; only genuinely external tests (`pihost` e2e)
  use the `_test` suffix. Test files are named after the concern, not the
  method (`loop_onstop_test.go`, `toolpolicy_setup_test.go`).
- **Tool names are lowercase, multi-word with underscores** (`bash_output`,
  `kill_bash`, `memory_search`, `schedule_create`); user-facing matching is
  case-insensitive so Claude Code spellings (`Read`) work.

## Testing notes

- `internal/runtime/faux_provider_test.go` is the project's primary test seam
  for the loop: it is a real `Provider` that replays scripted
  `AssistantMessageEvent` turns through `StreamFnFromProvider`, covering deltas,
  hooks, truncation handling and ordering end to end. Prefer scripting faux turns
  over mocking loop internals.
- Tests must not touch the developer's real state. Anything reading
  `~/.pigo` / `~/.agents/skills` / trust is expected to set
  `t.Setenv("PIGO_HOME", t.TempDir())` (see `cmd/pigo/main_test.go`,
  `internal/trust/manager_test.go`).
- External-tool tests (`internal/pihost/host_e2e_test.go`, requires `node` and
  the pi SDK) skip cleanly instead of failing; keep that pattern for anything
  needing network, credentials or an optional binary. There are no `//go:build`
  tags anywhere and no network-dependent tests in the suite.
- Hook tests execute real shell commands in temp dirs; keep hook tests hermetic
  the same way.

## Gotchas

- **`AGENTS.md` is injected into pigo's own system prompt.** `internal/runtime/prompt.go`
  concatenates every `AGENTS.md` from `PromptConfig.Root` down to `WorkingDir`,
  general → specific. `internal/cli/run/run.go` sets `Root = cwd`, so editing this
  file changes the behavior of any pigo/agent run started from the repo root.
  Nested `AGENTS.md` files are *not* picked up unless the walk includes them.
- **Two config systems coexist; do not confuse them.**
  - User-facing: `~/.config/pigo/config.toml` (TOML, `internal/cli/config`),
    overlaid by flags in `cmd/pigo/main.go`. Precedence: flag > file > default.
  - Internal layered config: `$PIGO_HOME/config.json` (global) and
    `./.pigo/config.json` (project), merged by `runtime.ResolveConfig`. Used for
    thinking level and hooks. Hooks *append-merge* across layers rather than
    overriding, and the project layer is ignored unless the directory is trusted.
  - `--allowed-tools`/`--disallowed-tools` replace their file counterparts
    wholesale (no merge), and deny wins. Filtering happens at tool registration,
    so the boundary is structural: `--approve` cannot widen it.
- **Internal-only flags exist**: `--subagent-rpc` (process-isolated sub-agent
  JSON-RPC server over stdio) and `--dream` / `--dream-dry-run`. Don't advertise
  them as user features or "fix" them into the normal dispatch path.
- **Session files are pigo's own JSONL**, not pi-compatible. `SessionHeader.Version`
  is 3; readers must keep migrating v1/v2 (bare message lines) as `readSession`
  does. Bumping the schema means handling the old shape.
- **Two isolation modes for sub-agents** (`goroutine` default, `process` via a
  child `pigo --subagent-rpc`); the child inherits a *scrubbed* environment, so
  credential-shaped env vars and `PIGO_*` do not leak into it.
- **Memory uses `modernc.org/sqlite`** (CGO-free); keep `CGO_ENABLED=0` builds
  working — goreleaser builds with it.
- **`internal/pihost` embeds `pihost.mjs` with `go:embed`** and writes it to disk
  at install time; it is lockstep with the binary, so never ship it separately.
- **README is the user-facing contract.** Every flag, tool, env var and slash
  command is documented there; adding or changing one without updating
  `README.md` (and usually `config.toml.example`) is considered incomplete.
- **Status docs are maintained by hand**: when landing a harness-level
  capability, update the corresponding row in `docs/harness-capability-matrix.md`
  (it explicitly says so). Larger features start as `tasks/prd-*.md` and get a
  `tasks/spec-*.md` before implementation.

## Adding things (observed patterns)

- **New built-in tool**: implement `agentcore.AgentTool` (`Name`, `Description`,
  `Schema` returning an inline JSON Schema, `ExecutionMode`, `Execute`) as
  `internal/agenttool/<name>_tool.go`; validate args through the registry rather
  than hand-parsing; register it in `run.BuiltinTools`; add the name to the
  README tool table and think about `--allowed-tools` docs.
- **New provider / model preset**: extend `internal/provider/presets.go` plus the
  resolution logic in `infer.go`/`resolve.go`; keys are referenced by env-var
  name only and are never embedded or logged.
- **New slash command**: `internal/runtime/slashcommand.go` (model/runtime) and
  `internal/cli/prompts/registry.go` (CLI-visible); REPL and TUI share the
  registry so both front-ends stay in sync.
- **New layer between CLI and runtime**: put the contract in
  `internal/cli/host.go` (interfaces) and the assembly in `internal/cli/run`, so
  `repl`, `tui` and `headless` keep using one wiring path.
