# Tasks: permissions

- [ ] 1. Ordered evaluator + TOML table (shorthand + `tool:pattern` keys +
  wildcards). `TestPermissionEval` table (allow/ask/deny, last-wins).
- [ ] 2. `ask` via `promptUser` (once/always/reject); `always` persists to
  `.valen/approvals.toml`, never overrides `deny`.
  `TestPermissionApprovals` with stubbed prompter.
- [x] 3. `doom_loop` triple-repeat guard (`doomLoopTool`/`doomLoopError` +
  `checkContextBudget`; `TestDoomLoopToolShapes`, `TestRunTurnStopsDoomLoop`).
- [ ] 4. Docs: SPEC + `config.toml` comments (incl. recommended `shell ask`
  note); gate (`gofmt`, `go vet`, `go test -race ./...`).
