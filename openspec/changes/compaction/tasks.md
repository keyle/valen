# Tasks: compaction

- [ ] 1. `compactSession` + `[compaction]` config + runes/4 estimator.
  `TestCompactKeepsRecent` (summary + keep window, idempotent update).
- [ ] 2. `/compact` (`/summarize`) + auto on too-long 400 / pre-send
  estimate; kept window shortens tool output + attachment stubs.
  `TestCompactAutoRetry` via fake 400-then-200 server.
- [ ] 3. Docs: SPEC + help + `config.toml` comments; gate
  (`gofmt`, `go vet`, `go test ./...`).
