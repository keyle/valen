# Tasks: perf-parity-opencode

Ordered, small, each verifiable. Stop after any task if it regresses memory.

## 0. Baseline (measure first)

- [ ] 0.1 Add depth/latency notes fixture: N-turn history, record per-round
  request bytes (`json.Marshal` size), TUI render count, `session.append` time.
  Verify: `go test -run 'TestDepthBaseline|TestPayloadSize' .` (new, offline).
- [ ] 0.2 Record memory baseline on same fixture (`-benchmem` or `/usr/bin/time -l`
  RSS): tool-output 16k path + 100-match grep path. No code change.

## 1. Parallel tool loop (do first, biggest win)

- [ ] 1.1 `runChatTurn` bounded parallel dispatch (sem 8, order-preserving
  history append, ordered `tool_start/result` events). `AskUserQuestion` serial.
  Verify: `go test -run 'TestChatTurn|TestRunTurn' .` + new parallel-order test, `-race`.
- [ ] 1.2 Same for `runMessagesTurn`, `runResponsesTurn` (share helper, keep
  UI-agnostic). Verify: `go test -run 'TestMessages|TestResponses|TestTransports' . -race`.
- [ ] 1.3 Document ordering + `AskUserQuestion` exception in `SPEC.md` agent-loop
  section. Verify: `gofmt -l .`, `go vet ./...`.

## 2. MCP + HTTP

- [ ] 2.1 Parallel `LoadMCP` startup (fail-soft per server preserved).
  Verify: `go test -run 'TestMCP|TestLoadMCP' . -race`.
- [ ] 2.2 Single shared tuned `http.Transport` for chat + `search.go`; keep
  `User-Agent` + `x-opencode-session` stamping. Verify: `go test -run
  'TestSearch|TestWebFetch|TestStream' .`.
- [ ] 2.3 Only if 2.2 shows tail: per-host timeout / retry audit. Otherwise skip.

## 3. Local file speed (keep single-binary)

- [ ] 3.1 Worker-pool `grep` (early exit 100, keep skips/caps). Verify:
  `go test -run 'TestGrep|TestGlob|TestFiles' . -race`.
- [ ] 3.2 Worker-pool or batched `glob` + `matchGlob` fast-path. Same verify.
- [ ] 3.3 Optional `rg --json` fast-path when on `PATH`, Go fallback otherwise.
  Behind helper, default off unless measured >2x win. Verify: same + offline
  `rg`-absent path.

## 4. Depth degradation (do after parallelism)

- [ ] 4.1 Stable prompt prefix: audit `splitChatHistory` / `splitResponsesInput`
  / `applyThinkingToRequest` for per-round churn; keep deterministic tool order.
  Verify: request-bytes fixture from 0.1 grows linearly; `go test -run
  'TestMessages|TestResponses|TestModels|TestDepth' .`.
- [ ] 4.2 TUI incremental render: cache committed turns, append-only pending,
  throttle tool events like deltas. Verify: `go test -run 'TestTUI|TestStreamTimeline' .`.
- [ ] 4.3 Async `session.append` + `usage`/`models` caching/dedup (off UI thread).
  Verify: `go test -run 'TestSession|TestUsage|TestModels' . -race`.
- [ ] 4.4 Only if still superlinear: bounded history/compaction proposal
  (separate openspec). Do not silently truncate.

## 5. Gate

- [ ] 5.1 Re-run depth + memory fixtures, confirm latency down, RSS ~baseline (2% story intact).
- [ ] 5.2 Update `SPEC.md` / `SPEC-tui.md` / `config.toml` comments if behavior
  or keys changed.
- [ ] 5.3 Final: `gofmt -l .` clean, `go vet ./...` green, `go test ./...` green
  (`-race` for 1-4).
