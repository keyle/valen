# AGENTS.md — valen

How to work in this repo: fast loop, small steps, together.

## Goals

- Terse: smallest change that closes the gap. Less code, fewer keys, fewer views.
- Maintainable: stdlib-first, single binary, one way to do things. No new
  runtime dep without explicit approval; follow the existing pattern
  (converter + registry + test).
- Fast: bounded parallelism, order-preserving, cached caps. Memory is a
  feature — keep every cap in `openspec/project.md`.
- Consistent: `SPEC.md`/`SPEC-tui.md` are the source of truth; update the
  relevant spec, help text, and `config.toml` comments together.

## Loop

1. Prefer many small iterations over one big drop. Think deeply, verify
   cheaply: confirm with the smallest possible check before widening.
2. While iterating run ONLY targeted tests:
   `go test -run 'TestName|TestOther' .`. Run the full suite only when
   something looks off, when touching shared code (`runTurn`, config loading,
   registry), or as the final gate.
3. Final gate: `gofmt -l .` clean, `go vet ./...` green, `go test ./...`
   green (`-race` for concurrency-adjacent changes). Pass `network=true`:
   tests are offline `httptest` fakes but still bind a loopback port.
4. If two cheap attempts don't fix it, stop and report what you tried. If the
   request is ambiguous (which model family? picker vs direct switch? persist
   or session-only?), ask — one round of questions beats a wrong
   implementation. When you must guess, state the assumption and make it easy
   to reverse (config key, small function, documented default).

## Changes

5. Minimal diffs: small hunks only, no reformatting untouched code. If the
   diff is much larger than the change warrants, undo and reapply minimally.
6. `runTurn` (`main.go`) is shared by headless (`-p`) and TUI: keep it
   UI-agnostic. UI state lives in `tuiModel` (`tui.go`) only. No server/client
   split — everything stays in-process.
7. Sampling/thinking params stay omitted unless configured (pointer/empty
   semantics). Follow that pattern for new request fields.

## Git

- Worktrees live under `/tmp` (paths beside the repo are not writable under
  the sandbox): `git worktree add /tmp/<name> -b <branch> origin/master`
  after `git fetch origin`.
- If that hits "Operation not permitted" from a tracked secret (e.g.
  `.env*`), add `--no-checkout`, exclude it
  (`git -C /tmp/<name> sparse-checkout set --no-cone '/*' '!/<file>'`),
  then `git -C /tmp/<name> checkout`.
- Keep history linear — rebase the branch onto `origin/master`, never merge
  `master` back and forth. Merge back with `git merge --ff-only <branch>`,
  then remove the worktree and branch.

## Never

- No `user:`/`assistant:` role prefixes in the transcript.
- No real network in tests: all tests use `httptest` fakes.
- Don't hand-edit `go.sum` (`go mod tidy` owns it).
