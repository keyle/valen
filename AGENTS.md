# AGENTS.md — mods

How to work in this repo: fast loop, small steps, together.

## Response loop (most important)

1. Keep the feedback loop quick enough that we work together. Prefer many small iterations over one big drop.
2. Think deeply, verify cheaply: keep reasoning effort high, but confirm with the smallest possible check before widening.
3. While iterating, run ONLY targeted tests: `go test -run 'TestName|TestOther' .`.
   Run the full suite only when something looks off, when touching shared code
   (`runTurn`, config loading, registry), or as a final gate before handing back.
4. Final gate before handing back: `gofmt -l .` clean, `go vet ./...` green,
   `go test ./...` green (`-race` for concurrency-adjacent changes).
5. If two cheap attempts don't fix it, stop and report: summarize what you tried and ask.

## Ask, don't rabbithole

6. Ask clarification when needed, don't rabbithole. If a request is ambiguous
   (which model family? picker vs direct switch? persist or session-only?), ask —
   one round of questions beats a wrong implementation.
7. When you must guess, state the assumption and make it easy to reverse
   (config key, small function, documented default).

## Changes

8. Minimal diffs. Small hunks only; check the diff size — if it's much larger
   than the change warrants, undo and reapply minimally. Don't reformat untouched code.
9. `runTurn` in `main.go` is shared by headless (`-p`) and TUI: keep it UI-agnostic.
   UI state lives in `tuiModel` (`tui.go`) only. No server/client split, everything
   stays in-process in the Bubble Tea program.
10. Sampling/thinking params stay omitted unless configured (pointer/empty semantics).
    Follow that pattern when adding request fields.
11. `SPEC.md` + `SPEC-tui.md` are the source of truth: update the relevant spec,
    help text, and `config.toml` comments when behavior or commands change.

## Never

- No `user:`/`assistant:` role prefixes in the transcript.
- No real network in tests: all tests are offline via `httptest` fakes.
- Don't hand-edit `go.sum` (`go mod tidy` owns it).
