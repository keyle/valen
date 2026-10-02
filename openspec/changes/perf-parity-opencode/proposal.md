# Change: perf-parity-opencode

## Why

`valen` feels slower than opencode, and the gap widens on deeper tasks.
Wall-clock is `sum(latencies)` almost everywhere — no `go func`/`WaitGroup`
in non-test code. History cost is `O(rounds × history)` per turn, so each
successive turn re-pays more: full re-encode + full re-send + full re-render.

Memory is the counter-weight: `valen` uses ~2% of opencode. This change must
close the latency gap without losing that.

## What changes

1. Parallel independent tool calls (bounded, order-preserving).
2. Parallel MCP startup; shorter, shared HTTP pooling.
3. Faster local search (`glob`/`grep`) with bounded concurrency + optional `rg`.
4. Kill the `O(n²)` depth degradation: stable prompt prefix, smaller per-round
   payloads, incremental TUI render, async session/usage I/O, history growth guard.
5. Explicit latency + memory budgets so the 2% win stays visible.

## Non-goals

- Multi-agent orchestration, subagent fan-out, Code Mode sandbox, background tasks.
- SQLite/WAL session store, Bun/Hono-style server split, LSP.
- New networked features, new tools, permission-model changes.
- Any unbounded cache, goroutine-per-delta, or full-history markdown cache that
  trades the 2% memory win for speed.

## Context

Prior research (valen audit + opencode `sst/opencode` docs/code):

- opencode runs one turn's tool calls with `concurrency:10`
  (`tool-runtime.ts`), `read` fan-out 8, `grep` stat 16; TUI batches deltas
  every 40ms; `grep`/`glob` use `rg --json`; prompt-cache breakpoints (max 4)
  + stable system prefix + 1h TTL; fewer LLM rounds via `plan`/`explore` agents.
- valen sequential spots: `main.go:299-308`, `messages.go:334-343`,
  `responses.go:343-352` tool loops; `mcp.go:27-38` serial startup;
  `stream.go:21-43` re-marshal per round; `files.go:330-348,401-444` serial
  `WalkDir`; `tui.go:1455-1537` full re-render per delta;
  `session.go:67-85` sync append on UI thread.

## Success criteria

- Multi-tool turn wall-time ~max(tool latencies), not sum. Covered by offline test.
- Depth test: turn N latency grows ~linearly with new tokens, not quadratically
  with history bytes re-encoded/re-rendered.
- Memory: no regression vs baseline on a fixed deep-turn fixture (RSS / allocs);
  caps in `project.md` unchanged unless proposal says otherwise.
- `gofmt -l .` clean, `go vet ./...` green, `go test ./...` green
  (`-race` for concurrency items).
