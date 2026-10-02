# Tasks: perf-parity-opencode

Ordered, small, each verifiable. Stop after any task if it regresses memory.

## 0. Baseline (measure first)

- [x] 0.1 Determinism fixture instead of throwaway notes: `TestRequestEncodingStable`
  locks identical history+registry to identical request bytes (chat, messages,
  responses converters), so cache affinity can't regress via churn.
- [x] 0.2 Memory baseline: existing caps unchanged (`8MiB` round body, `16k` tool
  truncate, `100` grep matches, `1MiB/512KiB` search/fetch, `8000` fetch runes);
  new caches bounded (8-worker output slices, one rendered-transcript copy).

## 1. Parallel tool loop (do first, biggest win)

- [x] 1.1 `runChatTurn` bounded parallel dispatch (sem 8, order-preserving
  history append, ordered `tool_start` then `tool_result` events). `AskUserQuestion`
  batches fall back to serial. Covered by `TestRunToolCallsParallel` (3×200/300ms
  under the 600ms sequential bound) + `TestRunToolCallsAskSerial`, `-race` green.
- [x] 1.2 Same for `runMessagesTurn`, `runResponsesTurn` via shared `runToolCalls`
  helper (UI-agnostic, in `main.go`). MCP calls serialize on `mcpMu` (one stdin
  pipe per server); file-approval/`AskUserQuestion` prompts serialize on `askMu`.
- [x] 1.3 Documented ordering + `AskUserQuestion`/MCP exceptions in `SPEC.md`
  agent-loop section.

## 2. MCP + HTTP

- [x] 2.1 Parallel `LoadMCP` startup (spawn+handshake+list in parallel, serial
  registration in config order so tool order stays deterministic; fail-soft per
  server preserved). `TestMCP|TestLoadMCP` green with `-race`.
- [x] 2.2 Single shared tuned `http.Transport` (`MaxIdleConns 100/20`, 90s idle)
  for chat + `search.go` web fetch; `User-Agent` + `x-opencode-session` stamping
  unchanged. `TestSearch|TestWebFetch|TestStream` green.
- [x] 2.3 Skipped: no slow-host tail observed; 30s fetch / 60s shell / 2m MCP
  timeouts kept as-is.

## 3. Local file speed (keep single-binary)

- [x] 3.1 Worker-pool `grep` (8 workers over walked candidates, early cap at 100,
  `.git`/binary/`>1MiB` skips kept, output sorted by file then line).
  `TestGrep|TestGlob|TestFiles` green with `-race`, plus
  `TestGrepParallelMultiFileSortedAndCapped`.
- [x] 3.2 Parallel `glob` (top-level subtrees walk concurrently, results sorted).
- [x] 3.3 `rg --json` fast-path DEFERRED: worker-pool grep keeps the strict
  single-binary contract with no exec dependency; revisit only with a measured
  >2x win on a real corpus. Go fallback is the implementation.

## 4. Depth degradation (do after parallelism)

- [x] 4.1 Stable prompt prefix audit: converters (`splitChatHistory`,
  `splitResponsesInput`, `applyThinkingToRequest`) are already deterministic —
  no churn found. Locked by `TestRequestEncodingStable`. Remaining per-turn
  growth is history itself (LLM tokens), addressed only by 4.4.
- [x] 4.2 TUI incremental render: committed turns cached append-only
  (`TestChatCacheIncrementalMatchesFull`); tool start/result events share the
  60ms delta throttle. `TestTUI|TestStreamTimeline` green.
- [x] 4.3 `session.append` deliberately kept synchronous: it writes one turn per
  call (O(turn), not O(history)) and ordering guarantees back recall tests.
  Usage/model polling deduped instead (`usageInflight`: tick + post-turn never
  overlap). `TestSession|TestUsage|TestModels` green with `-race`.
- [ ] 4.4 Bounded history/compaction: deferred to a separate openspec. No silent
  truncation.

## 5. Gate

- [ ] 5.1 Re-run depth + memory fixtures, confirm latency down, RSS ~baseline (2% story intact).
- [ ] 5.2 Update `SPEC.md` / `SPEC-tui.md` / `config.toml` comments if behavior
  or keys changed.
- [ ] 5.3 Final: `gofmt -l .` clean, `go vet ./...` green, `go test ./...` green
  (`-race` for 1-4).
