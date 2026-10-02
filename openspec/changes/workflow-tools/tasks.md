# Tasks: workflow-tools

- [ ] 1. `todowrite`/`todoread` (memory + session fold, opencode schema).
  `TestTodoRoundTrip`.
- [ ] 2. `skill` discovery + loader + `skill` permission action.
  `TestSkillDiscoveryLoad` (compat dirs, missing-ID error, ≤10 paths).
- [ ] 3. `task` foreground nested `runTurn` (`explore` fence, `general`
  full-minus-task, depth ≤2, serial interactive). `TestTaskExploreFence`,
  `TestTaskDepthCap`; `-race` green.
- [ ] 4. Docs: SPEC + `config.toml` comments + tool list; gate
  (`gofmt`, `go vet`, `go test ./...`).
