# LIFECYCLE — how a complex task flows through valen

A step-by-step reading guide to the harness side of models: what a turn is,
what a round is, where tokens go, why big jobs slow down, and what each
mechanism (batching, tools, todos, compaction, guards) costs and buys.
Numbers below are the live defaults in code, not approximations.

Terms first, because the codebase uses them precisely:

| Word | Meaning | Code |
| --- | --- | --- |
| **turn** | One user prompt → final assistant answer, including all tool work. The unit billed, logged, and resumed. | `Turn` in `session.go`, `runTurn` in `main.go` |
| **round** | One model request → response inside a turn. A turn with 5 tool batches = 5+ rounds. | loop in `runChatTurn` / `runMessagesTurn` / `runResponsesTurn` |
| **step** | opencode's word for roughly one round (its processor emits `step-start` / `step-finish` events around each model call plus its tool calls). Valen has no step events; round is the equivalent unit. | opencode `session/processor.ts` |
| **batch** | The set of tool calls the model emits in one round. Executed together, results appended in request order. | `runToolCalls` in `main.go` |
| **history** | The full OpenAI-shaped message list (`system` + alternating assistant/tool messages) carried across turns in memory. | `[]openai.ChatCompletionMessage` |

## 1. The lifecycle of one user turn, step by step

Say you type "add retry with backoff to the fetcher and cover it with tests"
and press Enter in the TUI. What happens, in order:

1. **Submit (UI thread).** The TUI echoes your message instantly, snapshots
   history (copy + your message), flips to busy, and fires a background
   command. The UI thread never blocks on the network; it only renders
   progress events. (`submit` in `tui.go`, `toolEventMsg` / `turnDoneMsg`.)
2. **Pre-send compaction check.** Before the first request,
   `maybeAutoCompact` runs: cheap local prune always (see §5), plus a
   summarizer LLM call only if history is past threshold. (`dispatchTurn`.)
3. **Round 1 request.** The active transport converts history to its wire
   format and POSTs with `stream: true`. Every request carries
   `x-opencode-session` (routing + cache affinity) and the iPad Safari
   `User-Agent`. System + tool schemas ride on **every** round.
4. **Stream parse.** SSE deltas (`text_delta`, `thinking_delta`) forward to
   the UI live (~60ms throttle, viewport pinned). Tool calls accumulate until
   the terminal event (`[DONE]` / `response.completed` / `message_stop`).
   A provider answering unary JSON folds through the same validation with no
   deltas; a 400 blaming streaming retries the round once without it.
5. **Append assistant message, run the batch.** Tool calls execute —
   parallel up to 8, results appended in request order — and each result is a
   new `role: tool` message in history. Per-round prune collapses older
   reads/outputs (local, no network).
6. **Rounds 2..N.** Repeat 3–5. Each round re-sends the *entire* grown
   history plus schemas: this is the `rounds × history` cost from §3.
7. **Finish.** A round with zero tool calls ends the turn: reply renders as
   markdown, token tally prints (`↑ prompt · ↓ completion`), the `Turn`
   (prompt, thinking, reply, tool records, usage, model/effort) appends to
   the session JSONL, UI goes idle. Errors (including cancel) also commit a
   turn so nothing is lost and the prompt is retryable.

Headless `-p` runs the identical `runTurn`; only the event sink differs
(stderr `[tool …]` lines vs TUI timeline). There is deliberately one loop.

## 2. Rounds vs turns, and why the count matters more than it looks

Cost of a turn ≈ sum over rounds of (prefill(history + schemas) + decode).
Prefill dominates once history passes ~20k tokens, and history grows every
round (assistant message + N tool results). So turn cost is roughly
**quadratic in the number of rounds**: round k re-sends everything rounds
1..k−1 produced.

Worked example (chat transport, defaults). Prompt needs: 1 plan write +
4 reads (10k each, stored) + 2 edits + final answer = ~8 rounds.

| Round | History content (stored) | Wire send (with 2k budget) |
| --- | --- | --- |
| 1 | system + prompt + schemas (~8k) | ~8k |
| 2 | + todowrite result (~1k) | ~9k |
| 3–6 | + 4 × 10k reads | ~13k, ~15k, ~17k, ~19k (each read contributes only 2k on wire) |
| 7–8 | + edits + summary | ~21k, ~22k |

Total prefill ≈ 115k tokens. **Without** the send budget, rounds 3–8 would
carry +8k extra each (≈ +48k prefill) — that is the A2 win in concrete
terms. Without batching (1 tool/round → 12 rounds instead of 8), total
prefill ≈ 200k+. Round count is the master lever: batching (§6) and the
task subagent (§7) both work by cutting rounds, and every mechanism below
is priced against this math.

Hard stops: `max_rounds` (default 100) ends a turn without an answer;
`doomLoopTool` aborts when the *same single* tool call repeats a third
consecutive time (multi-call batches break the chain — they run as one
parallel unit, not a loop step).

## 3. Token math: what a request is actually made of

One round's prefill, largest-first:

1. **History content** — all assistant text, user prompts, tool results
   (each capped to 2k on the wire; decisions verbatim).
2. **Tool schemas** — every builtin + every MCP server's tools, every round.
   Three builtins ≈ 300 tokens; a couple of MCP servers can be thousands.
   The budget guard counts this (`requestToolsTokens`); the old estimator
   didn't, which is why compaction used to fire late.
3. **System prompt** — a few hundred tokens, every round.
4. **JSON envelope** — roles, tool-call IDs, structure ≈ 40 tokens/message
   (`messageOverheadTokens`).
5. **Thinking/reasoning config** — small field, but it *authorizes* large
   decode: `xhigh` lets the model spend far more output tokens per round
   than `medium`.

The estimator is `runes/4 + 40/message + schemas/4` — no tokenizer dep,
deliberately approximate, used only for guardrails (fail-fast budget,
compaction threshold, prune accounting), never for billing. Server-reported
`usage` is the billed truth and is what the tally prints.

Decode side: you pay per output token, and thinking models spend output
tokens on reasoning *every round*. `thinking=xhigh` on the main loop plus a
big-model summarizer was the old default burn; the split is now explicit:
top-level `thinking` solves, `[compaction] thinking` (default `medium`,
cheapest fallback per model) summarizes.

## 4. Every size in the system, and why it is that number

| Knob | Value | Why |
| --- | --- | --- |
| `max_rounds` | 100 | Backstop against flapping loops; healthy tasks finish in <15 |
| `max_context_tokens` | 300000 | Fail-fast per-round budget, below the smallest catalog limit with margin |
| `max_tokens` (messages) | 16384, floor 8192 | Long answers finish in one round instead of two full-history rounds |
| Tool output stored | 16000 bytes | Session log stays useful; memory stays flat |
| Tool output on wire | 2000 chars | Matches opencode; rounds stop re-uploading 16k results; `send-truncated` marker tells the model to re-run narrower |
| Compaction `keep_tokens` | 15000, tail verbatim | Recent context the summary can't afford to lose |
| Compaction `buffer` | 10% of budget | Headroom so the summary + tail + reply fit under the fail-fast line |
| Compaction threshold | min(budget−buffer, keep×3) → **45000** default | Compacts early and often instead of near the limit |
| Prune: newest reads | 2 intact | Fresh reads are acted on immediately; older ones re-read cheaply via `read {path, offset}` |
| Prune: protect window | newest 40k tool-output tokens | Matches opencode `PRUNE_PROTECT`; shields the working set |
| Prune: decisions | never shrink | `AskUserQuestion` answers are load-bearing; summary must keep them verbatim |
| `read` default/max | 2000 / 5000 lines, ~12KB/call | opencode parity on lines; 12KB fits under the 16k storage cap so the offset-continue hint always reaches the model |
| `grep` | 100 matches, 20/file, skip >1MiB/binaries | One noisy file can't starve the result; same shape as opencode's limit: 100 |
| `shell` | 60s timeout, combined output | Non-zero exit returns as content (`[exit N]`), never as loop error |
| MCP handshake / call | 30s / 2m | Slow servers warn-and-continue at startup, never abort the run |
| Parallel tools | semaphore 8 | Wall-clock ≈ max(latencies); matches grep's worker pool |
| Stream body cap | 8MiB LimitReader/round | A runaway stream can't OOM the harness |
| Retry | 3 attempts, 300ms→800ms, 429/502/503/504 + network errors | Overloads back off; 400s never retry (stream-fallback depends on it) |
| Task subagent | ≤25 rounds, depth ≤2 | A flailing delegate can't eat the session budget |
| Web | search 8 results/1MiB; fetch 512KiB→8000 chars | Snippets stay plannable, pages stay skimmable |
| TUI stream tail | last ~1500 runes live; 32k delta cap; 60ms throttle | Live view stays cheap; commit renders the full text |

Ditched along the way: 200-line reads (10 rounds to see a file), hardcoded
8192 answer cap, global MCP mutex, unbilled schema blind spot, mid-turn
blocking summaries.

## 5. Compaction, in full (the part everyone hand-waves)

Two mechanisms, different jobs:

**Prune (every send, local, free).** `pruneOldToolOutputs` rewrites history
*in place*: all but the newest 2 `read` results collapse to
`File <path> (<total> lines, showing <a>-<b>)` + next-offset hint; other
outputs past the newest 40k tokens shorten to 2000 chars + `[pruned …]`;
decisions never shrink. Rationale: file content is disposable because the
model holds the path/range descriptor and can re-read exactly. Prune is why
the per-round lean costs zero network.

**Summary (past threshold, one LLM call).** `splitForCompaction` keeps
leading system messages + the newest `keep_tokens` tail verbatim; the head
serializes to handoff text (reads as descriptors, decisions fuller, outputs
capped 2k — same shape opencode sends its summarizer) and the
`[compaction] model` at `[compaction] thinking` with **no tools** returns
prose covering objective, decisions, completed/active work, blockers, next
moves, files. The head is replaced by one `[Session summary …]` assistant
message. Repeats *update in place* (previous summary feeds the prompt), so
blocks never stack. Disk JSONL is untouched — compaction is send-side only.

When it fires: (a) `maybeAutoCompact` before each turn's first send;
(b) once-and-retry after a too-long 400/413 (second failure is terminal);
(c) manually via `/compact`. Deliberately **not** inside the tool loop:
a mid-turn summary doubles that round's latency and rewrites bytes the
prefix cache was holding. The turn after a huge upload starts small because
the *next* turn's pre-send check compacts.

Cache interplay (worth understanding once): server-side prefix caching keys
on exact leading bytes. Prune-only rounds keep bytes stable, so the settled
prefix hits cache; summaries and threshold crossings re-fill it. On
`/messages` we additionally pin three explicit `cache_control: ephemeral`
breakpoints (system block, last tool, last message block) — the documented
Anthropic pattern: stable prefixes cache, the moving tail doesn't.

## 6. Batching and parallelism

- One round's batch runs concurrent (semaphore 8); results append in
  request order; `tool_start`s emit in order, then `tool_result`s in order,
  so the TUI timeline stays readable under concurrency.
- Batches containing `AskUserQuestion` go fully serial (one interactive
  overlay/stdin). File-access approval prompts serialize the same way.
- MCP calls lock **per server** (one stdin pipe each); cross-server batches
  overlap; builtins never lock.
- The model must be *told* to batch: system prompts demand independent
  calls in one block and `grep`/`glob`-before-`read`. One-tool-per-round is
  the failure mode that turns §2's table quadratic — batching is a prompt
  feature with token-math consequences, not politeness.
- Not overlapped (documented limit): tools start after the round's terminal
  SSE event. Chat/responses SSE has no per-call completion signal, so true
  mid-stream execution is infeasible there; only `/messages`
  `content_block_stop` could hook it (TODO D5).

## 7. Tooling

Each tool is `Name + Description + JSON Schema + Fn(ctx, argsJSON) → string`.
The registry enforces: name shape, no duplicates, insertion-order
determinism (stable request bytes → stable cache prefix), `{}` for empty
args, errors-as-output (never crash the loop), 16k truncate.

| Tool | Contract notes |
| --- | --- |
| `shell` | `sh -c` in workdir root, 60s, stdout+stderr combined; `[exit N]` is content. Foreground-only (background deferred, TODO D3). |
| `read`/`write`/`edit` | Workdir sandbox; escapes ask via the active `AskUserQuestion` frontend (remembered per path), denied headless. `read` on dirs errors (use `glob`). `edit` needs exact `oldString`, refuses ambiguity without `replaceAll`. |
| `glob`/`grep` | 8-worker fan-out, sorted deterministic output, skips `.git` + dependency/build dirs. `grep` = Go regexp (not ripgrep syntax — a real behavioral difference from opencode). |
| `web_search`/`web_fetch` | DuckDuckGo HTML + HTML→text; non-200s are errors the model sees, not pages. |
| `AskUserQuestion` | Options + type-your-own + confirmation; formatted answers return as one tool result on all transports. |
| `todowrite`/`todoread` | §8. |
| `task` | §8. |
| MCP `<server>__<tool>` | stdio servers, parallel startup, serial-in-config-order registration, 30s handshake / 2m calls, failures warn-and-continue, results flattened to text, `isError` surfaces as `error:`. |

## 8. Todo writing vs task delegation (not a double-up)

opencode ships **both**, and so do we — they do different jobs:

- **`todowrite`/`todoread` = the plan surface.** An in-memory checklist
  (opencode schema: content/status/priority) that tells the model *and you*
  what the steps are and which one is `in_progress`. It costs almost nothing
  (one small JSON result), breaks chunky work into verifiable steps, and
  renders as a glyph checklist (`☑ ◐ ☐ ✕`) in the scrollback next to
  thinking — that rendering is why opencode "shows a todo list within a
  job". Rule: exactly one `in_progress`, mark `completed` only after
  verification, update after every step. Resets on `/clear`/`/new`,
  restored from the session log on resume.
- **`task` = the execution surface.** A same-model subagent (`explore` =
  read-only subset, `general` = file+shell subset) running the full tool
  loop against an **isolated history**, whose *summary alone* folds back
  into the main turn. It costs one model loop but saves `rounds × full
  outputs` in main history. Foreground-only, ≤25 rounds, depth ≤2, silent
  in the timeline (one `task` row; live subagent streaming is TODO D6).

Mental model: **todos track the work, task does the work elsewhere.**
A 5-file refactor goes: `todowrite` (5 items) → `task(explore: map the
call sites)` → summary lands → edit per item, ticking each `completed` →
final answer. Without todos the model re-explores; without task the
exploration bloats main history. If the model skips `todowrite`, the system
prompt's MUST-first rule plus the checklist rendering are the two levers —
there is no third; a missing plan is a prompt-compliance failure, not a
missing feature.

## 9. Evaluation: the guards that judge every round

- `checkContextBudget` — fail-fast before a doomed send (history + schemas
  vs `max_context_tokens`), with the exact estimate and a remediation
  (`/compact`, `/clear`, raise the knob).
- `doomLoopTool` — same single tool+args a third consecutive time →
  abort with guidance. Multi-call batches don't count (parallel unit).
- Stall guard — `max_stagnant_rounds` (default 25) rounds with tool calls but
  no `write`/`edit`/`task` → abort ("exploring without acting"). Shell
  doesn't reset it. Raise for long research turns.
- Batching nudge — 3 consecutive single-call rounds append one ephemeral
  user reminder to batch independent calls; stripped at turn end.
  Question rounds pause both counters.
- Retry — 429/502/503/504 + network errors, 3 attempts, 300ms→800ms
  backoff, bodies drained for connection reuse, `Retry-After` not yet
  honored (TODO D1). 400s never retry (stream-fallback owns that path).
- `friendlyError` — every failure maps to one line (`Rate limited`,
  `Overloaded`, `Cancelled`…) with details one key away; the prompt is
  retained so limits and cancels are retryable.
- Overflow recovery — one compaction + one turn retry on too-long 400/413,
  then terminal. Summarizer failure never kills the turn (budget check
  still guards the next round).

## 10. Transports at a glance

History stays OpenAI-shaped; each round converts and folds back, so TUI,
log, and registry are transport-agnostic. Model ID selects the transport
(`modelTransport`): `grok-`/`gpt-`/`muse-spark-` → `/responses`,
`minimax-`/`qwen` → `/messages`, everything else → `/chat/completions`.

| | chat | messages | responses |
| --- | --- | --- | --- |
| Thinking knob | `reasoning_effort` / `enable_thinking` (family-dependent) | `{type: enabled, budget 1024…6144}` / `disabled` for `none` | `{effort}` verbatim; `none`/empty omitted (Muse Spark 400s on `none`) |
| Tool results | `role: tool` (2k-budgeted copy) | user `tool_result` blocks (2k + cache breakpoint on last) | `function_call_output` (2k) |
| Reasoning continuity | full (native) | thinking folds to `<think>` (signatures dropped — TODO D7) | summaries fold to `<think>`; items omitted (provider-encrypted replay infeasible — TODO D4) |
| Caching | server prefix over stable bytes | + 3 explicit breakpoints | server prefix over stable bytes |
| Answer cap | provider default | `max_tokens` (16384) | provider default |
| Unknown thinking | full vocabulary until a 400 teaches the list, cached to disk | same | same |

## 11. TUI lifecycle (where the pixels come from)

`submit` → instant echo + busy + background `runTurn` → `tool_start` /
`text_delta` / `thinking_delta` / `tool_result` events accumulate into
`pendingTools` / `pendingText` / `pendingThinking` (throttled ~60ms,
viewport pinned) → `turnDoneMsg` commits the `Turn` (JSONL append, history
swap, totals) and clears pending. Rendered committed turns cache
append-only (key: count, width, thinking toggle, model), so streaming costs
the live block only. `todowrite` rows render the checklist live from args
while running, from the stored result on commit. Overlays (history,
sessions, tools, errors, help, question, model picker) are views over the
same state; Esc/Ctrl-C cancels the run context (turn commits as
`Cancelled`, app stays alive); Ctrl-C/D at idle quits.

## 12. Session lifecycle (across runs)

Each session is one append-only JSONL (`$XDG_DATA_HOME/valen/sessions/<id>.jsonl`)
plus a `.meta.json` sidecar (title = first prompt's first 60 runes,
model/effort). `/sessions` (Ctrl+L) lists every session with title and
turn count; Enter resumes (transcript + history rebuilt, same file
continues, last `todowrite` plan restored). `/new` (Ctrl+N) mints a fresh
id; `/clear` reseeds history in place (log file kept, never deleted).
Compaction never touches disk — resume always sees the full lossless log
and re-derives the plan from the last `todowrite` record.
