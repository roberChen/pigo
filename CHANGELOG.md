# Changelog

All notable changes to **pigo** are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

pigo is a Go re-implementation of the [pi](https://pi.dev) AI coding agent — a
command-line coding assistant with both a headless script mode and an
interactive REPL/TUI.

## [Unreleased]

### Fixed
- **The TUI's context-usage readout is now permanent and live**: the amber `◔`
  block only appeared after a run's end-of-run telemetry and then froze until the
  next run finished, so it was absent exactly when a long run made it interesting.
  It is now always rendered (a `◔ --` placeholder until the first measurement) and
  is driven by a context meter fed from the events the bridge already delivers:
  the submitted prompt is counted on Enter, streamed assistant text grows the
  estimate as it arrives, each settled turn corrects it with the provider's usage
  block (or a character estimate when none is reported) and adds the turn's tool
  results, a compaction collapses it to the loop's post-compaction count (now
  carried on `compactionMsg`), and the run-end telemetry still reconciles it.
  `/resume`, `/new` and `/rebuild` re-seed it from the conversation they switch to.
  Every update uses copies carried by bridged messages, so the tea goroutine never
  reads the context the pump goroutine is writing.
- **One malformed tool call no longer poisons the rest of the session**: a model
  that streamed invalid JSON as a tool's arguments was answered with a
  "not valid JSON" tool result and given another turn — but its broken call stayed
  in history and was replayed verbatim on every later request. Upstreams validate
  replayed arguments, so the next request was rejected with a 400 and the
  conversation could never continue, from any front-end and after a resume. A
  replayed call now carries `{}` when its arguments are not a JSON object, the same
  policy the Anthropic wire already applied, while the in-memory message and the
  session file keep the model's original bytes for diagnostics. Well-formed
  arguments are replayed byte for byte, and Anthropic inputs keep integers exact
  instead of re-encoding them as floats.
- **A tool call with no id, a reused id, or an unusable name no longer blocks the
  session**: the same replay problem had three more shapes. A call streamed without
  an id — or two calls sharing one — left the loop's tool result and the session's
  copy of the call paired ambiguously, or not at all, and a name that was empty,
  longer than 64 characters, or carried spaces or non-ASCII characters failed the
  provider's name pattern before the model could be told the call had failed.
  Decoders now mint a unique, replayable id per call as the turn is materialized, so
  the tool result, the session file and every later request agree without a wire
  rewriting anything. Replay also repairs what an older session (or an SDK caller)
  still holds: ids are sanitized and de-duplicated, a name that cannot be replayed
  becomes `invalid_tool_call`, a result with no call to pair with is dropped, and a
  call no result answers gets an explanatory error result. The model's raw name stays
  in the recorded message, so the error result it was given still quotes it.
- **Interleaved thinking no longer collapses into one block**: an Anthropic turn can
  alternate thinking and `tool_use` several times, and each thinking block must be
  echoed back adjacent to the tool call that follows it or the next request is
  rejected. Blocks are now kept in production order (routed by content-block index)
  instead of being grouped by channel, while `redacted_thinking` payloads are
  preserved and empty thinking/text blocks are dropped. Chat-style wires, which have
  no block boundaries, still keep their reasoning as one leading block even when the
  gateway emits it after the answer.
- **The TUI shows a reasoning block for `deepseek` and other Responses-API
  providers**: the driver only understood OpenAI's
  `response.reasoning_summary_text.delta`, but DeepSeek's Responses endpoint streams
  raw reasoning as `response.reasoning_text.delta`, so every reasoning delta was
  silently dropped while the answer streamed normally. Raw reasoning deltas are now
  decoded from the payload's string delta, and the terminal message also reads the
  reasoning item's `content` parts (with `summary` empty), so the thinking block
  survives into the session and reappears on resume.
- **Clicking a tool card in the transcript now always toggles that card**: two bugs
  sat between the pointer and the click. The row arithmetic counted the newline that
  *terminates* a card as a blank separator row, but the transcript only draws a blank
  row before a user turn, so each separator shifted every later card one row late —
  by the third card the header belonged to nobody and its top border selected the
  previous card. And a click was only recognized when press and release landed on the
  exact same cell, so the one-cell drift of an ordinary trackpad click was taken for
  a drag and only drew a text selection. Row mapping now matches the renderer,
  rows below the transcript (input line, status bar) no longer resolve against
  scrolled content, motion only extends a selection while the button is held, and a
  click tolerates a cell of jitter. A click is also resolved against the frame that
  was actually painted: while a run streams, output appended after that frame moves
  every row under the pointer, so the old lookup toggled whichever card had
  scrolled into the clicked row instead of the one the user pointed at.
- **A thinking block now shows the newest reasoning and opens on click**: the
  reasoning preview kept the *first* six lines, which is the part the model has
  already moved past — while streaming, the interesting rows are the ones being
  written right now. The preview now keeps the tail and labels the hidden rows above
  it (`… N earlier lines · click to expand`). The block also became a click target,
  like a tool card: clicking anywhere on it expands the whole chain of thought
  (capped at 400 rows, with the rest in the session file) and clicking again
  collapses it. The expansion keeps the header where it is and fills the older rows
  in below it, so the chain reads in order instead of the view jumping to the newest
  line; a block that already fits the preview is not a click target. Clicks are
  resolved through the same painted-frame map as cards, and replayed reasoning from a
  resumed session is clickable too.
- **Expanding a tool card now shows the call's arguments in full**: the card body
  repeated only the arguments the header did not show, flattened every value onto one
  line and cut it at 120 columns — even when expanded. A bash card therefore hid the
  rest of a multi-line script behind the header's 80-column summary, and a `write`
  card hid the body of the file it wrote. An expanded card now lists every argument,
  including the header's, with line breaks preserved and no column budget (only the
  400-row ceiling remains, which says where the rest is); an MCP card breaks its
  one-line JSON summary out key by key. The collapsed card is unchanged.
- **Tool cards no longer crash the TUI on non-ASCII arguments**: the card's
  line-clipping helper measured a word boundary in bytes and then sliced runes, so a
  long CJK argument (or any multibyte text past the clip budget) ran off the end of
  the slice and took the whole session down while rendering. Clipping now walks runes
  and backs up to a whitespace boundary only within the rune budget. An MCP call also
  shows its arguments as one compact JSON object again, instead of an arbitrary
  single key=value.
- **Resuming a session loads the conversation you were on**: a session is a tree,
  and branching (`/tree` leaf switch, `/rewind`, a continued fork) leaves sibling
  branches in the same file. Resume used to read every entry in file order, so it
  could replay a turn you had rewound away, interleave two branches, and then append
  the next turn to the wrong place. Resume (REPL, TUI and headless) now follows the
  path to the session's active leaf, and the next turn continues from it.
- **Resuming a compacted session keeps the whole conversation visible**: replay shows
  the turns before the compaction as well, with a marker where the history was
  summarized, while the agent's context starts at the checkpoint so the summarized
  prefix is not resent (and re-compacted). The REPL replay also renders assistant
  replies as Markdown again instead of printing the raw source.
- **Compacting no longer deletes the conversation it summarizes**: after a
  compaction the session file was rewritten to just the checkpoint plus the retained
  tail, so the earlier turns were gone from disk (and unrecoverable without memory
  checkpoints). The compacted context is now appended to the tree instead — the
  checkpoint attaches to the last summarized entry and the retained tail follows it,
  so `/tree`, export and a later resume still have the full history. A second
  compaction attaches at the right node when the session was resumed first.

### Added
- **One assistant-turn model behind every wire protocol**: the OpenAI Chat,
  Anthropic and Responses drivers used to accumulate a response each in their own
  way — block ordering, empty-block suppression, usage and stop-reason mapping were
  re-implemented three times, so a capability could work on one protocol and quietly
  do nothing on another. All three now record into a shared accumulator, and a
  cross-protocol conformance suite drives the same logical turn through each real
  decoder and asserts identical results (thinking → text → tool call, joined
  arguments, shared usage, `tool_use`). The Responses driver also stops going
  through the `openai-go` SDK and speaks SSE on the shared transport, which drops
  the dependency (and four transitive ones), gives it the same retries, idle/stall
  watchdogs and dual failure model as the other drivers, and lets it read events a
  pinned SDK union would reject as unknown.
- **TUI: the `/model` picker is navigable from the keyboard**: a bare `/model`
  (or `/models`) opens a filterable overlay instead of printing the catalog, and
  the highlight now moves with `↑`/`↓` (or `Ctrl+P`/`Ctrl+N`), wrapping at both
  ends; `PgUp`/`PgDn` jump a screenful and clamp at the edges, `Home`/`End` jump to
  the first/last row, and the mouse wheel walks the list too. The filter line
  shows the position (`(12/95)`) so a long catalog does not hide where the cursor
  is, typing resets the cursor to the first match, and `Enter` performs exactly the
  `/model <id>` switch through `prompts.ApplyModelSwitch` (the status bar updates
  immediately).
- **TUI: reasoning, per-tool cards, card selection and input marking**: the TUI
  now shows a model's reasoning as a dim `✻ thinking` block capped at six lines, and
  a user turn carries a `▌ you` gutter with long input capped and counted, so the
  human's words are never confused with model output. Tool cards render by tool
  instead of dumping every argument and every result line: bash keeps its command in
  the header and shows the head and tail of its output, read/write/edit show the path
  plus a colored diff, grep/find show their pattern, todo renders a status checklist
  with a progress line, and an MCP tool shows `mcp server/tool` with a one-line
  argument summary and a short result preview instead of the full JSON and response.
  Every section has a line budget and says how much it hides. Any card in the history
  can be picked — click it, or walk the cards with Alt+↑/Alt+↓ — and Ctrl+O (or a
  second click) expands or collapses that card, not just the newest one.
- **`/new` starts a fresh empty session**: the interactive modes can now begin a
  new conversation without restarting pigo. It persists the outgoing session first,
  so nothing is lost and `/resume` (or `--resume`) brings it back, then mints a new
  session id, clears the conversation, re-dispatches SessionStart so
  context-injecting hooks run for the new session, and resets the branch cursor,
  side thread and telemetry. Everything that belongs to the session machinery
  rather than to one conversation — tools, hooks, MCP connections, the memory store,
  trust and the slash registry — stays in place, and the new session runs the
  configuration that is live now, including anything a reload applied.
- **Live configuration reload**: editing a pigorc or config.toml no longer
  requires restarting pigo. The interactive modes check the config files'
  modification time before each turn and apply a changed one first, and `/reload`
  applies them on demand (REPL and TUI alike). A reload re-reads the pigorc (and the
  environment variables it exports), config.toml, third-party provider definitions
  (an edited definition replaces the old one, a deleted one stops resolving),
  custom sub-agents, MCP servers (reconnected), hooks, skills, the system prompt,
  the tool set and its boundary, and the model/provider/reasoning level. Session
  state is carried over instead of being thrown away: background shell jobs, the
  todo list, the reminder schedule, background sub-agents, the memory database and
  the plugin processes. A configuration that cannot be assembled is rejected and
  the running one keeps working, with the reason reported.
- **`pigo_config` tool**: the agent can now report its own effective
  configuration instead of guessing. One read-only tool answers "which config files
  are actually loaded?", "is that hook active?", "why is this tool missing?" and
  "which MCP servers connected?" by rendering what the run assembled: the
  config.toml / global and project config.json / pigorc layers (with
  present-but-skipped files named as such, e.g. a project layer under an untrusted
  directory), the resolved model, provider, endpoint, reasoning effort and output
  format, the advertised tools grouped by origin plus the
  `--allowed-tools`/`--disallowed-tools` boundary, skills, configured versus
  connected MCP servers with their tool counts, the resolved hook set, custom
  sub-agents, and the memory/checkpoint/compaction settings. A `section` argument
  narrows the report. `--no-tools` disables it along with every other tool.
- **MCP client support**: pigo connects to external Model Context Protocol servers
  and exposes their tools as `mcp__<server>__<tool>`, with the server's argument
  schema passed through. Servers that advertise resources also get
  `list_resources` / `read_resource`, and servers that advertise prompts get
  `list_prompts` / `get_prompt`. Three transports are supported: stdio
  (`command`/`args`/`env`, line-delimited JSON-RPC), Streamable HTTP (`url`/
  `headers`, JSON or SSE replies, session id echoed), and the legacy HTTP+SSE
  transport (`transport = "sse"`). HTTP servers behind OAuth run the MCP
  authorization flow (`oauth = true`: protected-resource and authorization-server
  discovery, dynamic client registration, authorization code with PKCE over a
  loopback redirect, token caching under `$PIGO_HOME/mcp-oauth/` and refresh).
  Server-initiated requests are answered (`ping`, `roots/list`), while sampling and
  elicitation are declined explicitly. A `tools/resources/prompts list_changed`
  notification hot-reloads the affected tool set on the next turn instead of
  requiring a restart. Servers are configured with `[[mcp]]` tables in config.toml
  or `mcp add ...` / `mcp remove ...` in a pigorc (a project pigorc layers onto the
  global one, so it can add args/env/headers or disable a server without repeating
  the command). Connections happen during tool assembly, a server that fails to
  start or initialize is reported and skipped, `--no-tools` disables MCP like the
  other tools, results map text/image/embedded resources onto pigo content blocks,
  `isError` becomes a tool error, and per-request timeouts default to 60s. The
  generated configuration reference documents the fields and DSL automatically.
- **Built-in `pigorc` skill with a generated config reference**: pigo now ships a
  `pigorc` skill (installed to the skills directory on first run, invocable as
  `/pigorc`) whose hand-written `SKILL.md` covers the workflow and recipes, alongside
  a `reference.md` that is **regenerated from the running binary on every launch**.
  The reference is rendered from the code that implements the configuration — the
  `option` registry, provider and agent definition fields, the hook event list, the
  environment variables the code reads, and every registered CLI flag — so it cannot
  go stale. Coverage tests fail when a config.toml field, definition field, hook
  event, `PIGO_*` variable or CLI flag has no documented entry, and the generator only
  rewrites pigo's own skill directory (a user's hand-written skill of the same name
  is left untouched).
- **Background sub-agents**: a `task` / custom-agent call with `background: true`
  starts the child on its own goroutine and returns a job id immediately, so the
  parent can keep working. The report is delivered as a follow-up user turn when
  the child finishes (the run does not settle while a background child is still
  running, and waiting stops if the run is cancelled), and the new `task_output`
  tool lists jobs, reports status, or waits for one. Jobs run on the parent run's
  context, share the existing concurrency cap, persist their own session like any
  other sub-agent, and are rejected in process isolation (which has no resumable
  session). The tool registry, the follow-up seam and the goal loop are wired so
  reports reach the model wherever the run was started.
- **Custom sub-agents from the pigorc**: `agent add ...` / `agent hook ...` in
  the pigorc (the single source of truth for agent definitions; there is no
  config.toml agent table) define named agents the main agent dispatches as tools, each
  with its own system prompt, tool allow list, model, reasoning effort, timeout
  and isolation. A defined agent runs as a complete agent: it compacts like a
  top-level run and persists its own session, parented to the calling
  conversation and marked with the agent name. Those sessions are hidden from the
  resume listings and cannot be resumed directly; the parent continues one by
  passing the reported session id, which is also the documented fallback for a
  decision: sub-agents never get the `question` tool, the main agent asks the user
  and then continues the child. Agent hooks are the agent's own by default
  (`inherit_hooks` also runs the global/project set), tool-execution hooks and
  `SubagentStop` are wired on the child run, and a bad definition (unknown tool,
  isolation or thinking level, name collision) warns and is skipped.
- **Custom third-party providers from config**: `[[providers]]` tables in
  `config.toml` (and `provider add <name> ...` in a pigorc) register a gateway
  that is not in the built-in registry, after which it behaves like one:
  `--provider <name>` builds its driver from the declared protocol/base URL/auth
  scheme, listed `models` resolve by id, a bare `--model <name>` selects the
  first of them, and `/model` plus the preset picker list it. The API key comes
  from `env_var` (default `<NAME>_API_KEY`), a named credential reference, or an
  inline `api_key` that stays out of the process environment (the environment
  still wins, so keys rotate without editing config). `<NAME>_BASE_URL` overrides
  the endpoint as for built-ins; a name that collides with a built-in, a missing
  base URL or an unknown protocol warns and is skipped instead of aborting the
  launch.
- **`pigorc`: a Bash-script config layer**: `$PIGO_HOME/pigorc` (always) and
  `./.pigorc` (only in a trusted directory) are sourced by one `bash` process at
  startup, so includes, conditionals, variables and `$(command)` secrets work.
  Options (`option <key> <value>`) cover the whole `config.toml` surface — model,
  provider, endpoint, protocol, key or credential reference, thinking level,
  output format, system prompt, the tool boundary, prompt sources, and the
  memory/checkpoint/compaction/dream tables — and outrank the TOML file while an
  explicit CLI flag still wins. Hooks can be command hooks (`hook add <event>
  --matcher RE --timeout N --command CMD`) or **shell functions named after the
  event** (`PreToolUse() { ...; }`), which are wrapped as command hooks so they get
  the same stdin JSON, exit-code and timeout semantics; both merge into the
  existing trust-gated hook set. Exported variables are applied to the process, so
  provider keys, `<PROVIDER>_BASE_URL` and `PIGO_*` can be set from the script.
  Evaluation is best-effort: no bash, a failing script, an unknown option or a
  timeout prints a warning and never blocks the launch.
- **`question` tool (human-in-the-loop)**: the model can ask the operator a
  question through a tool call and continue with the answer. The TUI shows the
  question just above the composer (digit picks a suggested option, typing plus
  Enter answers, Esc cancels the run); the line REPL prints it and reads a line
  from the shared input, serialized with the tool-confirmation prompt. Runs
  without interactive input (headless `-p`, sub-agents, autonomous goals) get an
  actionable error result telling the model to proceed with a stated assumption
  instead of blocking. Questions always run sequentially so prompts never
  interleave, `--allowed-tools` / `--disallowed-tools` apply as for any tool, and
  the tool is withheld from sub-agents.
- **In-session session restore**: `/resume` lists the stored sessions with
  their age, project, model and first-prompt preview; the TUI opens a picker
  (arrows to move, Enter to load, Esc to cancel) and the line REPL accepts
  `/resume <n>` or `/resume <id>`. Loading swaps the live conversation in
  place — no restart — and replays its history. `pigo --list-sessions` now
  shows the same first-prompt preview.
- **Tool-level admission control**: `--allowed-tools` / `--disallowed-tools`
  (repeatable, comma-separated, case-insensitive) narrow the tool set handed to
  the model, filling the gap between the full set and `--no-tools`. Deny wins
  over allow on conflict; task sub-agents inherit the boundary; an unknown tool
  name aborts with exit code 2 instead of being silently ignored. Because
  filtering happens at the tool-registration layer — before the side-effect
  confirmation gate — `--approve` waives confirmation prompts but cannot widen
  the boundary. Also configurable as `allowed_tools` / `disallowed_tools` in
  `config.toml`.
- **Self-update**: `pigo update` with no package name (or flags-only calls such
  as `pigo update --check`) now upgrades the pigo binary itself to the latest
  GitHub Release, replacing the executable in place (with a `sudo` hint when the
  target path needs elevated permissions). (#465, #466, #468)
- **Startup upgrade hint**: the TUI banner shows the version row and, when a
  newer release is available, a `Run pigo update to upgrade` hint backed by a
  24h cached background release check. (#467)

### Fixed
- **Malformed tool-call arguments are recoverable**: when the model emits a
  tool call whose JSON does not parse (a truncated or corrupted call), the call
  is rejected with an explanatory error result that tells the model to re-issue
  it, the run continues, and the TUI card shows the payload that was actually
  sent. The stored arguments now survive session save/resume verbatim instead of
  coming back as a quoted blob, and the Anthropic request encoding degrades them
  to an empty object rather than `null`, which the API rejects.

### Changed
- **Restored conversations keep their tool activity**: resuming a session
  (startup `--resume` or `/resume`) rebuilds the same rich tool-call cards a
  live run showed — decoded arguments, status, response tree and diff —
  instead of a bare `· name` line.
- **`pigo update` semantics**: a no-argument `pigo update` no longer updates
  every installed package; it now self-updates the binary. Update packages
  individually with `pigo update <name>`. (#468)
- Internationalized the codebase: all user-facing strings and internal comments
  were translated from Chinese to English.
- Documented self-update and the revised `update` semantics in the README and
  the docs site.

## [0.4.3] - 2026-07-31

### Added
- Generic **task tool** for sub-agent fan-out, with a concurrency semaphore and
  a nesting guard; advertised in the system prompt. (#454, #458)
- **Sub-agent progress reporting**: `SubAgentProgressEvent` and a context-carried
  progress emitter (#453), surfaced through a loop-injected emitter (#455),
  printed to stderr in headless mode (#457), and shown in a multi-line sub-agent
  status panel in the TUI. (#456)
- TUI prompt-history navigation, plus sub-agent panel and credential fixes.
- Subagent-orchestration PRD and SPEC.

## [0.4.2] - 2026-07-31

### Added
- **Remote control**: pair a phone/browser with a running REPL over LAN. Includes
  a pairing token and session credential store (#438), LAN address / free-port
  resolver (#439), terminal Unicode QR renderer (#440), HTTP + WebSocket server
  (#441), a REPL bridge seam (#442), a responsive web SPA (#444), and REPL
  wiring with server hardening. (#443, #445)
- `--cwd`/`-C` flag to run pigo as if launched in a given working directory.
- `/think` slash command to switch reasoning effort at runtime.

### Fixed
- Avoid a slice panic when persisting a session after compaction.
- Auto-scroll the TUI to the newest output on submit.

## [0.4.1] - 2026-07-30

### Added
- **User-extensible hooks**: core `internal/hooks` infrastructure (#417), layered
  `ConfigLayer.Hooks` with append-merge (#418), an `InstallHooks` assembly helper
  (#419), and wiring for PreToolUse/PostToolUse (#420), UserPromptSubmit with
  block + one-shot injection (#421), Stop/SubagentStop (#422), SessionStart (#423),
  and a `HookNotifier` for observer events (#424), converged across all six
  drivers (#425).
- Runnable hook examples, README Hooks section, and security notes (#426).
- `config.toml.example` template and a TUI screenshot for the README.

### Fixed
- Bound the hook runner timeout with `WaitDelay` so orphaned grandchildren can't
  block `Run`. (#437)
- Show tool arguments in the TUI tool-card header.

## [0.4.0] - 2026-07-29

### Added
- **Full-screen TUI** (Bubble Tea v2): skeleton with default entry gating (#384),
  theme layer and width-aware rendering (#385), a persistent bottom status bar
  (#386), an AgentEvent→`tea.Msg` bridge (#387), streaming transcript with
  viewport scrolling (#388), rich tool-call cards (#389), multi-line input with
  CJK editing and two-stage interrupt (#390), slash commands with an autocomplete
  popup (#391), and real run-seam binding with session resume/persistence. (#392)
- Startup logo + config splash, image paste (Ctrl+V/Cmd+V), multi-line paste
  collapsed into a placeholder, and mouse-selection copy.
- lipgloss v2 upgrade; slash-command registry sunk into `internal/cli/prompts`. (#393)
- TUI agent PRD and SPEC.

### Fixed
- Numerous TUI rendering fixes: scrollbar visibility and alignment across
  reflows, Shift+Enter newline reliability (with Ctrl+J/Alt+Enter fallbacks),
  CJK/IME input, terminal query-reply leakage into the input box, and blank-line
  turn separation. (#403–#416)

## [0.3.7] - 2026-07-28

### Added
- **Prompt templates**: shell-style arg tokenizer (#344) and expansion engine
  (#332) with tiered slash-command priority (#335), argument-hint frontmatter
  (#334), `--prompt-template`/`--no-prompt-templates` flags (#339), settings-tier
  `prompts` from `config.toml` (#338), project-level `.pigo/prompts` when trusted
  (#337), `~/.pigo/prompts` alongside legacy `~/.pigo/commands` (#336), and
  autocomplete/`/help` listings with argument-hint and source tier. (#340, #341)
- Install prompt packages to `~/.pigo/prompts`. (#342)
- Honor `~/.config/pigo/config.toml` over built-in defaults.

### Changed
- Large CLI refactor: `cmd/pigo/main.go` converged to a thin entry, with the REPL,
  `/status`, `/btw`, `/goal`, headless/subagent paths, pkgcmd, run-assembly,
  provider resolution, config loading, trust glue, and UI helpers each migrated
  into dedicated `internal/cli/*` packages. (#357–#369)

### Fixed
- Include the model in the Anthropic Messages request body.
- Gitignore pigo config files to avoid committing API keys.

## [0.3.6] - 2026-07-27

### Fixed
- Handle Ctrl+D/Ctrl+C as CSI-u reports in the REPL. (#329)
- Allow file tools to reach the advertised skills dir. (#327)
- Align the cursor with wide CJK runes in the line editor.

### Added
- Documented the interactive REPL and slash commands in `index.html`.

## [0.3.5] - 2026-07-27

### Added
- **Multi-line REPL input**: multi-line buffer with a cursor (#313), Shift+Enter
  newline / plain-Enter submit (#314), cross-line cursor movement and
  Home/End/Ctrl+A/E (#315), mid-line editing and line-merging backspace (#316),
  trailing-backslash line continuation (#317), full-line rendering with cursor
  repositioning (#318), and history record/restore. (#319)
- **Model-invoked skills**: name/description validation and
  disable-model-invocation (#301), a `FormatSkillsForPrompt` renderer (#302),
  `<available_skills>` injection into the system prompt (#303), and run-assembly
  wiring. (#304)
- `/status` slash command with env/credentials/telemetry sections and runtime
  config + context rendering, retaining telemetry across runs. (#291–#297)

## [0.3.4] - 2026-07-24

### Added
- **`/btw` side-thread**: command skeleton (#279), multi-turn follow-up and exit
  interaction (#280), bare `/btw` to reopen the most recent side thread (#281),
  and per-thread model/thinking overrides. (#282)
- **`/goal` autonomous mode** (mirrors pi-goal / Claude Code goal). (#278)
- Bundle built-in skills and install them on first run. (#277)
- Browse prior REPL inputs with up/down arrows on a blank line.

## [0.3.3] - 2026-07-24

### Added
- Render assistant replies as Markdown in the REPL. (#276)
- Cycle REPL input suggestions with the up/down arrows. (#275)

## [0.3.2] - 2026-07-24

### Added
- Recent-input suggestions in the REPL. (#274)

## [0.3.1] - 2026-07-24

### Added
- **pi-extension hosting**: an embedded Node host (`pihost.mjs`) to run pi
  extensions (#263), routing extension launchers through it (#264), plus
  `Plugin.CallCommand` / `Manager.Commands` command aggregation (#262) and
  `commands/call` wire types (#268), registering plugin commands as slash
  commands with prompt injection. (#265)

## [0.3.0] - 2026-07-23

### Added
- **Harness engineering**: a generic system-reminder dynamic-context injection
  mechanism (#248), a unified tool-result trimming budget in the tool executor
  (#250), structured harness telemetry collection (#251), and classification
  with bounded retries for tool-execution failures. (#252)
- `--thinking-level` flag with layered config resolution. (#246)
- **Model-name → provider inference**: an inference table (#234) wired into
  `resolveProvider` (#235) and documented in the README/`--help`. (#236)
- Output byte cap and head/tail preview truncation for the bash tool. (#249)
- Harness capability matrix and baseline docs; `internal` package diagrams.

### Fixed
- Model-quality degradation and strict-gateway compatibility issues. (#240–#244)

## [0.2.1] - 2026-07-22

### Added
- Chinese-market providers: Qianfan, Volcengine Ark, DashScope (Bailian), and
  Hunyuan. (#232)
- README architecture overview with the two-layer agent-loop flowchart and the
  runtime layering diagram.

## [0.2.0] - 2026-07-22

### Added
- **Provider registry**: a central registry as the single source of truth (#182),
  provider-derived API-key resolution (#183), a `--provider` flag to select a
  built-in provider (#184), base-URL override precedence (#185), and wiring for
  all OpenAI-protocol (#186) and Anthropic-Messages (#187) providers, with
  special-auth parameter validation. (#188)
- Curated model catalog expanded with 11 providers and curated models. (#189)
- **The companion book** *Writing the pi Agent in Go*: `book/` scaffolding with a
  pandoc + xelatex + ElegantBook build pipeline (#200), a full 10-chapter outline
  (#201), the introduction (#202), all chapters 1–10 and the afterword
  (#204, #212–#221), sample figures, and a PDF pipeline with SVG embedding. (#205)

### Changed
- Renamed `manual.html` to `index.html` and made the docs site bilingual (zh/en).

## [0.1.2] - 2026-07-20

### Added
- One-click install script (`install.sh`).
- REPL tool-status line coloring: green on success, red on failure.

## [0.1.1] - 2026-07-20

### Added
- CI GitHub Action and goreleaser support.

## [0.1.0] - 2026-07-20

Initial public release. pigo lands as a working Go re-implementation of the pi
coding agent.

### Added
- **Agent core**: an `EventStream` mechanism (#18), a tool registry with JSON
  Schema validation (#19), streaming backfill (#20), three-phase tool execution
  (#21), parallel & sequential batch tool execution (#22), and the two-layer
  agent loop `runLoop`. (#23)
- **Provider layer**: a unified `Provider` interface with a dual failure model
  (#25), a shared transport driver (SSE + retry + dual watchdogs) (#26),
  three-stage thinking normalization (#27), decoders for Anthropic Messages
  (#63), OpenAI-compatible (#29), and Gemini (#30), and the first concrete
  providers — Bedrock/OpenRouter/Ollama. (#33)
- **Built-in tools**: `read` (#34), `write` (#35), `edit` with diff (#36),
  `bash` with streaming/timeout/cancellation (#37), `grep`/`find`/`ls` honoring
  `.gitignore` (#38), `todo` task tracking (#127), and `webfetch`. (#128)
- **Model & auth**: a model registry and provider directory (#31), plus OAuth
  and API-key resolution. (#32)
- **Security**: an in-process sandbox gate and secret redaction. (#44)
- **Run modes**: a headless/stdio mode and the `pigo` CLI entry (#39),
  headless/stream-json runs carrying a session id with resume support (#176),
  and `--system-prompt` / `--append-system-prompt`. (#180)
- **System prompt & config**: system-prompt assembly with `AGENTS.md` injection
  (#40) and a layered configuration system. (#42)
- **Sessions**: local JSONL persistence and resume (#43); a session tree with
  id/parentId and v3 migration (#121); `/fork`, `/clone` (#122), `/tree`
  navigation (#123), `/export`/`/import` (#124), and `/copy` + `/session`. (#125)
- **Context compaction**: token accounting and trigger detection (#117),
  `FindCutPoint` (#118), summary generation with on-disk `CompactionEntry` (#119),
  and the `/compact` command with automatic loop compaction. (#120)
- **Extensibility**: sub-agent orchestration, skills, and slash-commands (#45);
  a plugin system with a subprocess JSON-RPC 2.0 protocol (#132) and lifecycle
  event subscription (#133); project trust (#134); and process-isolated
  sub-agent mode. (#135)
- **Package manager**: a lockfile and install-dir conventions (#154), `npm:<name>`
  reference parsing (#155), npm detection and content fetch (#156), pi package
  classification (#157), distribution of extensions (#158), skills (#159),
  prompts/commands (#160), and themes (#161), plus `pigo install` (#162),
  `list` + `uninstall` (#163), and `update`. (#164)
- **Interactive TUI** (bubbletea v2) with a lipgloss theme layer, width-aware
  rendering, Markdown output, streaming spinners, tool-call cards, viewport
  scrolling, role-styled transcripts, action slash-commands, and a model picker.
  (#41, #89–#95, #103)
- Image input as content blocks with two-provider encoding. (#126)
- Switched to `spf13/pflag` (#87) and split `internal/agent` into `agentcore`,
  `provider`, `agenttool`, and `runtime` leaf packages with a verified layering
  DAG. (#74–#79)

[Unreleased]: https://github.com/smallnest/pigo/compare/v0.4.3...HEAD
[0.4.3]: https://github.com/smallnest/pigo/compare/v0.4.2...v0.4.3
[0.4.2]: https://github.com/smallnest/pigo/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/smallnest/pigo/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/smallnest/pigo/compare/v0.3.7...v0.4.0
[0.3.7]: https://github.com/smallnest/pigo/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/smallnest/pigo/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/smallnest/pigo/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/smallnest/pigo/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/smallnest/pigo/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/smallnest/pigo/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/smallnest/pigo/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/smallnest/pigo/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/smallnest/pigo/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/smallnest/pigo/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/smallnest/pigo/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/smallnest/pigo/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/smallnest/pigo/releases/tag/v0.1.0

