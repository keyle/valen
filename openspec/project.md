# valen openspec — conventions

Single-binary Go agent harness. No server/client split, everything in-process in Bubble Tea.

## Source of truth

- `SPEC.md` + `SPEC-tui.md`: behavior, provider/tool/MCP contracts.
- `LIFECYCLE.md`: narrative guide — turn/round lifecycle, token math, sizes, compaction, batching, tools, todos vs task.
- `config.toml`: example config + comments.
- `AGENTS.md`: fast loop, minimal diffs, shared `runTurn` stays UI-agnostic.

## Constraints (all changes must keep)

- Single binary, stdlib-first. No new runtime deps without explicit approval.
  Current direct deps: `toml`, `go-openai`, `mcp-go`, `x/net/html` + charm TUI stack.
- Memory is a feature (historical target ~2% of OpenCode, not a fresh benchmark). Keep streaming caps:
  `8MiB LimitReader` per round, `16k` tool-output truncate (storage/log),
  no secondary live-send truncation; rolling `32768`-byte UTF-8-safe TUI
  text/thinking tails, exact delta concatenation, ~60ms render throttle,
  committed-scrollback line budget `[tui] max_lines` default `1000` (trims
  reasoning then old turns, keeps prompts/tools; `0` disables),
  `32768`-byte cap on the sideloaded `~/.config/valen/AGENTS.md`,
  `100` grep matches, `1MiB/512KiB` search/fetch caps, `8000` fetch runes,
  `@` file picker index `20000` entries / `200` fuzzy choices / `8` visible rows,
  `16384` output tokens default on all three transports (positive explicit
  overrides honored, no floor). Keep compaction caps: `15k` soft whole-turn
  tail target, entire current turn protected, `10%` buffer, early threshold
  at `keep*3`, `2000`-char ordinary tool output inside summarizer payload
  (`4000` for decisions/assistant text), latest `2` genuine user turns intact
  plus `40k` older tool-output token shield. Beyond that shield, reads become
  descriptors and ordinary outputs shorten to `2000` chars; decisions never
  prune. A lone oversized current turn cannot compact and may fail fast.
  Estimation: runes/4 (including Chat native reasoning) + 40 envelope tokens
  per message + full tool schemas. Read: 2000 lines default, max 5000,
  ~12KB/call, 1000 chars/line; grep: 100 matches, 20/file, skip >1MiB/binaries
  and dependency/build dirs. Messages has 3 ephemeral cache breakpoints;
  Chat/Responses rely on unchanged server-side prefixes.
- Execution caps: `300` main rounds per window with one compact-and-continue
  extension; `25` consecutive identical ordered
  batches allowed, single-call third-repeat guard; max `8` parallel known
  read-only builtins plus max `5` parallel `task` subagents (extras queue).
  Any other tool (including MCP) makes a batch ordered serial. Result Events are completion-ordered, history request-ordered.
  Shell `60s`; MCP handshake `30s`, call `2m`; task max `25` rounds/depth `1`.
  Each HTTP attempt including body: `request_timeout_seconds=300` default,
  header `60s`, dial/TLS `10s`; transient retry max `3`, backoff `300/800ms`,
  no blocking retry-body drain or new dependency. No idle/whole-turn timeout.
- Native Messages signatures/blocks and Responses encrypted reasoning/phases
  replay only inside the active transport loop. They are not durable session
  metadata; Chat native reasoning is checkpointed. Disk keeps all appended
  `Turn.History` checkpoints, memory only the latest; failures retain work,
  Event-captured tools and legacy tool reconstruction preserve resume state.
- Offline tests only: `httptest` fakes, no real network. Never hand-edit `go.sum`.
- `runTurn` in `main.go` shared by headless (`-p`) and TUI: keep UI-agnostic.
  `runModelTurn` owns policy with no synthetic nudges; transports own replay.
  UI state lives in `tuiModel` (`tui.go`) only. Task inherits current config
  and `effectiveSystem` plus kind instructions, returns only visible answer.
- Sampling/thinking params omitted unless configured (pointer/empty semantics).
- Loop: targeted `go test -run 'TestName' .` while iterating, final gate
  `gofmt -l .` + `go vet ./...` + `go test ./...` (`-race` for concurrency).
