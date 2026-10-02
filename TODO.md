# TODO — fullscreen loop UI (SPEC-tui.md)

Ordered, each item ends in a tangible, verifiable result.

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
- [x] 12. the models should request permission to navigate outside of the executed folder. we also should add a parameter to ./mods where it takes a folder, e.g. ./mods ~/Code/something, or ./mods . (`-dir` flag + positional dir in TUI mode; file tools prompt via AskUserQuestion on escapes.)
- [ ] 13. the progress text in white appends to the end of line as progress happens that is meta progress. this is good. however we'd like it to insert a \n for cleaner approach.

