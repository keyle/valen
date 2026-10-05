# AGENTS.md — valen

How to work in this repo: fast loop, small steps, together.

## Goals

- Terse: smallest change that closes the gap. Less code, fewer keys, fewer views.
- Maintainable: stdlib-first, single binary, one way to do things. No new
  runtime dep without explicit approval; follow the existing pattern
  (converter + registry + test).
- Fast: bounded parallelism, order-preserving, cached caps. Memory is a
  feature (~2% of opencode) — keep every cap in `openspec/project.md`.
- Consistent: `runTurn` stays UI-agnostic; SPEC + help + `config.toml`
  comments update together; offline `httptest` tests for every track.

## Response loop (most important)

1. Prefer many small iterations over one big drop.
2. Think deeply, verify cheaply: confirm with the smallest possible check
   before widening.
3. While iterating, run ONLY targeted tests:
   `go test -run 'TestName|TestOther' .` (tests that bind a server need
   `network=true`). Run the full suite only when something looks off, when
   touching shared code (`runTurn`, config loading, registry), or as a
   final gate.
4. Final gate: `gofmt -l .` clean, `go vet ./...` green, `go test ./...`
   green (`-race` for concurrency-adjacent changes). Pass `network=true`:
   `httptest` binds a loopback port and the sandbox denies the bind even
   though loopback egress is allowed.
5. If two cheap attempts don't fix it, stop and report: summarize what you
   tried and ask.

## Ask, don't rabbithole

6. If a request is ambiguous (which model family? picker vs direct switch?
   persist or session-only?), ask — one round of questions beats a wrong
   implementation.
7. When you must guess, state the assumption and make it easy to reverse
   (config key, small function, documented default).

## Changes

8. Minimal diffs. Small hunks only; if the diff is much larger than the
   change warrants, undo and reapply minimally. Don't reformat untouched code.
9. `runTurn` in `main.go` is shared by headless (`-p`) and TUI: keep it
   UI-agnostic. UI state lives in `tuiModel` (`tui.go`) only. No
   server/client split, everything stays in-process.
10. Sampling/thinking params stay omitted unless configured
    (pointer/empty semantics). Follow that pattern for new request fields.
11. `SPEC.md` + `SPEC-tui.md` are the source of truth: update the relevant
    spec, help text, and `config.toml` comments when behavior changes.

## Git

- When asked for a worktree, put it under `/tmp` — paths beside the repo
  are not writable under the sandbox:
  `git worktree add /tmp/<name> -b <branch> origin/master` (after
  `git fetch origin`).
- If that fails with "Operation not permitted" because the repo tracks a
  blocked secret (e.g. `.env*`), add `--no-checkout`, exclude the path
  (`git -C /tmp/<name> sparse-checkout set --no-cone '/*' '!/<file>'`),
  then `git -C /tmp/<name> checkout`.
- Keep history linear — rebase the worktree branch onto `origin/master`
  (`git fetch origin && git rebase origin/master`), never merge `master`
  into it back-and-forth.
- Merge back with rebase + fast-forward
  (`git checkout master && git merge --ff-only <branch>`), then
  `git worktree remove /tmp/<name>` and delete the branch.

## Never

- No `user:`/`assistant:` role prefixes in the transcript.
- No real network in tests: all tests use `httptest` fakes. The sandbox
  still needs `network=true` for the loopback bind.
- Don't hand-edit `go.sum` (`go mod tidy` owns it).
