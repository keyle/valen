# valen openspec — conventions

Single-binary Go agent harness. No server/client split, everything in-process in Bubble Tea.

## Source of truth

- `SPEC.md` + `SPEC-tui.md`: behavior, provider/tool/MCP contracts.
- `LIFECYCLE.md`: narrative guide — turn/round lifecycle, token math, sizes, compaction, batching, tools, todos vs task.
- `config.toml`: example config + comments.
- `AGENTS.md`: fast loop, minimal diffs, shared `runTurn` stays UI-agnostic.

## Constraints (all changes must keep)

- Single binary, stdlib-first. No new runtime deps without explicit approval.
  Current direct deps: `toml`, `go-openai`, `mcp-go`, `x/net/html` + charm TUI stack.
- Memory is a feature: currently ~2% of opencode. Keep streaming caps:
  `8MiB LimitReader` per round, `16k` tool-output truncate (storage/log),
  `2k` tool-output send budget per round (wire; decisions exempt),
  `100` grep matches, `1MiB/512KiB` search/fetch caps, `8000` fetch runes,
  `16k` messages `max_tokens` default (floor 8192). Keep compaction caps:
  `15k` keep-tokens tail, `10%` buffer, early threshold at `keep×3`,
  `2000`-char tool output inside the summarizer payload, newest `2` reads
  intact (older reads collapse to path/range descriptors), `40k` newest
  tool-output tokens protected from pruning, user decisions never pruned. Estimation: runes/4 + 40 tokens envelope per message + full tool-schema block per round. Exploration stays lean: read defaults to 2000 lines (max 5000, ~12KB per call), grep capped at 100 matches / 20 per file, dependency/build dirs skipped. Prompt cache: 3 ephemeral breakpoints on messages transport (system, last tool, last block); chat/responses rely on server-side prefix cache over stable bytes.
- Offline tests only: `httptest` fakes, no real network. Never hand-edit `go.sum`.
- `runTurn` in `main.go` shared by headless (`-p`) and TUI: keep UI-agnostic.
  UI state lives in `tuiModel` (`tui.go`) only.
- Sampling/thinking params omitted unless configured (pointer/empty semantics).
- Loop: targeted `go test -run 'TestName' .` while iterating, final gate
  `gofmt -l .` + `go vet ./...` + `go test ./...` (`-race` for concurrency).
