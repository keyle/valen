# Design: harness

Verified against `e48a948` on `master`. Repo was clean; no worktree yet.
`go build ./...` passes. (A grep showed `10*[PHONE]` at
`model_thinking.go:186`. That is a display artifact, not source. Do not
chase it.)

## Verified facts

- **extra_body vs early return.** `chatWireBody` (`chat.go:137`) returns `req`
  unchanged when no provider, pad or mirror patch is needed (`chat.go:148-149`).
  extra_body must bypass that return. It applies to the chat transport only.
  Messages (`messages.go`) and Responses (`responses.go`) ignore it. OpenRouter
  models use chat (`models.go:215-217`).
- **Flag parsing.** `reorderFlags` takes its value-flag map at `main.go:104-106`.
  `--history` must be added there, or the path is read as the prompt.
- **Registry.** `Add` (`tools.go:44`) and `Has` (`tools.go:58`) exist. There is
  no `Remove`. `LoadBuiltins(cfg.Tools)` (`main.go:205`, mapping at
  `tools.go:99`) can re-add `AskUserQuestion`, `todowrite` and `todoread` from
  config. The always-present adds are at `main.go:209`, `:217` and `:233`
  (`task`). Gates must run after `LoadBuiltins` and cover all of them.
- **Events.** `Event` is at `turn.go:19`. Emit sites: `tool_start` at
  `agent.go:163` and `:220`, `tool_result` at `agent.go:207` and `:232`.
  `round_start` is at `agent.go:259`. `retry` (`retry.go:108`) and `or_meta`
  (`chat.go:291/313/344/349/439`) are emitted but are not in the spec.
  `thinking_delta` is not in the spec either.
- **text_delta.** Only SSE rounds emit it (`chat.go:269-276`). A unary reply
  emits none, so `done.reply` is the only text in that case.
- **Stdout answer.** `chat.go:566` prints `reply.Content`. The session records
  the think-split `answer` (`chat.go:536`, `chat.go:543`, `think.go:9`). The
  stdout answer and `done.reply` should be the split answer. Needs a test with
  a thinking-bearing reply.
- **Parallel tool events.** Results are collected in the caller's loop
  (`agent.go:202-209`), so `tool_result` comes from one goroutine. Keep a mutex
  on the NDJSON writer anyway.
- **TUI gate.** `tuiMode = qMode || (!printMode && isTTY(stdin) && isTTY(stdout))`
  (`main.go:151`). `--stream` must set `printMode`, or a PTY caller gets the TUI.
- **Sessions.** Every headless run writes a session (`chat.go:492-498`,
  `newSession` at `session.go:105`). Pruning keeps `saved_sessions` = 50 per
  folder (`config.go:22`, `session.go:297`).
- **System prompts.** Most family prompts name tools the flags can remove:
  `prompt_default.txt:30` (`todowrite`), `prompt_meta.txt:44` (`AskUserQuestion`),
  and also anthropic, beast, codex, copilot, gemini, gpt, gpt_astra, kimi, plan
  and trinity (`internal/prompts/`). The voice profile sets `system`, so it is
  covered. A job with no `system` is not.
- **Stdin.** `readPrompt` (`main.go:347-362`) reads all of stdin when it is not
  a TTY. Removing the AskUserQuestion tool stops it from reading stdin.
- **Config validation.** `config.go:220` rejects a thinking level that is not in
  `validThinkingLevel`. `models.go:341-342` lists deepseek-v4-flash as
  `{low, high, max}`.

## S3.5 — why the existing probe can't answer it

`probeThinkingLevels` (`model_thinking.go:148`) returns early from the cache or
static table (`:150`, `:153`). It never asks the API. It also sends `__probe__`
as the effort (`:156`). So the probe cannot test `minimal`. The spike is a direct
request with `reasoning_effort: "minimal"`, the same field the probe sends
(`:169`). It is a manual spike, not a `go test` (AGENTS.md: no real network in
tests). It needs an OpenRouter key from the user; `.env` cannot be read.

## Decisions (defaults, reversible)

- **D1** `--stream` implies `-p`.
- **D2** NDJSON carries only the spec kinds: `round_start`, `text_delta`,
  `tool_start`, `tool_result`, `error`, `done`. Drop `thinking_delta`, `retry`
  and `or_meta`.
- **D3** `tool_result` omits `output` in v1. There is no flag to ask for it.
- **D4** `done.messages` = this turn's new messages, including the user prompt,
  in the same JSON shape `--history` reads. The caller appends them and passes
  the array back next time. This makes the round-trip test meaningful.
- **D5** `--history`: JSON array of `openai.ChatCompletionMessage`. Read through
  an 8 MiB `LimitReader` (new cap, add to `openspec/project.md`). Drop
  `system`-role entries, since the run's system prompt wins. Bad JSON exits 1
  before any model call.
- **D6** Removal happens after `LoadBuiltins`, via a new `Registry.Remove`. It
  deletes from the map and the `names` slice.
- **D7** `--one-shot-task` = `-p` + `--no-question` + `--no-todo`. `task` stays
  (as the voxtral spec says). Exit 0 on success, 1 on error.
- **D8** extra_body reserved keys: `model`, `messages`, `stream`,
  `stream_options`, `tools`, `provider`, `reasoning_effort`. The spec lists the
  first four. valen also writes the last three. A skipped key logs a warning to
  stderr.
- **D9** SIGTERM: emit `done` with `error: "cancelled"` if possible, then exit
  non-zero. Use the existing signal context at `main.go:201`.
- **D10** `buildRegistry(cfg, opts)` is extracted from `start()`. `start()` reads
  globals and calls `os.Exit`, so the registry logic can't be tested without
  this.

## Open questions (decide before S3.1 lands)

- **Q1 Session pollution.** `--one-shot-task` must write session JSONL (spec).
  Voice and job turns will push TUI sessions out after 50. Options: (a) accept;
  (b) skip persistence for `--stream` only; (c) separate session dir.
  Default: (a).
- **Q2 Removed tools named in family prompts.** Options: (a) callers set
  `system` in their config; (b) valen appends a "tools unavailable: ..." line
  when the flags remove tools. Default: (a), and document it.
- **Q3 tool_result output.** Default: omit in v1 (D3). Add a flag later only if
  the caller needs it.

## Results (fill in)

- S3.5 `minimal` probe: pending (needs key). Record status and error text.
