# Tasks: session-resume

Status: implemented and verified.

Rules: targeted `go test -run` while iterating; no commits unless asked.
Decisions D1–D7 are in `design.md`.

## 1. Session helpers

- [x] 1.1 `resolveSessionArg(arg)` (`session.go`): absolute/existing path wins,
      else `<sessionsDir>/<arg>.jsonl`; error names both.
- [x] 1.2 `openSession(path)` (`session.go`): `loadSession`, ID from the file
      stem, sidecar metadata, summed usage; no create/prune.
- [x] 1.3 `historyFromTurns(cfg, turns)` (`session.go`): moved verbatim from
      `tui_lists.go`, system message refreshed via `effectiveSystem`.
- [x] 1.4 `resumeSessionPath` uses `historyFromTurns`; no behaviour change.

## 2. CLI wiring

- [x] 2.1 `-r` and `--resume` bound to one value flag; `r`/`resume` added to
      `reorderFlags`.
- [x] 2.2 Path-looking arguments absolutized before `-dir` chdir.
- [x] 2.3 Resolve and open after `setFileRoot`; set `cfg.SessionID` before
      `newClient`.
- [x] 2.4 Errors: `-r` in TUI mode (shared one-shot check); `-r` combined with
      `--history`.
- [x] 2.5 `headlessOptions.resume` passed to `chatWithAttachments`.

## 3. Headless behaviour

- [x] 3.1 Resume uses `opts.resume` instead of `newSession`; skips
      `nameSession`.
- [x] 3.2 History: `historyFromTurns` + new prompt; new runs use
      `seedHistory`.
- [x] 3.3 `[session <path>]` on stderr after a successful append, before a
      turn error return; append failure warns and omits the path.
- [x] 3.4 `done.session` in `--stream`; stdout stays pure NDJSON.

## 4. Tests (`resume_test.go`)

- [x] 4.1 `runValenCLIFull` returns stdout and stderr; `runValenCLI` wraps it.
- [x] 4.2 `TestHeadlessReportsSessionPath`: stderr path exists, one turn with
      the prompt.
- [x] 4.3 `TestStreamDoneCarriesSession`: last line has `kind=done` and
      `session`; file exists.
- [x] 4.4 `TestResumeContinuesSession`: second run reuses the file (two turns,
      no new file), keeps the title, and sends the prior prompt/reply.
- [x] 4.5 `TestResumeByID`: bare session ID resolves in the project folder.
- [x] 4.6 `TestResumeRejectsHistory`: `-r` + `--history` exits before a
      request.
- [x] 4.7 `TestResumeKeepsSessionID`: `x-opencode-session` equals the file
      stem on the resumed request.
- [x] 4.8 `TestHeadlessFailureReportsSession`: a failed turn still prints the
      path and leaves the failed turn on disk.
- [x] 4.9 Child-process tests record requests through channels
      (`sseServerWith` + `cliBodies`), so `-race` sees a happens-before edge
      and the suite stays clean.

## 5. Docs and gate

- [x] 5.1 `SPEC.md` usage: stderr session line, `done.session`, `-r`.
- [x] 5.2 `gofmt -l .` clean.
- [x] 5.3 `go vet ./...` green.
- [x] 5.4 `go test ./...` and `go test -race ./...` green with
      `network=true`.
- [x] 5.5 Branch stays local; no commit or push.
