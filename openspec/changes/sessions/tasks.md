# Tasks: sessions

- [ ] 1. Flags `--session/--continue/--fork` + 60-rune title capture;
  fork copies history, new ID. `TestSessionFlags` on seeded JSONLs.
- [ ] 2. `/sessions` overlay (resume/fork) reusing history view +
  `listSessions()`. `TestTUISessionResumeFork`.
- [ ] 3. Docs: SPEC-tui + help + usage text; gate
  (`gofmt`, `go vet`, `go test ./...`).
