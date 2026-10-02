# Change: sessions

## Why

Every run mints `ses-<unixnano>` and `/clear` wipes memory (`main.go`,
`session.go`, `tui.go`). There is no resume/fork, so a dropped TUI or a
second terminal means starting over. Opencode has `/sessions`|`/resume` +
`--session/--continue/--fork` + titles.

## What changes

1. Headless `--session ID` (resume exact), `--continue` (newest JSONL),
   `--fork` (copy history under a new ID).
2. Title = first user prompt truncated to 60 runes, kept alongside the
   JSONL (sidecar/filename suffix — JSONL stays append-only).
3. TUI `/sessions` (`/resume`|`/continue`) overlay reusing the history
   view + existing `listSessions()` newest-first: Enter resumes, `f` forks.

## Non-goals

- Share/unshare URLs, `import` from JSON/URL, server-backed sync.

## Success criteria

- Flag round trip on seeded JSONLs + overlay resume/fork test; no session
  file ever rewritten in place.
