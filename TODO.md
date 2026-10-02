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

- [ ] S. Sessions (`sessions/`): resume/continue/fork without a server.
  Verifiable: `--session/--continue/--fork` round-trip + `/sessions`
  overlay resume/fork on seeded JSONLs.
- [ ] S.1 Flags + title capture (first prompt, 60 runes); JSONL stays
  append-only, fork copies history under a new ID.
- [x] S.1a Title (first prompt, 60 runes) + model/effort captured per turn
  and in a `.jsonl.meta.json` sidecar; Ctrl+L `/sessions` overlay lists
  titles with model/effort and resumes on Enter; tool-call log moved to
  Ctrl+U (`/tools`). (`TestSessionTitleCapturedOnce`,
  `TestTUISessionsListShowsTitlesAndResumes`.)
- [ ] S.2 `/sessions` (`/resume`|`/continue`) overlay reusing history view:
  Enter resumes, `f` forks.

- [x] C. Compaction (`compaction/`): summary-mode only, send-side (disk log
  lossless). Closes perf 4.4. Verifiable: long fixture compacts, recent
  `keep.tokens` kept, `/compact` + auto on too-long 400.
- [x] C.1 `compactSession` via cheap summarizer model (summarizer prompt, runes/4
  token estimate, `[compaction] auto/keep_tokens/buffer/model`; empty model =
  session model at minimal thinking).
- [x] C.2 `/compact` (`/summarize` alias) + auto trigger; attachment data →
  description in kept window.

- [ ] P. Permissions (`permissions/`): ordered `allow/ask/deny` + patterns,
  defaults preserve today (`default allow`, `external_directory ask`).
  Verifiable: pure evaluator table test + once/always/reject via stubbed
  `askPrompter` + triple-repeat fixture.
- [ ] P.1 Evaluator (last-match-wins, `deny` hard-stops) + TOML table
  (`shell`, `read:*.env`, `mcp_*` wildcards).
- [ ] P.2 `ask` via `promptUser` (once/always/reject); `always` appends to
  git-ignored `.mods/approvals.toml`, never overrides `deny`.
- [x] P.3 `doom_loop`: 3× identical call → guided ask instead of 4th run.
  (done: `doomLoopTool` aborts the turn with a friendly error on all three
  transports; `checkContextBudget` fail-fasts over-budget history;
  `TestRunTurnStopsDoomLoop`/`TestDoomLoopToolShapes`/
  `TestRunTurnRejectsOversizeContext` green.)

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
- [ ] W.2 `skill` loader + discovery (`~/.config/mods/skills`,
  `.mods/skills`, `.opencode/`+`.claude/` compat) + `skill` permission.
- [ ] W.3 `task` foreground-only (`general`/`explore` policies, depth ≤2,
  shared `mcpMu`); background deferred.

Gate per track: SPEC + help + `config.toml` comments together;
`gofmt -l .` clean, `go vet ./...` + `go test ./...` green
(`-race` for parallel/approval paths).

- [x] 1. Refactor: extract agent loop from `chat()` into `runTurn(..., onEvent func(Event))`; headless `run()` keeps working (`go test ./...` green).
- [x] 2. `session.go`: `Turn` model (user/assistant/thinking/toolcalls/usage/error/timestamp), JSONL append + load at `$XDG_DATA_HOME/mods/sessions/<id>.jsonl`, `/clear` semantics (new file).
- [x] 3. `errors.go`: `friendlyError(err) → {Title, Details}` mapping table from SPEC-tui; unit-tested for 401/402/404/429/5xx/timeout/cancelled/empty-choices/max-rounds/mcp.
- [x] 4. `think.go`: parse `<think>...</think>` and reasoning field into `Turn.Thinking`; unit-tested.
- [x] 5. `tui.go`: bubbletea chat — viewport conversation, textarea input, spinner status, overlays (history/tools/errors/help/detail), enter/esc/ctrl shortcuts, `/clear` `/think` `/save` `/help` `/quit`.
- [x] 6. Recall views: history (current turns + past JSONL files read-only), tool-call log with full output, errors with details.
- [x] 7. Conversational UX: instant user echo on submit, thinking shown by default (ctrl+y toggles), padded blocks, no `user:`/`assistant:` prefixes, Monokai-dark lipgloss + glamour theme, token tallies.
- [x] 8. `main()`: TTY && !`-p` → TUI; else headless `run()`; shared config/registry/MCP setup, single in-process runtime (no server/client split).
- [x] 9. Tests: model-level (instant echo, reply/headers/usage, 402 friendly error, history/tools detail, `/clear` `/think` unknown command, no role prefixes), session JSONL round-trip, `go vet`, `gofmt`, `go test -race ./...` green.
- [x] 10. Docs: SPEC.md layout/usage updated for both modes; SPEC-tui.md rewritten for bubbletea; TODO.md checkboxes updated.
- [x] 11. Models: `models.go` (live `/models` list + curated fallback, transport map, per-model thinking levels, comment-preserving `config.toml` persist), `thinking` config key with `loadConfig` validation, reasoning params in `runTurn`, `/model` (ctrl+p) + `/effort` (`/thinking` alias) pickers with persistence, header shows level, help + SPEC/SPEC-tui/config.toml docs, `models_test.go` + TUI switch tests.
- [x] 12. the models should request permission to navigate outside of the executed folder. we also should add a parameter to ./mods where it takes a folder, e.g. ./mods ~/Code/something, or ./mods . (`-dir` flag + positional dir in TUI mode; file tools prompt via AskUserQuestion on escapes; `files_test.go` covers split/containment/allow/deny, `main_test.go` the builtin list.)
- [x] 13. the progress text in white appends to the end of line as progress happens that is meta progress. this is good. however we'd like it to insert a \n for cleaner approach. (each `text_delta` now lands on its own line in the live pending block via `appendStreamLine`; committed reply text unchanged.)
- [x] 14. ability to change the thinking level with ctrl+[ for lower and ctrl+] for higher (they're usually listed low to high). (`stepThinking` low→high with clamp; ctrl+[ arrives as esc in the terminal so ctrl+\ steps down, ctrl+] steps up; help + SPEC-tui updated.)
- [x] 15. questions need to wrap, so does content in the timeline / scrollback buffer, it's going off the screen! (all transcript/question/detail blocks wrap to viewport width via `contentWidth` + `wrapPlain`, long single tokens force-broken via `breakLongWords`; lists truncate to width.)
- [x] 16. the chatbox I'd like shift+enter to not submit but add a line return. Also the background is gray but not for the entire box. It would be better if it was either the entire box or no background to it at all! (shift+enter/alt+enter/ctrl+j insert newline — shift+enter where the terminal supports it, alt+enter and ctrl+j always; textarea CursorLine background cleared so the box is uniformly flat.)
- [x] 17. can we hint to read AGENTS.md on a blank session, first. Make that part of the default "you are a helpful assistant" type line. Give a hint that we're a mcp capable thinking agent with a fast loopback. (built-in `defaultSystem` used when `system` is unset, in TUI + headless; empty-state transcript shows the AGENTS.md tip; SPEC.md + config.toml document the override.)
- [x] 18. remove the slop intro text that looks like AI generated. just offer a very simple label. (blank chat now renders only `No turns yet.` via `renderChat`; the old "Ask anything … Tip …" paragraph is gone.)
- [x] 19. shift the 'automatically reading Agents.md (see 17.) to be left to the user's "You are a helpeful assistant" tagline thing, just use it as default and put it in the toml as default. (`system` is now set explicitly in `config.toml` to the AGENTS.md-first default; `defaultSystem` in `main.go` stays as the built-in fallback when `system` is empty; `TestTUIDefaultSystemHintsAgents` pins the label + the default.)

