# TODO — fullscreen loop UI (SPEC-tui.md)


## Later (D — speedups not taken this round)

- [ ] D1 `Retry-After` aware backoff: honor the header on 429 instead of
  fixed 300/800ms; provider-aware retry policy like opencode's
  `SessionRetry.policy`.
- [ ] D2 full `.gitignore` parsing in grep/glob (current fixed skip list is
  the 80% baseline) + `rg` fast path when on PATH.
- [ ] D3 background shell: `run_in_background` + poll/kill tools so dev
  servers and long builds don't park the loop.
- [ ] D4 A4 follow-up: native reasoning replay if the provider ever exposes
  reasoning objects/IDs on responses SSE; until then summaries fold as
  `<think>` (status quo).
- [ ] D5 B1 follow-up: messages-only mid-stream execution at
  `content_block_stop` (registry callback into the SSE scan + merge
  pre-executed results in `runMessagesTurn` instead of re-running).
- [ ] D6 TUI subagent streaming: forward subagent `tool_start`/`text_delta`
  events into the pending block instead of today's silent single `task` row.
- [ ] D7 thinking-block signature capture on messages (replay native
  thinking blocks for cache affinity) instead of `<think>` text folding.

## Minor, UX

- [ ] the content of the chat scrollback is not all aligned. Some have a space in front, or two.
- [ ] thinking messages (white) in scrollback would benefit from a timestamp of when they happen, e.g. message (1m3s) <-- when they occurred.
- [ ] ability to paste a paragraph
- [ ] shift+enter should insert a line return in the chatbox, it does not (yet!) - fix.
- [ ] when working, page up/down temporarly jumps and then gets smashed back down.
- [ ] related: the wheelmouse should scroll the scrollback, it does not. add this. Scrolling up should go through the history.
- [ ] show live errors such as backend overloaded, rate limiting, etc. in the thinking but in red?
- [ ] when 'working' we want the user to press ESC twice to stop, 1 can be hit by mistake. Add a debounce and check for 2 ESC key sequential within 2 seconds, otherwise go back to working state. Do not interrupt the process until 2 ESC confirmed.
- [ ] sandboxing permission, when working in a git worktree can lead to asking tons of permission, we should remember access given to a folder and not ask again, per session.

## Medium

- [ ] M.2 are different sessions trully independent? I feel running more than 2 can lead to a major slowdown, maybe backend is doing it?
- [ ] M.3 we need Plan mode! Something we don't have yet.

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

