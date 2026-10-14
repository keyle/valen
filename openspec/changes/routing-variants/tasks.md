# Tasks: routing-variants

## Phase 1 — selection + wire

- [ ] 1. `Routing` on `Config`; `validRouting`/`cycleRouting` helpers and the
  `:floor`/`:nitro`/`:exacto` suffix in `wireModelID` (OpenRouter only; Go/Zen
  return the bare ID). `TestRoutingWireSuffix`.
- [ ] 2. ctrl+y handler (`tui.go`) cycles with a notice; **silent no-op** on
  Go/Zen (no notice, no state write). `TestRoutingCycleOpenRouter`,
  `TestRoutingNoopGoZen`.
- [ ] 3. Move thinking show/hide to ctrl+o (default still shown) and the status
  details toggle to ctrl+b; keep `/think`; update the collapse hints
  (`ctrl+o to expand`) and `/help`. `TestCtrlOThinkToggle`.

## Phase 2 — persistence

- [ ] 4. `routing` in `saveModelState` / `loadUserState` / `applyUserState`
  (state.json) and the config.toml schema; invalid/stale values ignored on
  load. `TestRoutingStateRoundTrip`.
- [ ] 5. `modelSlot` gains `routing`; read/write `<digit>: <model> [thinking]
  [route]` (disjoint vocabulary), assign/apply + notices name it.
  `TestSlotsRoutingRoundTrip`.
- [ ] 6. `/route [none|floor|nitro|exacto]` command + `/help` + command list.
  `TestRouteCommand`.

## Phase 3 — footer metadata

- [ ] 7. Capture OpenRouter `provider` + `usage.cost` from the completion body
  and the last SSE chunk (raw-JSON capture beside `altStreamReasoning`); parse
  catalog `pricing.prompt`/`completion` in the models fetch.
  `TestOpenRouterMetadataParse`.
- [ ] 8. Footer appends optional `· <provider> · <tps> t/s · in $x/out $y per M`
  after the balance (tps from local stream timing, no API call); segments drop
  independently and the suffix yields to the existing width rule.
  `TestRoutingFooterMetadata`.

## Phase 4 — docs + gate

- [x] 9. Docs together: `SPEC.md` (config `routing`, state, provider contract,
  footer), `SPEC-tui.md` (header, footer, keys, slots, commands), `README.md`,
  `config.toml` comments.
- [ ] 10. Gate: `gofmt -l .` clean, `go vet ./...` green, `go test ./...` green
  (`network=true`); `-race` if the metadata capture touches concurrent paths.
