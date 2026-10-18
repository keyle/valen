# Tasks: shell-approval

- [ ] 1. `[shell] prompt` config key (`outside`/`always`/`never`) + mode
  selection in `gateShell`; `never` boot warning. `TestShellPromptPolicy`.
- [ ] 2. Approval event fields on `Event`, emit from `approveShell` and the
  `checkFileAccess` approval path only; headless stderr line.
  `TestApprovalEventEmitted`, `TestHeadlessApprovalStderr`.
- [ ] 3. TUI: pending approval lines, commit into `Turn.Approvals` (JSONL),
  dim render, resume. `TestApprovalLineCommitted`.
- [ ] 4. Question gains the `Scope:` line; extend destructive labels
  (`git push --force`, `chmod -R`, `curl|sh`). `TestShellLabelExtend`.
- [ ] 5. Docs: SPEC (mode table + recommendation), SPEC-tui (approval lines),
  `config.toml` comments; caps into `project.md`; gate (`gofmt -l .`,
  `go vet ./...`, `go test -race ./...`).
