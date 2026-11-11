# Design: session-resume

Built on the uncommitted `harness` change (same working tree). Session layout
and TUI resume already exist; this change exposes them to one-shot callers.

## Verified facts

- **Sessions folder.** `sessionsDir()` (`session.go:85`) is
  `$XDG_DATA_HOME/valen/sessions/<workdir>-<hash>`, one folder per project.
  `newSession` (`session.go:105`) creates it and prunes the oldest files;
  `loadSession` (`session.go:237`) reads JSONL checkpoints and the latest
  `History` record subsumes earlier ones.
- **TUI resume.** `resumeSessionPath` (`tui_lists.go:286`) loads the turns,
  sets `ID` from `filepath.Base(path)` stem, rebuilds `cfg.SessionID`, rebuilds
  the history from the newest checkpoint and replays later turns. That history
  builder is the only new shared code: it moves to `historyFromTurns`.
- **Headless turns.** `chatWithAttachments` (`chat.go:499`) always calls
  `newSession`, `nameSession`, then appends the turn. A failed turn is still
  appended (with `Error`), so the path must be reported even when the run
  fails.
- **`x-opencode-session`.** Stamped by `headerTransport` and the raw POST
  helpers at request time from `cfg.SessionID`; `newClient`/`newHTTPClient`
  capture the config, so the resumed ID must be set before both are built
  (`main.go:232`, `main.go:258`).
- **Flag parsing.** `reorderFlags` (`main.go:338`) needs `r`/`resume` in its
  value map, or `-p prompt -r id` reads `id` as a positional.
- **`--history`.** `loadHistoryFile` rejects system/tool roles; it stays
  separate from resume.

## Decisions (defaults, reversible)

- **D1 Path on stderr.** Plain `-p` prints `[session <abs path>]` on stderr, so
  stdout is byte-for-byte the answer. `--stream` prints nothing extra on
  stderr for the session and carries `"session"` in `done` instead (stdout
  stays pure NDJSON). Written after append, before returning a turn error, so
  a failed run is still resumable.
- **D2 Append failures.** If `session.append` fails, warn
  `valen: session: <err>` on stderr and omit the session from stderr/done.
  Never report a path that does not hold the turn.
- **D3 Argument.** `-r` takes an absolute path or an existing file path as
  given; otherwise it is a session ID (`<id>.jsonl` in this project's session
  folder). Path-looking arguments are made absolute before `-dir` chdir, so
  relative paths resolve from the launch directory. A miss names both paths
  tried.
- **D4 No create, no prune.** `openSession` only loads: no `MkdirAll`, no
  `nameSession`, and `Keep = 0` so append does not prune. The sidecar title is
  preserved; `ensureMeta` only fills blank fields.
- **D5 Headless only.** `-r` in TUI mode is an error, like `--stream` and
  `--history`. `-r` with `--history` is rejected as ambiguous. A prompt is
  still required.
- **D6 History order.** `historyFromTurns(cfg, turns)`, then `--history` (empty
  in practice), then the new prompt. New runs keep `seedHistory`.
- **D7 Config wins.** Model and thinking come from config/state, not the
  session metadata, exactly like TUI resume.

## Files

- `session.go`: `resolveSessionArg`, `openSession`, `historyFromTurns`.
- `tui_lists.go`: `resumeSessionPath` calls `historyFromTurns` (no behaviour
  change).
- `main.go`: `-r`/`--resume`, reorder map, validation, resolve+open, set
  `cfg.SessionID`, pass `headlessOptions.resume`.
- `chat.go`: resume instead of `newSession`, skip `nameSession`, report the
  path (stderr or `done.session`), warn on append failure.
- `resume_test.go`: CLI tests against the fake SSE server.
