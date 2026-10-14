# Change: routing-variants

## Why

OpenRouter serves one model slug through many upstream providers whose price,
throughput and tool-call quality vary. Its model-suffix variants give
request-level control with no provider object to hand-build:

- `:floor` — cheapest (`provider.sort: "price"`, flex-tier eligible).
- `:nitro` — highest throughput (`provider.sort: "throughput"`, priority-tier
  eligible).
- `:exacto` — quality-first provider sorting for tool use (same signals as Auto
  Exacto, applied explicitly).

valen already treats the reasoning level as a per-model, per-slot switch; the
routing preference is the same kind of selection and is currently
unavailable. Go and Zen each have a single upstream and no such variants, so
the control must be OpenRouter-only and harmless elsewhere.

## What changes

1. **Per-session routing mode** — one of `none` / `floor` / `nitro` / `exacto`,
   default `none`.
   - OpenRouter only: serialized as a `:floor` / `:nitro` / `:exacto` suffix on
     the wire model (`wireModelID`); `none` sends the bare slug (unchanged
     default routing).
   - Go/Zen: a silent no-op — no notice, no state write, key left untouched.
2. **ctrl+y cycles** `none → floor → nitro → exacto → none` and posts a
   one-line notice. `/route [none|floor|nitro|exacto]` sets/reads it
   (`/thinking`-style), and appears in `/help`.
   - Because ctrl+y is repurposed, the thinking-block show/hide toggle moves to
     **ctrl+o** (thinking stays **shown by default**) and remains available as
     `/think`; the status-details toggle it displaces moves to
     **ctrl+b**. Both chords are listed in `/help`.
3. **Persistence** — routing rides with model + thinking everywhere they are
   saved: `state.json` gains a `routing` key (validated and applied on boot like
   `thinking`), `config.toml` gains an optional `routing` key, and favourite
   slots store it. Slot syntax stays one line: `<digit>: <model> [thinking]
   [floor|nitro|exacto]` — the two vocabularies are disjoint, so the parser
   needs no quoting. Slot assign/apply and their notices name the routing.
4. **Footer metadata** (OpenRouter only, best effort) — after the balance,
   append the serving provider, throughput, rates and the session's cumulative
   billed cost when OpenRouter reports them, e.g. `openrouter $12.34 left ·
   Infermatic · 42 t/s · in $0.10/out $0.40 per M · session $0.0034`.
   - Provider + billed cost come from the top-level `provider` and `usage.cost`
     (`cost_details.upstream_inference_cost`) in the completion body, or in the
     last SSE usage chunk when streaming.
   - Input/output per-million rates come from the model catalog's
     `pricing.prompt` / `pricing.completion`.
   - TPS is measured from our own stream — completion tokens over the wall time
     from the first to the last content delta — so it needs no extra API call.
   - The session cost sums every turn's billed cost (all tool-loop rounds),
     four decimals below a cent, resetting on a new/resumed session; being
     local it shows even without a balance.
   - Every segment is independently optional; missing data drops silently and
     the whole suffix yields to the width rule so narrow terminals keep the
     plain balance.

## Non-goals

- A general `provider` object (`order`, `only`, `ignore`, `max_price`,
  `service_tier`) — the three documented suffixes only.
- Routing on Go/Zen, or inventing provider/tps/cost the provider does not
  report.
- Changing OpenRouter's default (load-balanced) routing: a `none` request is
  byte-for-byte what valen sends today.

## Success criteria

- `TestRoutingWireSuffix` (suffix only on `openrouter/…`; `none` = bare),
  `TestRoutingCycleOpenRouter`, `TestRoutingNoopGoZen` (ctrl+y silent, state
  untouched), `TestRoutingStateAndSlotsRoundTrip`, `TestRoutingFooterMetadata`
  (fake SSE + catalog; absent fields drop cleanly).
- No new runtime deps; all fakes offline (`httptest`).

## Open questions

- **Per-M rates:** the models fetch does not parse `pricing` today; the smaller
  first cut shows only the actual per-turn cost and adds the rates later.

## Docs-first

This change lands the openspec plus the source-of-truth docs (`SPEC.md`,
`SPEC-tui.md`, `README.md`, `config.toml` comments) ahead of the implementation;
the code tasks in `tasks.md` are the follow-up.
