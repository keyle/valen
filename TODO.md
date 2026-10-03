# TODO — fullscreen loop UI (SPEC-tui.md)

## Perf findings (A–C vs opencode, same model/settings)

Verdict: no single bug — six compounding gaps, all pushing toward
`rounds × history` quadratic sends. Old caps are ditched where noted.

- [x] A1 no prompt caching. Zero `cache_control` sent; `responses` replays
  stateless (`store:false`, no `previous_response_id`); per-round prune
  churned bytes and defeated server-side prefix cache.
  DONE: 3 ephemeral breakpoints on messages (system, last tool, last
  block); chat/responses rely on server prefix cache over now-stable bytes.
- [x] A2 full 16k tool outputs re-sent every round. DONE: 2k send budget in
  all 3 converters (`sendToolOutputChars`, decisions exempt); storage/log
  keeps full 16k.
- [x] A3 `estimateTokens` ignored envelope + schemas → late compaction.
  DONE: +40 tokens/message envelope + full tool-schema block in the budget
  guard (`requestToolsTokens`).
- [x] A4 responses reasoning continuity. ASSESSED, not done: replay needs
  provider-encrypted reasoning objects; the SSE stream only yields
  `summary_text` deltas, so native replay is infeasible without new provider
  data. See D.
- [x] A5 `max_tokens` hardcoded 8192 → long answers cost an extra
  full-history round. DONE: `max_tokens` config, default 16384 (floor 8192).
- [x] B1 tools start only after full SSE round. ASSESSED, not done: chat and
  responses SSE carry no per-call completion signal, so mid-stream execution
  is structurally infeasible there; only messages (`content_block_stop`)
  could overlap, and that transport isn't the daily driver. See D.
- [x] B2 global MCP mutex stalled all servers on one slow call. DONE:
  per-server mutexes.
- [x] B3 hand-rolled grep/glob walks ignored ignore files (only `.git`
  skipped). DONE: skip list (`node_modules`, `target`, `dist`, `build`,
  `bin`, `vendor`, `.venv`/`venv`, `__pycache__`, `.next`, `out`,
  `coverage`). Full `.gitignore` parsing deferred to D.
- [x] B4 no subagent isolation — every explore landed in main history. DONE:
  foreground-only `task` tool (explore/general, isolated history, summary
  folds back, depth ≤2). Wired into system prompts.
- [x] C1/C2 settings: main `thinking=xhigh` kept for solving (explicit),
  compaction effort explicit via `[compaction] thinking` (default `medium`,
  cheapest fallback per model). C3 retry still ignores `Retry-After` — see D.

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

- [ ] remove the white thick bar on the left of the chatbox
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

- [x] M.1 operations that take the model more than 30 seconds need to be investigated as either we're sending too large data in the window or something has gone wrong. How can we handle this? this is a bigger todo that may require soul searching so to speak.
		fixed: per-round lean (`leanHistoryForNextRound` in all 3 transports — prune every round, summarize past the threshold; previously only between turns) + `todowrite`/`todoread` session plan so chunky work runs as small verified steps. Diagnosis: single turns resent rounds × history every round (quadratic), and no plan surface meant sprawling loops.
		perf: per-round lean is now prune-only (no blocking summary in the hot loop; summaries fire between turns via maybeAutoCompact and on overflow retry) + `[compaction] model=glm-5.3-flash` + `thinking=medium` + 429/5xx backoff retry. Rationale: mid-turn summaries doubled latency past 45k and busted prefix-cache; prune keeps bytes stable.
		note: I feel maybe we need to check whether we get clogged down, maybe we need more turns? Big task may need to automatically be done as multiple turn TODO written in memory like opencode does? 'Updating Todos' which are only for this session. This might be the key!
		note2: I am not sure we're actually compacting between tool calls, only between turns, and that may not be often enough? thoughts?
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

