# Tasks: perf-parity-opencode

Done: parallel tool loop (§1), parallel MCP load + shared transport (§2),
worker-pool grep/glob, `rg` deferred (§3), prompt determinism + TUI render
cache + sync-append kept (§4.1–4.3). Locked by `TestRequestEncodingStable`,
`TestRunToolCallsParallel`, `TestGrepParallelMultiFileSortedAndCapped`.

- [ ] 4.4 Bounded history/compaction: deferred to a separate openspec. No silent
  truncation.

## 5. Gate

- [ ] 5.1 Re-run depth + memory fixtures, confirm latency down, RSS ~baseline (2% story intact).
- [ ] 5.2 Update `SPEC.md` / `SPEC-tui.md` / `config.toml` comments if behavior
  or keys changed.
- [ ] 5.3 Final: `gofmt -l .` clean, `go vet ./...` green, `go test ./...` green
  (`-race` for 1-4).
