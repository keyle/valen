# Tasks: workflow-tools

- [x] 1. `todowrite`/`todoread` (memory + session fold, opencode schema).
  `TestTodoRoundTrip`.
- [ ] 2. `skill` — moved to `openspec/changes/skill-tool/`.
- [x] 3. `task` foreground nested `runTurn` (`explore` fence, `general`
  full-minus-task, depth 1, parallel max 5).
  `TestTaskExploreIsReadOnly`, `TestTaskDelegatesViaLoop`,
  `TestRunToolCallsParallel`; `-race` green.
- [ ] 4. Docs: SPEC + `config.toml` comments + tool list; gate
  (`gofmt`, `go vet`, `go test ./...`).
