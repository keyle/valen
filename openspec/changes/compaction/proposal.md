# Change: compaction

## Why

History grows unbounded (`max_rounds=9999`) and every round resends all of
it, until the provider 400s too-long. Deferred as perf 4.4. Opencode
summarizes everything except recent `keep.tokens` (default 15000) beside a
`buffer` (10%), manually via `/compact` and automatically near the limit,
retrying a too-long request once after compacting.

## What changes

1. `compactSession` (summary-mode only, no provider-native checkpoints):
   one `runTurn` with a summarizer prompt; token estimate = runes/4 (no
   tokenizer dep); config `[compaction] auto/keep_tokens/buffer/model`
   (opencode defaults plus an optional cheap `model` override — empty means
   the session model at minimal thinking, so large sessions stay cheap for
   the big model).
2. Send-side only: disk JSONL keeps everything (recall lossless); the kept
   window shortens long tool output and replaces attachment data with a
   description. File reads are disposable (tools re-read them): all but the
   newest 2 collapse to path/range descriptors, while user decisions never
   shrink — important context stays verbatim, code bloat is forgotten.
3. `/compact` (`/summarize` alias) forces it; auto triggers on too-long 400
   or estimated over-limit before send.

## Non-goals

- Native/encrypted provider checkpoints (pins provider+model); background
  summarization.

## Success criteria

- Long-history fixture compacts (recent kept, summary prepended, second
  compaction updates in place); `/compact` + auto-400-retry tests offline.
