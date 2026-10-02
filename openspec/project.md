# mods openspec — conventions

Single-binary Go agent harness. No server/client split, everything in-process in Bubble Tea.

## Source of truth

- `SPEC.md` + `SPEC-tui.md`: behavior, provider/tool/MCP contracts.
- `config.toml`: example config + comments.
- `AGENTS.md`: fast loop, minimal diffs, shared `runTurn` stays UI-agnostic.

## Constraints (all changes must keep)

- Single binary, stdlib-first. No new runtime deps without explicit approval.
  Current direct deps: `toml`, `go-openai`, `mcp-go`, `x/net/html` + charm TUI stack.
- Memory is a feature: currently ~2% of opencode. Keep streaming caps:
  `8MiB LimitReader` per round, `16k` tool-output truncate, `100` grep matches,
  `1MiB/512KiB` search/fetch caps, `8000` fetch runes.
- Offline tests only: `httptest` fakes, no real network. Never hand-edit `go.sum`.
- `runTurn` in `main.go` shared by headless (`-p`) and TUI: keep UI-agnostic.
  UI state lives in `tuiModel` (`tui.go`) only.
- Sampling/thinking params omitted unless configured (pointer/empty semantics).
- Loop: targeted `go test -run 'TestName' .` while iterating, final gate
  `gofmt -l .` + `go vet ./...` + `go test ./...` (`-race` for concurrency).
