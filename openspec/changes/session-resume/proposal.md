# Change: session-resume

## Why

An external model driver (the voxtral voice loop, background jobs) runs valen
as a one-shot subprocess. It cannot continue a conversation: every `-p` run
starts a new session, and nothing tells the caller where the session JSONL
went. The driver would have to guess the project folder hash and parse the
directory itself, or keep the whole history in memory and pass it back through
`--history`.

## What changes

1. **`-p` reports the session path on stderr**: `[session <abs path>]`, after
   the turn is saved. Stdout stays exactly the answer.
2. **`--stream` reports it in the last NDJSON line**: `done.session`.
3. **`-r/--resume <id-or-path>`**: headless run that appends to a saved
   session instead of starting a new one. The argument is an absolute or
   existing JSONL path, or a session ID looked up in this project's session
   folder.
4. **Resume keeps the session identity**: the ID comes from the file name, so
   `x-opencode-session` stays stable; config model/thinking stay authoritative,
   like TUI resume.

## Non-goals

- No `-r` for the TUI: `/sessions` already resumes there.
- No change to `--history`: `-r` and `--history` are mutually exclusive.
- No new session format and no new runtime dependency.
- No automatic title rewrite on resume: the existing sidecar title stays.

## Success criteria

- `-p` prints the answer on stdout and `[session <path>]` on stderr, and the
  path names the JSONL that holds the turn.
- A second run with `-r <path>` appends to the same file (two turns, no new
  file) and sends the prior conversation to the model.
- `--stream` keeps stdout pure NDJSON and puts `session` in `done`.
- Targeted tests green offline; final gate: `gofmt -l .` clean,
  `go vet ./...` green, `go test ./...` green with `network=true`.
