# TODO — fullscreen loop UI (SPEC-tui.md)

## Minor, UX

## Later (D — speedups not taken this round)

- [ ] D1 `Retry-After` aware backoff: honor the header on 429 instead of
  fixed 300/800ms; provider-aware retry policy like opencode's
  `SessionRetry.policy`.
- [ ] D2 full `.gitignore` parsing in grep/glob (current fixed skip list is
  the 80% baseline) + `rg` fast path when on PATH.
- [ ] D3 background shell: `run_in_background` + poll/kill tools so dev
  servers and long builds don't park the loop.
- [ ] D5 B1 follow-up: messages-only mid-stream execution at
  `content_block_stop` (registry callback into the SSE scan + merge
  pre-executed results in `runMessagesTurn` instead of re-running).
- [ ] D6 TUI subagent streaming: forward subagent `tool_start`/`text_delta`
  events into the pending block instead of today's silent single `task` row.

## Execution Follow-ups

Active-turn Responses encrypted reasoning/phase replay and Messages native
thinking/signature capture are implemented, not deferred D4/D7 work. See
`EXECUTION-REVIEW.md` for the source-pinned comparison and known limits.

- [ ] Persist native signed Messages blocks and Responses output items in
  transport-tagged checkpoints, with same-model/prefix-safe replay across
  later turns, overflow redispatch and resume (Chat native reasoning already
  checkpoints). Preserve block order and opaque fields, not display `<think>`.
- [ ] Use provider usage/context limits to complement approximate budgeting,
  including native replay overhead and output headroom; expose compactor cost.
- [ ] Add optional stream-idle timeout distinct from the per-attempt HTTP
  deadline; honor Retry-After (D1) without making retry-body reads block.
- [ ] Define safe compaction for a very long single turn. Current whole-turn
  protection makes `keep_tokens` soft and can fail fast with no compactable
  head; never silently split call/result or signed-prefix transactions.

## Medium

- [x] M.3 Plan mode (`/plan`, `-plan`, Tab in the chatbox) — shipped; see
  `SPEC.md` agent-loop section and `LIFECYCLE.md` §9.

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
- [x] W.1 `todowrite`/`todoread` (in-memory + session fold, opencode schema).
- [ ] W.2 `skill` loader + discovery (`~/.config/valen/skills`,
  `.valen/skills`, `.opencode/`+`.claude/` compat) + `skill` permission —
  now `skill-tool/`.
- [x] W.3 `task` foreground-only (`general`/`explore` policies, depth 1,
  shared `mcpMu`); background deferred. Parallel task batches (max 5) shipped.

Gate per track: SPEC + help + `config.toml` comments together;
`gofmt -l .` clean, `go vet ./...` + `go test ./...` green
(`-race` for parallel/approval paths).

## Safety & input (new openspecs, not parity tracks)

- [ ] I. Composer input (`attach-input/`): paste an image's bytes or drop a
  file → `[img-N]` / `[name.png]` tokens → attachments. Depends on A.
- [ ] S. Shell approvals (`shell-approval/`): `[shell] prompt` policy,
  committed approval lines, extra destructive labels.
- [ ] U. Undo (`turn-undo/`): atomic writes + per-turn journal + `/undo`.
  F.2 uses its atomic helper.
