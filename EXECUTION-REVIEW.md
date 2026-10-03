# Execution Review

Source review, 2026-10-03. This compares execution semantics, not model quality
or measured speed. No live-provider benchmarks were run. Historical causes
below refer to the pre-rewrite behavior (notably local `3f85109`). The specs
also document the requested HTTP deadline and task prompt/answer contracts;
those are concurrent implementation work, not completed source-verified fixes
at this review snapshot. Other current behavior follows the inspected source.

## Prioritized Findings

1. **P1: Working-set loss made investigation repeat instead of advance.**
   The old `compactionKeepReadsIntact=2` turned a four-file read into only two
   usable file bodies, even within one prompt. Separately, the 2k send cap
   discarded fresh grep/shell evidence. Current `pruneOldToolOutputs` protects
   the latest two genuine user turns plus a 40k-token older-output shield;
   `fitSendBudget` preserves fresh registry-capped text. The 2k limit belongs
   to older output pruning and summary serialization, not every model send.
   Sources: `compaction.go:150`, `compaction.go:306`; historical
   `3f85109:compaction.go` (`fitSendBudget`, `pruneOldToolOutputs`).
2. **P1: Partial/error responses could be mistaken for actionable success.**
   Stream error state could be overwritten by a later fold; EOF or partial
   calls were not a trustworthy completion boundary. Current parsers return
   provider/scan errors before folding, require terminal SSE completion and
   validate complete calls before any tool executes. Thinking without a
   visible answer or tools is an error. Sources: `main.go` (`chatRoundStream`,
   `validateModelReply`), `messages.go` (`messagesRoundStreamDetailed`),
   `responses.go` (`responsesRoundStreamDetailed`, `responsesResponseError`).
3. **P1: Replay and resume dropped execution state.**
   Display `<think>` flattening lost native reasoning continuity; old resume
   reconstructed prompt/reply text without tool evidence, and history-length
   slicing lost tool records after compaction. Active-loop native replay now
   retains Responses encrypted reasoning/phases and Messages signed/opaque
   blocks. Event-captured tools plus `Turn.History` checkpoints retain failed
   work and compacted continuation; legacy tools reconstruct call/result
   pairs. Native Messages/Responses metadata still does not survive the loop,
   later turns, overflow redispatch or resume. Chat `ReasoningContent` does.
   Sources: `responses.go:114`, `messages.go:201`, `tui.go:818`,
   `tui.go:2290`, `session.go:151`; `session_regression_test.go`.
4. **P1: No body deadline allowed an apparently hung turn.**
   A byte cap is not a time cap: a provider can send headers then stop, and
   even a small retry-body drain can block. The concurrent HTTP fix specifies
   `request_timeout_seconds=300` per attempt including SSE/unary body reads,
   60s headers, 10s dial/TLS, and no blocking retry-body drain. Retries get
   fresh deadlines; this does not bound an entire tool-rich turn or detect
   stream idleness earlier. At inspection, `requestTimeout` existed but was
   not yet wired through body reads, and retry draining remained.
   Sources: `main.go` (`requestTimeout`,
   `sharedTransport`), `stream.go` (`clientDoWithRetry`, `postAPI`).
5. **P2: Synthetic intervention changed the task and could suppress edits.**
   Mutation-based stagnation treated valid research as failure; batching/act/
   claim nudges spent extra model rounds, and no-tools recovery could decline
   requested edits while returning text as success. `runModelTurn` now owns
   one policy across transports, adds no synthetic nudges, and limits actual
   identical ordered batches (`max_stagnant_rounds`), with explicit errors.
   A final prose claim alone is not verification; checking the work remains
   the model/user's responsibility. Sources: `main.go:719`,
   `internal/guard/guard.go` (legacy notices only in the active loop).
6. **P2: Unrestricted batch concurrency made dependent work unsafe.**
   Reads, edits, shell, delegation and MCP are not interchangeable independent
   jobs. Only known read-only builtin batches overlap (max 8); any other tool
   makes the whole batch ordered serial. Result Events arrive as completed,
   while history stays request-ordered. Current config inheritance is wired;
   the concurrent task fix adds `effectiveSystem` plus kind instructions and
   visible-answer-only output (inspected `runTask` still used kind-only system
   text and returned `msg.Content`). Sources: `main.go:634`,
   `main.go` (`currentTurnConfig`), `task.go`
   (`runTask`).
7. **P2: Live display could conceal progress or misattribute it.**
   Old 32k accumulation froze at the cap and inserted newlines between token
   fragments. Current buffers keep a rolling 32768-byte UTF-8-safe tail and
   concatenate exactly; successful commit renders the full answer. Historical
   model/effort attribution belongs to the originating turn, not today's
   selection. Sources: `tui.go:2106`, `tui.go:1397`; `tui_test.go`.

## OpenCode Comparison

Upstream pinned to `907b3bc518fa48e90e8ec24dd327d13eee71c36c`:

- [`packages/opencode/src/session/compaction.ts`](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/session/compaction.ts):
  `prune` skips the recent user-turn region, then protects 40k tool-output
  tokens; it only marks output compacted when reclamation exceeds 20k and
  exempts `skill`. `TOOL_OUTPUT_MAX_CHARS=2000` is summary serialization, not
  a universal fresh-output cap. Tail `select`/`splitTurn` can split a large
  turn; Valen deliberately keeps whole turns/current request, so its tail
  target is soft and a lone oversized turn can fail fast. The upstream prune
  boundary is not byte-for-byte identical to Valen's two-whole-turn shield.
- [`packages/opencode/src/session/processor.ts`](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/session/processor.ts):
  `handleEvent` persists text/reasoning/tool state and provider metadata as
  events arrive. `step-finish` records finish/usage and checks usage-based
  overflow; `cleanup` marks unfinished tools interrupted, and `halt` records
  failures. Doom-loop detection requests permission rather than Valen's hard
  stop. Valen's shared local loop is smaller, but end-of-turn JSONL checkpoints
  are not equivalent to this per-part durability or usage accounting.
- [`packages/opencode/src/session/message-v2.ts`](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/session/message-v2.ts):
  `toModelMessagesEffect` carries same-model provider metadata/reasoning,
  preserves signed-thinking separators, reconstructs completed/error tools,
  and supplies interrupted results for pending/running tools. Truncation is
  opt-in (`toolOutputMaxChars`). `filterCompacted` restores summary plus
  retained tail. Valen preserves native replay inside an active loop only;
  ordinary history/checkpoints cannot claim this native persistence parity.

## Remaining Work

1. Persist transport/model-tagged native signed blocks and encrypted items,
   with prefix-safe ordering across later turns, redispatch and resume.
2. Complement estimates with provider usage, native replay overhead and
   output headroom; expose synchronous compactor usage/latency. The shipped
   compactor now uses `medium` while main thinking stays `high`, a config
   choice to reduce summary work, not a measured speedup.
3. Add optional stream-idle control and Retry-After-aware backoff. Per-attempt
   deadlines do not provide a whole-turn budget, and tools still have their
   own timeouts.
4. Define safe very-long-single-turn compaction without breaking tool
   transactions or signed prefixes. Until then, whole-current-turn protection
   can produce an honest context-limit failure. Summaries remain lossy and
   durable checkpoints are end-of-turn, not crash-time journals.

Offline regression fixtures cover implemented paths; this review does not
assert live latency, throughput, memory measurements, or upstream test runs.

## Verification Snapshot

Targeted config, compaction effort, latest-two-turn pruning, Messages native
replay, session checkpoint and rolling-tail tests passed. `gofmt -l .` was
clean and `go vet ./...` passed. `go test ./...` exposed three stale assertions
in concurrent code work: `TestRunToolCallsParallelDv` expects unknown-tool
parallelism, `TestChatFallsBackWhenStreamRejected` expects a fresh stream
probe every round, and `TestTUIPendingTextDeltasRenderOnOwnLines` expects
inserted newlines. This documentation-only change does not alter those tests
or claim a green full-suite gate.
