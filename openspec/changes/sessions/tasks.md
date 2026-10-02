# Tasks: sessions (TUI-only; no headless/terminal flags)

- [x] 0. Ctrl+L `/sessions` overlay: titles (first prompt, 60 runes) +
  model/effort; Enter resumes. Sidecar `<id>.jsonl.meta.json`, JSONL
  append-only; tools log moved to Ctrl+U. (`TestSessionTitleCapturedOnce`,
  `TestTUISessionsListShowsTitlesAndResumes`.)
- [ ] 1. `f` forks in overlay (copy history, new ID). `TestTUISessionResumeFork`.
- [ ] 2. Docs + gate (`gofmt`, `go vet`, `go test ./...`).
