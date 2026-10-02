# Tasks: compaction

- [x] 1. `compactSession` + `[compaction]` config + runes/4 estimator.
  `TestCompactKeepsRecent` (summary + keep window, idempotent update).
- [x] 2. `/compact` (`/summarize`) + auto on too-long 400 / pre-send
  estimate; kept window shortens tool output + attachment stubs.
  `TestCompactAutoRetry` via fake 400-then-200 server.
- [x] 3. Docs: SPEC + help + `config.toml` comments; gate
  (`gofmt`, `go vet`, `go test ./...`).

Shipped: `compaction.go` (prune + cheap-model summary via the shared
`runTurn`, all three transports), auto-compact + one 400-retry in `runTurn`,
TUI `/compact` (`compaction_test.go` green). `[compaction] model` selects the
cheap summarizer (empty = session model at minimal thinking).
