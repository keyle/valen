# TODO — fullscreen loop UI (SPEC-tui.md)

## Parity (6 tracks — each has a small openspec in `openspec/changes/<track>/`)

Ordered by ROI per lightness cost. Each item ends in a verifiable result.
Build in track order; stop a track if it needs a new runtime dep or
regresses `project.md` caps.

- [ ] A. Attachments (`attachments/`): text/dir/image resolve (20 MiB/item,
  bytes-sniffed, unsupported rejected) → wire on all 3 transports as shared
  `<attachment>` text + image parts → TUI `@` picker + chips + `[image]`
  placeholder → headless `-f/--file` + `@path` expand.
  Verifiable: offline converter tests per transport + cap/reject paths.
- [ ] A.1 `Attachment` resolve + `[attachments]` config defaults
  (`attach_test.go` fixtures: txt, dir, tiny PNG, oversize reject).
- [ ] A.2 Wire parts: chat MultiContent, messages image blocks, responses
  input_image; `read` image path with same limits (`TestAttachmentWire*`).
- [ ] A.3 TUI `@` fuzzy picker + composer chips; headless `-f` repeatable.
  (`TestTUIAttachChip`, `TestHeadlessFileFlag`).

- [ ] S. Sessions (`sessions/`): TUI-only resume/fork without a server.
  Verifiable: `/sessions` overlay resume/fork on seeded JSONLs.
  (No headless `--session/--continue/--fork` — dropped.)
- [x] S.1 Title capture (first prompt, 60 runes); JSONL stays
  append-only; Ctrl+L `/sessions` lists titles with model/effort,
  Enter resumes; tool-call log moved to Ctrl+U (`/tools`).
  (`TestSessionTitleCapturedOnce`,
  `TestTUISessionsListShowsTitlesAndResumes`.)
- [ ] S.2 `f` forks in overlay (copy history under a new ID).
  (`TestTUISessionResumeFork`.)

- [ ] P. Permissions (`permissions/`): ordered `allow/ask/deny` + patterns,
  defaults preserve today (`default allow`, `external_directory ask`).
  Verifiable: pure evaluator table test + once/always/reject via stubbed
  `askPrompter` + triple-repeat fixture.
- [ ] P.1 Evaluator (last-match-wins, `deny` hard-stops) + TOML table
  (`shell`, `read:*.env`, `mcp_*` wildcards).
- [ ] P.2 `ask` via `promptUser` (once/always/reject); `always` appends to
  git-ignored `.valen/approvals.toml`, never overrides `deny`.
- [x] P.3 `doom_loop`: 3× identical call → guided ask instead of 4th run.

- [ ] F. File tools (`file-tools/`): `read` lists dirs, atomic `patch`,
  `gofmt` hook, grep/glob opts. All under existing caps (16k/100/1MiB).
  Verifiable: `files_test.go` additions, parse-fail-writes-nothing test.
- [ ] F.1 `read` on dir → non-recursive listing (cap 100), not an error.
- [ ] F.2 `patch {patchText}` two-phase (parse→validate all→write all or
  none); permission resource = every path under `edit`.
- [ ] F.3 Post-write formatter hook (`gofmt -w` if on PATH else no-op,
  `[format]` toggle); `grep literal/caseSensitive/limit`, `glob hidden/limit`.

- [ ] W. Workflow tools (`workflow-tools/`): phased, foreground-only.
  Verifiable: each tool's round trip offline, no new files for todos.
- [ ] W.1 `todowrite`/`todoread` (in-memory + session fold, opencode schema).
- [ ] W.2 `skill` loader + discovery (`~/.config/valen/skills`,
  `.valen/skills`, `.opencode/`+`.claude/` compat) + `skill` permission.
- [ ] W.3 `task` foreground-only (`general`/`explore` policies, depth ≤2,
  shared `mcpMu`); background deferred.

Gate per track: SPEC + help + `config.toml` comments together;
`gofmt -l .` clean, `go vet ./...` + `go test ./...` green
(`-race` for parallel/approval paths).

