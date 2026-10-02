# Design: perf-parity-opencode

## 1. Why deeper tasks get slower (current)

- **Per-round full re-encode:** `runChatTurn (main.go:274)`,
  `runMessagesTurn (messages.go:288-306)`, `runResponsesTurn (responses.go:293-311)`
  rebuild `Tools()` + `splitChatHistory (messages.go:116-155)` /
  `splitResponsesInput (responses.go:105-153)` + `json.Marshal(history+tools)`
  in `postAPI (stream.go:21-43)` every round. Cost per turn grows with history.
- **Full tool schema resend:** 8 tools always sent (`config.toml:17`,
  `tools.go:60-67`, `messages.go:86-97`, `responses.go:76-87`).
- **Sequential tool block:** each `registry.Run (tools.go:78-84)` waits for the
  prior `shell 60s (tools.go:18)` / `fetch 30s (search.go:28)` / `MCP 2m (mcp.go:19)`.
- **Serial local scan:** `glob/grep (files.go:330-348,401-444)` walk + read +
  regexp one file at a time, skip `>1MiB`, cap 100 — tail file blocks all.
- **TUI `O(history)` per delta:** `renderChat: for session.Turns: renderTurn +
  glamour.Render (tui.go:1455-1537,606-619)`, `renderPending (1578-1646)`
  string concat per `text_delta`; `tool_start/result → refreshViewport+GotoBottom
  (956-979)` unthrottled vs deltas throttled 60ms (545-554).
- **Sync disk + poll on path:** `session.append: Marshal+Open+Write+Close
  (session.go:67-85)` on UI thread (`tui.go:1068-1073`); `listSessions Stat+sort
  (session.go:111-138)` per overlay open; `GET /usage (usage.go:35-55)` on same
  `hc` as chat; `ListModels 15s (models.go:135-148)` blocks picker.
- **Cache-hostile prefix:** `responses.go:299 store:false` stateless replay,
  `newSessionID per run (main.go:109-111)`, thinking churn (`models.go:72-132`),
  `max_rounds=9999 (config.toml:14)` + `thinking=high (config.toml:4)` inflate
  reasoning tokens with no compaction. LLM itself slows as input tokens grow.

## 2. Approach (memory-bounded)

### A. Bounded parallel tool dispatch (biggest win)

- `sync.WaitGroup` + semaphore (e.g. 8), stdlib only, in all three
  `run*Turn` loops. Preserve request order when appending `history`.
- Classification: `read/glob/grep/web_search/web_fetch/shell/MCP-readonly`
  parallel; `AskUserQuestion (ask.go:63,74)` + `checkFileAccess prompts
  (files.go:84-116)` stay serial (they block on user).
- `onEvent tool_start/result` must remain ordered for TUI timeline; collect
  outputs then emit in index order.
- Memory guard: existing `truncate 16k` stays; cap concurrent large outputs
  via semaphore, no output buffering beyond current caps.

### B. MCP + HTTP pooling

- `LoadMCP (mcp.go:27-38)`: `WaitGroup` over `registerMCPServer`, per-server
  fail-soft preserved. Keep `30s` handshake as outer bound, consider shorter
  per-server fail-fast only if flaky servers observed.
- Single shared tuned `http.Transport` (`MaxIdleConns`, `IdleConnTimeout`,
  keep-alive) for chat + `search.go` fetch; remove split `httpClient` pool.
  Shorter per-host timeout only if measured slow-host tail exists.
- No new deps (`x/sync/errgroup` optional; `WaitGroup` suffices).

### C. Local search without losing single-binary

- Phase 1 (no dep): worker-pool `WalkDir` (fixed workers, early exit at 100),
  keep `.git`/binary/`>1MiB` skips. `read` keeps whole-file `ReadFile+Split`
  but stays under current limits.
- Phase 2 (opt-in): if `rg` on `PATH`, use `rg --no-config --json` for
  `grep/glob`, streaming parse, else Go fallback. Never require `rg`.
- Respect `.gitignore` only if cheap; otherwise keep current `.git`-skip.

### D. Depth degradation fixes

- **Stable prompt prefix:** keep tool order deterministic (already), split
  system into stable vs dynamic, avoid resending unchanged `thinking` toggles,
  keep `x-opencode-session` affinity. Do not add cache directives the backend
  doesn't document.
- **Smaller payloads:** audit per-round bytes (history JSON + tools JSON);
  drop `redacted_thinking`/empty blocks already done, avoid duplicate system
  joins. History pruning/compaction is follow-up, not this change, unless depth
  test forces it — then propose explicit `/clear`-safe truncation policy.
- **Incremental TUI:** cache committed-turn renders, append-only pending block,
  throttle `tool_start/result` like deltas, pin viewport without full
  `SetContent` per delta.
- **Async I/O:** move `session.append` + `usage` poll + `listSessions` off
  critical path (background `tea.Cmd`, dedup while `busy`, cache models/usage).

## 3. Alternatives considered

- Unbounded goroutines / `concurrency:"unbounded"`: rejected, breaks 2% memory.
- SQLite/WAL store, subagents, Code Mode JS sandbox: rejected, out of scope.
- Mandatory `rg` dependency: rejected, violates single-binary; opt-in only.
- Full-history markdown cache: rejected unless bounded LRU with measured win.

## 4. Verification

- Offline fake-server tests for ordering + parallelism timing (generous bounds,
  `-race`).
- Depth fixture: synthetic history of N turns, assert per-round marshal/render
  bytes grow ~linearly; RSS/alloc benchmark before/after.
- Existing caps test: `tools_test.go`, `files_test.go`, `search_test.go`,
  `stream_test.go` unchanged behavior.
