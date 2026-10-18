# Tasks: attach-input

Depends on `attachments/` (resolver + transport parts). Do not start task 4
before task 2 of that change is green.

- [x] 1. Token registry in `tuiModel` (`[img-N]`, `[basename]`), chip line
  above the chatbox, reconciliation at submit. `TestTokenReconcileDropsDeleted`.
- [x] 2. Paste-path detection in `tui_key.go` (newline split, quote/`file://`
  strip, all-paths rule; mixed text untouched).
  `TestPastePathBecomeTokens`, `TestPasteMixedTextUnchanged`.
- [x] 3. Clipboard bridge (`clipboard.go`): `execCommand` var, darwin/
  wayland/x11 commands, 3s timeout, sniffing, content-addressed cache +
  7-day prune. `TestClipboardImageProbe`, `TestClipboardOversizeRejected`,
  `TestAttachCachePrune`.
- [x] 4. Wire pending attachments into `submit` → resolver → turn; offline
  request-body test. `TestAttachTokensReachRequestBody`.
- [x] 5. Docs: SPEC-tui (tokens, ctrl+v, chip line, terminal caveats),
  README limitations (clipboard tool per OS), `[attachments]` cache note;
  caps into `project.md`; gate (`gofmt -l .`, `go vet ./...`,
  `go test ./...`).
