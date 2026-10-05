# LIFECYCLE — how a complex task flows through valen

A step-by-step reading guide to the harness side of models: what a turn is,
what a round is, where tokens go, why big jobs slow down, and what each
mechanism (batching, tools, todos, compaction, guards) costs and buys.
Caps below are code defaults; token estimates and cost examples are
illustrative, not measured latency or live-provider benchmarks.

Terms first, because the codebase uses them precisely:

| Word | Meaning | Code |
| --- | --- | --- |
| **turn** | One user prompt → final assistant answer, including all tool work. The unit billed, logged, and resumed. | `Turn` in `session.go`, `runTurn` in `main.go` |
| **round** | One model request → response inside a turn. A turn with 5 tool batches = 5+ rounds. | shared `runModelTurn`; transport closures in `runChatTurn` / `runMessagesTurn` / `runResponsesTurn` |
| **step** | opencode's word for roughly one round (its processor emits `step-start` / `step-finish` events around each model call plus its tool calls). Valen has no step events; round is the equivalent unit. | opencode `session/processor.ts` |
| **batch** | Tool calls emitted in one round. Read-only builtin batches may overlap; other batches execute serially. Results append in request order. | `runToolCalls` in `main.go` |
| **history** | OpenAI-shaped continuation state carried across turns; older turns may be pruned/summarized. Separate session records keep the transcript and tool outputs. | `[]openai.ChatCompletionMessage`, `Turn.History` |

## 1. The lifecycle of one user turn, step by step

Say you type "add retry with backoff to the fetcher and cover it with tests"
and press Enter in the TUI. What happens, in order:

1. **Submit (UI thread).** The TUI echoes your message instantly, snapshots
   history (copy + your message), flips to busy, and fires a background
   command. The UI thread never blocks on the network; it only renders
   progress events. (`submit` in `tui.go`, `toolEventMsg` / `turnDoneMsg`.)
2. **Pre-send compaction check.** Before the first request,
   `maybeAutoCompact` runs when auto-compaction is enabled: cheap local prune
   (see §5), plus a summarizer call if older turns are compactable and history
   is past threshold. (`dispatchTurn`.)
3. **Round 1 request.** The active transport converts history to its wire
   format and POSTs with `stream: true`. Every request carries
   `x-opencode-session` (routing + cache affinity) and the honest
   `valen/<version>` User-Agent. System + tool schemas ride on every round.
   Each HTTP attempt is bounded by `request_timeout_seconds` (300s including
   body reads); dial/TLS limits are 10s, response headers 60s.
4. **Stream parse.** SSE deltas (`text_delta`, `thinking_delta`) forward to
   the UI live (~60ms throttle, viewport pinned). Tool calls accumulate until
   a valid terminal event (Chat `[DONE]` or finish reason,
   Responses `response.completed`, Messages `message_stop`).
   A provider answering unary JSON folds through the same validation with no
   deltas; a 400 blaming streaming retries the round once without it. Errors,
   incomplete streams/blocks, invalid tool arguments and thinking-only
   tool-less replies fail instead of executing partial work.
5. **Append assistant message, run the batch.** Tool calls execute —
   parallel up to 8 only for entirely read-only builtin batches, otherwise
   serially in request order. Result events arrive in completion order;
   history appends every result in request order. Per-round local prune
   protects the latest two user turns plus 40k tokens of older outputs.
6. **Rounds 2..N.** Repeat 3–5. Each round re-sends the *entire* grown
   continuation history plus schemas: this is the `rounds × history` cost
   from §3. `runModelTurn` does not insert synthetic nudge/recovery rounds.
7. **Finish.** A completed round with a visible answer and no tools ends the
   turn: reply renders as markdown, session totals refresh once in the
   status row above the chatbox (`N turns · win W/B · ↑P ↓C`, in thousands), the `Turn`
   (prompt, thinking, reply, Event-captured tools, usage, model/effort and
   continuation checkpoint) appends to JSONL, UI goes idle. Errors/cancel
   also commit completed work and history; this is end-of-turn saving, not
   a guarantee against a process crash or disk-save failure.

Headless `-p` uses the same `runTurn`/`runModelTurn`; its sink prints stderr
tool traces rather than a TUI timeline. Session checkpointing is TUI-owned.

## 2. Rounds vs turns, and why the count matters more than it looks

Cost of a turn ≈ sum over rounds of (prefill(history + schemas) + decode).
History grows each round (assistant message + N tool results). Without
compaction, replayed input can grow quadratically with round count: round k
re-sends everything rounds 1..k-1 produced. Actual latency and billing depend
on the provider's prefix caching and decode; no fixed crossover is assumed.

Illustrative example (not a benchmark): a plan, four reads, two edits and a
final answer sent one batch per round require eight model rounds. If each
read contributes 10KB (about 2.5k estimated tokens), all four remain visible
throughout that user turn; the third read no longer discards the first.

| Round | Continuation history | Approximate wire input |
| --- | --- | --- |
| 1 | system + prompt + schemas (~8k) | ~8k |
| 2 | + todowrite result (~1k) | ~9k |
| 3-6 | + four reads, about 2.5k tokens each | ~11.5k, ~14k, ~16.5k, ~19k |
| 7-8 | + edits + final answer request | ~19k plus edit results |

These figures exclude envelopes, native metadata and provider caching.
Batching independent reads cuts model round trips without discarding the
working set. There is no secondary 2k send cap: all fresh outputs retain the
16k-byte registry cap. `task` isolates exploration instead of repeatedly
uploading it in main history.

Hard stops: `max_rounds` (default 300) ends a turn's first window without an
answer; `runTurn` then compacts send-side and grants exactly one more window,
so long jobs continue instead of dying at the cap. A second exhaustion is
terminal ("across 2 compacted windows");
`doomLoopTool` aborts when the *same single* tool call repeats a third
consecutive time (multi-call batches break that single-call chain).
`max_stagnant_rounds` (25) independently bounds consecutive identical ordered
batches, not rounds without writes; changed calls reset it.

## 3. Token math: what a request is actually made of

One round's prefill, largest-first:

1. **History content**: assistant text, user prompts and registry-capped tool
   results, with older unprotected outputs pruned. Active Messages/Responses
   replay also carries native blocks/items, not just display text.
2. **Tool schemas** — every builtin + every MCP server's tools, every round.
   Three builtins ≈ 300 tokens; a couple of MCP servers can be thousands.
   The budget guard counts this (`requestToolsTokens`); the old estimator
   didn't, which is why compaction used to fire late.
3. **System prompt** — a few hundred tokens, every round.
4. **JSON envelope** — roles, tool-call IDs, structure ≈ 40 tokens/message
   (`messageOverheadTokens`).
5. **Thinking/reasoning config** — small field, but it *authorizes* large
   decode: `xhigh` lets the model spend far more output tokens per round
   than `medium`, within the explicit output cap.

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
| `max_rounds` | 300 | Per-turn backstop per window (one compact-and-continue extension), not an expected task duration |
| `max_stagnant_rounds` | 25 | Consecutive identical ordered batches allowed; not mutation-based stagnation |
| `max_context_tokens` | 0 = auto (model window or 300000) | Approximate fail-fast budget; auto resolves the model's own input window when known, else the safe default — not a guarantee against every provider's limit |
| `max_tokens` (all transports) | 16384, no floor for positive overrides | Explicit output bound, Responses uses `max_output_tokens` |
| Tool output | 16000 bytes in registry, same fresh wire output | UTF-8-safe cap; session tool records keep this output |
| Compaction `keep_tokens` | 15000 soft whole-turn target | Entire current turn protected even when oversized |
| Compaction `buffer` | 10% of budget | Headroom so the summary + tail + reply fit under the fail-fast line |
| Compaction threshold | min(budget−buffer, keep×3) → **45000** default | Compacts early and often instead of near the limit |
| Prune: recent turns | latest 2 genuine user turns intact | Keep the working set, not just the last two reads |
| Prune: older shield | 40k tool-output tokens beyond protected turns | Reads and other outputs share this shield |
| Prune: decisions | never prune | `AskUserQuestion` answers are load-bearing; summary serialization keeps up to 4k chars |
| Old output / summary serialization | 2000 chars per ordinary tool result | Older reads become descriptors; this is not a live-send cap |
| `read` default/max | 2000 / 5000 lines, ~12KB/call | opencode parity on lines; 12KB fits under the 16k storage cap so the offset-continue hint always reaches the model |
| `grep` | 100 matches, 20/file, skip >1MiB/binaries | One noisy file can't starve the result; same shape as opencode's limit: 100 |
| `shell` | 60s timeout, combined output | Non-zero exit returns as content (`[exit N]`), never as loop error |
| MCP handshake / call | 30s / 2m | Slow servers warn-and-continue at startup, never abort the run |
| Parallel tools | max 8 known read-only builtins | Any other tool makes the whole batch serial |
| Stream body cap | 8MiB LimitReader/round | A runaway stream can't OOM the harness |
| Retry | 3 attempts, 300ms→800ms, 429/502/503/504 + network errors | Overloads back off; 400s never retry (stream-fallback depends on it) |
| HTTP deadlines | 300s per attempt including body; headers 60s, dial/TLS 10s | Optional `request_timeout_seconds`; no blocking retry-body drain, no new dependency |
| Task subagent | ≤25 rounds, depth ≤2 | A flailing delegate can't eat the session budget |
| Web | search 8 results/1MiB; fetch 512KiB→8000 chars | Snippets stay plannable, pages stay skimmable |
| TUI stream tail | last ~1500 runes displayed; rolling 32768 bytes per text/thinking buffer; 60ms throttle | Exact fragments, no inserted newlines; newest UTF-8-safe tail retained, commit renders full reply |

Ditched along the way: 200-line reads (10 rounds to see a file), hardcoded
8192 answer cap, global MCP mutex, unbilled schema blind spot, mid-turn
blocking summaries.

## 5. Compaction, in full (the part everyone hand-waves)

Two mechanisms, different jobs:

**Prune (local, no model call).** With auto-compaction on,
`pruneOldToolOutputs` protects the latest two genuine user turns in full,
then another 40k estimated tokens of older tool outputs. Beyond the shield,
reads become path/range + next-offset descriptors and other results shorten
to 2000 chars with a prune marker. Decisions and call records never prune.
The third/fourth read in a current turn therefore cannot erase earlier reads.

**Summary (past threshold, one LLM call).** `splitForCompaction` keeps
leading system messages + a whole-user-turn tail verbatim; `keep_tokens` is
soft and the entire current turn always stays. Call/result pairs cannot be
split. The head serializes to handoff text (reads as descriptors, decisions
up to 4k chars, ordinary outputs up to 2k; private thinking omitted) and the
`[compaction] model` at `[compaction] thinking` with **no tools** returns
prose covering objective, decisions, completed/active work, blockers, next
moves, files. The head is replaced by one `[Session summary …]` assistant
message. Repeats *update in place* (previous summary feeds the prompt), so
blocks never stack. Prior disk JSONL records remain untouched. New turn
checkpoints capture the current compacted continuation state alongside full
transcript/tool records.

When it fires: (a) `maybeAutoCompact` before each turn's first send;
(b) once-and-retry after a too-long 400/413 (second failure is terminal);
(c) manually via `/compact`. Deliberately **not** inside the tool loop:
a mid-turn summary adds another synchronous model call and rewrites bytes the
prefix cache was holding. The next turn's pre-send check can compact older
work when there is a compactable head. A lone oversized turn
has no compactable head; protecting it means a context-budget/provider error
can still stop the run. Raising `keep_tokens` does not solve this limitation.

Cache interplay (worth understanding once): server-side prefix caching keys
on exact leading bytes. Unchanged protected prefixes may hit cache; pruning
older output and summary replacements change bytes and can miss it. On
`/messages` we additionally pin three explicit `cache_control: ephemeral`
breakpoints (system block, last tool, last message block) — the documented
Anthropic pattern: stable prefixes cache, the moving tail doesn't.

## 6. Batching and parallelism

- Only entirely read-only builtin batches (`read`, `glob`, `grep`,
  `web_search`, `web_fetch`) overlap, max 8. Starts emit in request order,
  results in completion order; call IDs match live rows. History always
  appends in request order after the batch finishes.
- Any mutating, interactive, stateful, `task`, MCP or unknown tool makes the
  whole batch serial in request order. Even cross-server MCP calls do not
  overlap within a batch. File-access approvals also serialize.
- The model must be *told* to batch: `prompt_meta.txt` / `prompt_default.txt`
  demand small purposeful batches (2–4 related calls) and
  `grep`/`glob`-before-`read`. One-tool-per-round is
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
  loop against an isolated history, inheriting the current turn config and
  `effectiveSystem` plus kind-specific instructions. Its visible final
  answer alone folds back (no private thinking or intermediate tool output)
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
- `doomLoopTool`: same single tool + normalized args a third consecutive
  time stops before executing it. Multi-call batches break that chain.
- `max_stagnant_rounds` (25): maximum consecutive identical ordered tool
  batches allowed; changed calls/args/order reset the count. Research is not
  judged by whether it mutates files. Exceeding a limit returns an error.
- No synthetic batching/act/claim challenges or final no-tools recovery.
  The shared `runModelTurn` uses the actual prompt and tool evidence;
  legacy guard notices are stripped on entry.
- Completion validation: a valid terminal response and complete tool calls
  are required; failed/incomplete output never triggers tools. A tool-less
  thinking-only response is an error, not a final answer.
- Plan mode — `plan` / `/plan` / `-plan` flips the turn read-only instead of
  guarding it: the plan overlay joins the system prompt, `write`/`edit`/
  `shell` refuse as tool output, `task` only accepts `explore`. The model
  investigates (small batches, todowrite-tracked) and the reply IS the plan;
  leaving plan mode prints the build-switch notice and restores the build
  prompt. The summarizer always runs with plan off.
- Retry: 429/502/503/504 + network errors, 3 attempts, 300ms then 800ms.
  Each HTTP attempt has its own body-inclusive deadline, retryable bodies
  close without blocking drain, `Retry-After` remains TODO D1. 400s do not
  transient-retry (stream fallback owns that path). No idle/whole-turn timer.
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
| Tool results | `role: tool`, fresh registry-capped text | user `tool_result`, same text + last cache breakpoint | `function_call_output`, same text |
| Active-turn reasoning | native `ReasoningContent` | native thinking/signature/redacted blocks replayed in order | native reasoning/encrypted content and output phases replayed, stateless |
| Beyond active loop / resume | native Chat reasoning checkpointed | display thinking stored, native blocks/signatures not persisted | display summaries stored, native items/encryption/phases not persisted |
| Caching | server prefix over stable bytes | + 3 explicit breakpoints | server prefix over stable bytes |
| Answer cap | `max_tokens` (16384 default) | `max_tokens` (16384 default) | `max_output_tokens` (16384 default) |
| Unknown thinking | full vocabulary until a 400 teaches the list, cached to disk | same | same |

## 11. TUI lifecycle (where the pixels come from)

`submit` → instant echo + busy + background `runTurn` → `tool_start` /
`text_delta` / `thinking_delta` / `tool_result` events accumulate into
`pendingTools` / `pendingText` / `pendingThinking` (throttled ~60ms,
viewport pinned) → `turnDoneMsg` commits the `Turn` (JSONL append, history
swap, totals, continuation checkpoint) and clears pending. Live text
buffers retain the newest 32768 bytes on UTF-8 boundaries, concatenate exact
fragments without invented newlines, and do not cap the committed reply.
Live thinking buffers share the same cap but start every addition on its
own line. Tool records come from Events, not a possibly compacted history diff.
Historical headers use each turn's model/effort; live model switches affect
future requests, not past attribution. Rendered committed turns cache
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
turn count; Enter resumes (latest `Turn.History` checkpoint plus any later
legacy turns, same file continues, last `todowrite` plan restored). Legacy
records without checkpoints reconstruct assistant tool calls and matching
results, not just prompt/reply text. `/new` (Ctrl+N) mints a fresh
id; `/clear` reseeds history in place (log file kept, never deleted).
Disk keeps every appended turn and checkpoint; memory retains only the latest
checkpoint. Compaction does not rewrite prior records; resume preserves the
compacted continuation rather than restoring all old output to the wire.
Failures/cancellations also checkpoint completed work. Native Messages
signatures and Responses encrypted reasoning remain active-loop-only, not
durable resume metadata. See `EXECUTION-REVIEW.md` for the pinned OpenCode
comparison and remaining gaps.
