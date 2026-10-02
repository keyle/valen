# Tasks: sessions

- [x] 0. Ctrl+L `/sessions` overlay lists titles (first prompt, 60 runes,
  single-line) with model/effort; Enter resumes (transcript + history
  rebuilt). Tool-call log moved to Ctrl+U (`/tools`). Titles + model/effort
  stored in `<id>.jsonl.meta.json` sidecar (JSONL append-only untouched) and
  per-turn `model`/`effort`; old JSONLs backfill titles on first list.
  `TestSessionTitleCapturedOnce`, `TestTUISessionsListShowsTitlesAndResumes`.
- [ ] 1. Flags `--session/--continue/--fork` + 60-rune title capture;
  fork copies history, new ID. `TestSessionFlags` on seeded JSONLs.
- [ ] 2. `/sessions` overlay (resume/fork) reusing history view +
  `listSessions()`. `TestTUISessionResumeFork`.
- [ ] 3. Docs: SPEC-tui + help + usage text; gate
  (`gofmt`, `go vet`, `go test ./...`).
