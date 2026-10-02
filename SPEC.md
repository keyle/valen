# valen — spec

A single-binary Go agent harness for OpenCode Go models. No server, no runtime
dependencies beyond the binary: it talks to the Go transports
(`/chat/completions`, `/messages`, `/responses`), runs a tool loop locally,
and exposes tools plus any number of stdio MCP servers to the model.

## Goals

- Small, readable, dependency-light (4 direct deps: toml, go-openai, mcp-go,
  x/net/html for text extraction).
- Provider-agnostic: any OpenAI-compatible `chat/completions` endpoint works.
- Deterministic and testable: every layer is exercised offline by `go test`.
- Fail soft: a broken MCP server or tool must not abort the run.

## Non-goals

- Multi-agent orchestration.
- Sandboxing or permission prompts for `shell` — it runs commands as you.

## Layout

| File | Responsibility |
| --- | --- |
| `main.go` | CLI flags, config loading/defaults, HTTP identity headers, agent loop |
| `tui.go` | Bubble Tea chat UI: padded conversation, instant echo, recall views, Monokai theme |
| `models.go` | OpenCode Go catalog: live list, transport map, thinking levels, config persist |
| `messages.go` | Anthropic `/messages` transport: wire format, thinking, tool loop |
| `responses.go` | OpenAI `/responses` transport (stateless): wire format, reasoning, tool loop |
| `stream.go` | Shared streaming layer: raw POST, SSE scanner, unary-JSON fallback |
| `session.go` | Turn model + JSONL session log at `$XDG_DATA_HOME/valen/sessions/` |
| `compaction.go` | Summary-mode compaction (prune + cheap-model summary, send-side only) |
| `usage.go` | OpenCode Go plan quota (`GET /usage`) for the TUI footer |
| `errors.go` | Friendly one-line errors mapped from status codes/causes (ctrl+x has details) |
| `think.go` | Splits `<think>` blocks out of replies so the UI can collapse them |
| `tools.go` | Tool contract, ordered registry, builtin loader, output limits, `shell` |
| `files.go` | File builtins (`read`, `write`, `edit`, `glob`, `grep`): workdir sandbox, outside-access approval, glob matching |
| `ask.go` | `AskUserQuestion` contract, headless prompting/confirmation, answer formatting |
| `search.go` | `web_search` (DuckDuckGo HTML) and `web_fetch` (HTML → text) |
| `mcp.go` | stdio MCP client lifecycle, tool registration, result flattening |
| `config.toml` | Example configuration |

## Config schema (`config.toml`)

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `model` | string | — | **required.** Model ID, e.g. `glm-5.3-flash`, `kimi-k3`. Switch live with `/model` (ctrl+p); persisted to `config.toml` |
| `thinking` | string | empty (provider default) | Reasoning level, per-model (see `/effort` picker): muse-spark `minimal`/`low`/`medium`/`high`/`xhigh`, gpt/grok `low`…`max` (+`none` on gpt), glm/deepseek `low`/`high`/`max` or `high`/`max`, kimi-k3 `max` only, toggle models `none`/`high`, fixed no-control models omit. Switch live with `/effort` (`/thinking` alias) or ctrl+t (cycles low→high, wraps); persisted to `config.toml`. Unknown models fall back to the full vocabulary until a 400 teaches the valid list, cached at `$XDG_DATA_HOME/valen/thinking_levels.json` |
| `base_url` | string | `https://opencode.ai/zen/go/v1` | API base; the transport path (`/chat/completions`, `/messages`, `/responses`) is appended per model |
| `api_key_env` | string | `OPENCODE_GO_API_KEY` | Name of the env var holding the key, not the key itself |
| `session_id` | string | `ses-<unixnano>` | Stable per conversation; sent as `x-opencode-session` |
| `system` | string | built-in default (mirrors `config.toml`) | System prompt; empty uses the built-in default (helpful assistant, MCP-capable thinking agent with fast tool loopback, check AGENTS.md first). The shipped `config.toml` sets it explicitly — edit there to override |
| `max_rounds` | int | `8` | Hard stop on tool-loop iterations |
| `max_context_tokens` | int | `200000` | Fail-fast budget per round (est. runes/4): over-budget history aborts the turn with guidance instead of a doomed send |
| `temperature` | float | unset | Pointer semantics: unset omits the field, `0.0` sends zero |
| `compaction` | table | `auto=true, keep_tokens=15000, buffer=10%` | Summary-mode, send-side only (disk JSONL stays lossless): `auto` compacts before send when over budget and retries once after a too-long 400; `keep_tokens` recent tail kept verbatim beside the summary; `buffer` headroom below `max_context_tokens`; `model` cheap summarizer override (empty = session model with minimal thinking) |
| `[input]` | table | `blink=true, height=3` | Chatbox chrome (TUI only): block-cursor blink plus input box height in lines |
| `tools` | string[] | empty | Subset of `shell`, `web_search`, `web_fetch`, `read`, `write`, `edit`, `glob`, `grep`, `AskUserQuestion`. `AskUserQuestion` is also registered automatically when absent, so model-driven questions work in every frontend |
| `mcp` | table[] | empty | Each entry needs `name` + `cmd`; optional `args`, `env` |

Validation errors (missing `model`, MCP entry without `cmd`, unreadable file)
exit non-zero before any network call.

## Provider contract

Chat requests are `POST {base_url}/chat/completions` with
`Authorization: Bearer $KEY`. OpenCode Go additionally requires, on every
request:

- `x-opencode-session: <stable id>` — routing and prompt-cache affinity
- `User-Agent: <iPad Safari>` — a browser identity, not a generic SDK string
  (providers return fuller, cleaner responses to it; one const in `main.go`)

On chat both are stamped by `headerTransport` in `main.go`, so they survive
retries and apply to every call the OpenAI client makes. The `messages` and
`responses` transports stamp the same two headers per request, plus
`anthropic-version: 2023-06-01` and `x-api-key` on messages.

`GET {base_url}/usage` returns the Go plan quota (`rolling` / `weekly` /
`monthly`, each with `status`, `percent`, `resetsAt`). The TUI polls it at
startup, once a minute, and after each turn to render the footer; non-200s and
non-Go keys are ignored so the footer simply stays hidden.

Transport is selected per model by `modelTransport` (live Go table, Oct 2
2026): `grok-4.x`, `gpt-*-luna` and `muse-spark-*-contributor` are
`/responses` (`@ai-sdk/openai`); `minimax-*` and all `qwen*`
(`qwen3.8-max`, `qwen3.8-flash`, `qwen3.7-plus`) are `/messages`
(`@ai-sdk/anthropic`); everything else (`glm-*`, `kimi-*`, `deepseek-*`,
`mimo-*`, `longcat-*`, `hy*`, `space-bunny-free`) is `/chat/completions`.
History stays OpenAI-shaped in memory; the non-chat transports convert each
round and fold the reply back, so the TUI, session log and tool registry are
transport-agnostic.

- `messages` (`messages.go`): Anthropic wire format. `POST {base}/messages`
  with `model`, required `max_tokens` (8192), `system`, `messages`,
  `tools` (`input_schema`), `tool_choice: {type: auto}` when tools exist.
  Assistant `tool_calls` become `tool_use` blocks, tool messages become user
  `tool_result` blocks. Thinking blocks fold back as `<think>` so the UI
  collapse keeps working; `redacted_thinking` is dropped.
- `responses` (`responses.go`): OpenAI Responses, stateless (`store: false`,
  history replayed client-side, no `previous_response_id`). `POST
  {base}/responses` with `model`, `input` array, `instructions` (system),
  `tools` (`type: function` + `parameters`), `tool_choice: auto`.
  Assistant text before a call is tagged `phase: commentary` (Meta requires
  it); reasoning summaries fold back as `<think>`. Reasoning items are
  otherwise omitted in v1, so chain-of-thought continuity across turns is a
  follow-up.

Sampling params are omitted unless configured: some models reject a
`temperature` that differs from their fixed value. The same rule covers
`thinking`: empty omits the field (provider default). On chat, effort models
get `reasoning_effort` and toggle models get `enable_thinking` in
`chat_template_kwargs` (see `models.go`); fixed no-control models omit both.
On messages, `none` sends `thinking: {type: disabled}` and any other known
level sends `{type: enabled}` with a budget scaled low→high (1024…6144).
On responses, `none`/empty omit `reasoning` (Muse Spark rejects effort
`none` with 400); otherwise `reasoning: {effort: <level>}` passes through
verbatim. A 400 listing valid efforts is parsed and cached per model at
`$XDG_DATA_HOME/valen/thinking_levels.json`, so the next `/effort` and ctrl+t
use the learned list.

## Agent loop

```
messages = [system?] + [user prompt]
repeat up to max_rounds:
  resp = backend(messages, tools)   # chat | messages | responses per model
  messages += resp.message
  if no tool_calls: print content, print token tally, exit 0
  run tool_calls with bounded parallelism (max 8), append results in order
error: "stopped after N rounds without a final answer"
```

- Tool calls in one round run concurrently (semaphore 8) and their results
  append to history in request order, so wall-clock is ~max(latencies), not
  the sum. `tool_start` events emit in order, then `tool_result` in order.
  Batches containing `AskUserQuestion` run sequentially instead: the single
  interactive overlay/stdin cannot serve concurrent prompts. File-access
  approval prompts serialize on the same mutex. MCP tool calls serialize on
  a shared mutex (one stdin pipe per server); builtins run fully parallel.

- Assistant text goes to stdout; `[tool name]` traces and the token tally go to
  stderr, so `valen "..." > out.md` stays clean.
- `resp.Choices` empty is an error, not a panic.
- API errors are returned wrapped (`api: ...`) with the provider's message.
- Ctrl-C cancels the request context and unwinds MCP closers.
- Every round requests `stream: true` SSE. Text/thinking deltas stream to the
  frontend via `onEvent` (`text_delta`, `thinking_delta`) while tool calls
  accumulate per round. A provider answering with a complete unary JSON body
  folds through the same validation with no deltas; a 400 blaming streaming
  retries the round once without `stream: true`.
- `AskUserQuestion` is part of the same loop: the model may ask multiple
  questions, the active frontend collects answers (options, type-your-own, and
  confirmation), and the formatted answers return as an ordinary tool result.
  Transports encode that result with their existing converters (`role: tool`,
  `tool_result`, or `function_call_output`).
- Good-citizen guards run every round on all three transports:
  `checkContextBudget` fail-fasts when history exceeds `max_context_tokens`
  (estimated as runes/4, no tokenizer dep) instead of sending a doomed
  request into a 429; `doomLoopTool` aborts the turn when the same single
  tool call repeats a third consecutive time (multi-call batches break the
  chain). `maybeAutoCompact` keeps every send lean: prune runs unconditionally
  (cheap, local — file reads are disposable since tools re-read them, so all
  but the newest 2 collapse to path/range descriptors while user decisions
  never shrink and other outputs keep the newest 40k tokens), then the head
  is summarized past the threshold (`max_context_tokens` minus `buffer`, or
  `keep_tokens`×3 on huge budgets so sessions compact early and often) beside
  the verbatim `keep_tokens` tail — via the cheap `[compaction] model` when
  set, else the session model at minimal thinking, no tools. A too-long 400/413
  compacts once and retries the turn; the second failure is terminal. The summary
  updates in place on repeats, and `/compact` (`/summarize`) forces the same path
  manually from the TUI. Compaction is send-side only: the JSONL log keeps
  every turn. Both keep history and surface as friendly errors, so the user can
  redirect or `/clear`.

## Tool contract

```go
type Tool struct {
	Name, Description string
	Schema            json.RawMessage // JSON Schema object
	Fn                func(ctx context.Context, args string) (string, error)
}
```

Rules the registry enforces:

- Names must match `^[a-zA-Z0-9_-]{1,64}$`; duplicates are rejected.
- Insertion order is preserved, so `tools` in the request is deterministic.
- Empty/whitespace arguments are normalized to `{}` (models often omit args).
- Errors never escape the loop: `Run` converts them to `error: <msg>` tool
  output, which the model can react to.
- Output is truncated at 16000 bytes on a UTF-8 boundary with a
  `...[truncated N bytes]` suffix.

### Builtins

| Tool | Args | Behaviour |
| --- | --- | --- |
| `shell` | `command` | `sh -c` in the workdir, 60s timeout, combined stdout+stderr; non-zero exit is returned as content with `[exit N]`, not as an error |
| `web_search` | `query` | DuckDuckGo HTML endpoint, up to 8 results as `N. title / url / snippet`; unwraps `/l/?uddg=` tracker links; 1 MiB body cap |
| `web_fetch` | `url` | GET with 512 KiB cap, HTML tokenized to text (script/style/noscript dropped), truncated to 8000 chars |
| `read` | `path`, `offset`, `limit` | Line-windowed read (default 200 lines, max 5000); lines prefixed with their number for `edit` context; long lines trimmed at 1000 chars |
| `write` | `path`, `content` | Create/overwrite a file, creating parents; preserves the existing mode |
| `edit` | `path`, `oldString`, `newString`, `replaceAll` | Exact-string replace; refuses ambiguous multi-matches unless `replaceAll` |
| `glob` | `pattern`, `path` | Recursive file list supporting `*`/`?`/`[...]`/`**`; top-level subtrees walk in parallel, results sorted; skips `.git` |
| `grep` | `pattern`, `path`, `include` | Go regexp content search as `file:line: text`, sorted by file then line; directory walk collects candidates while file reads/matches run on a bounded worker pool (8); skips `.git`/binaries/>1 MiB files, capped at 100 matches (max 20 per file, so one noisy file can't starve others) |
| `AskUserQuestion` | `questions[]` | Presents each question with options plus type-your-own, confirms all answers, and returns `User has answered your questions: ...` |

File tools and `shell` share one sandbox root: the startup directory,
`-dir <path>`, or a positional directory in TUI mode
(`./valen ~/Code/something`). Paths escaping the root pause for approval
through the active `AskUserQuestion` frontend (remembered per path for the
session); without an interactive frontend they are denied with a clear
error, so the model can explain and ask.

Non-200 responses are errors, so the model sees `error: <url> returned 429 ...`
rather than an HTML error page.

## MCP

Each `[[mcp]]` entry spawns `cmd args...` over stdio with the parent environment
plus `env` overrides, then `initialize` → `tools/list`. Server startups run in
parallel; registration stays serial in config order so tool order is
deterministic. Tools are registered as
`<server>__<tool>`; names failing the tool-name rule abort that server only.

- Handshake (start/initialize/list) is bounded at 30s; tool calls at 2m.
- The handshake deadline is a child context, while the transport keeps the
  parent context — cancelling the handshake must not kill the connection.
- Results flatten to text: `TextContent` verbatim, other content types as JSON.
  `isError: true` results surface as `error: <text>`.
- A server that fails to start logs `mcp <name>: <err>` to stderr and the run
  continues with the remaining servers.
- All clients are closed on exit, including on error paths.

## Testing

`go test ./...` is fully offline:

- `main_test.go` — config defaults/overrides/validation, identity headers, and
  the agent loop against a fake completions server (tool round trip, tool
  failure feedback, max-round stop, empty choices, 402 surfacing, system prompt
  and tool list serialization, temperature omission).
- `tools_test.go` — ordering, name validation, unknown tools, truncation,
  argument normalization, shell success/exit-code/empty-command paths.
- `files_test.go` — workdir split, sandbox containment, `read`/`write`/`edit`
  round trip and error paths, `glob`/`grep` results, outside-access
  allow/deny via a stubbed prompter, shell running in the file root.
- `search_test.go` — result parsing, redirect unwrapping, result cap, no
  results, HTTP error, script/style stripping, truncation, text extraction.
- `mcp_test.go` — end-to-end stdio round trip: the test binary re-execs itself
  as a one-tool MCP server (`VALEN_MCP_FIXTURE=1`, see `TestMain`), covering
  registration, schema exposure, success, `isError`, bad arguments, and the
  warn-and-continue path for a server that fails to start.
- `messages_test.go` — `/messages` transport offline: history conversion
  (`tool_use`/`tool_result`), thinking mapping, tool round trip, request
  shape (path, headers, `max_tokens`, system, `input_schema`), temperature
  omission, Anthropic error surfacing, empty-content rejection.
- `responses_test.go` — `/responses` transport offline: input conversion
  (`function_call`/`function_call_output`, commentary phase), reasoning
  mapping, tool round trip, request shape (`store: false`, instructions,
  function tools), 429 surfacing.
- `models_test.go` — transport map per live Go table, per-model thinking
  levels, request wiring (`reasoning_effort` vs `enable_thinking`), config
  persist round-trip, `thinking` validation in `loadConfig`.
- `errors_test.go` — friendly mapping incl. `transportError` (messages 429,
  responses 401).
- `transports_test.go` — one model per transport pinned to its endpoint path
  (`/chat/completions`, `/messages`, `/responses`) through `chat()`.
- `stream_test.go` — SSE rounds per transport (text/tool deltas, usage,
  `stream: true` requested), stream-rejection fallback to unary, and the TUI
  live-transcript accumulation.
- `ask_test.go` — question parsing/formatting, headless option selection,
  type-your-own, confirmation/edit, the TUI overlay state machine, and one
  `AskUserQuestion` round trip per transport (`tool`, `tool_result`,
  `function_call_output`).

## Usage

```sh
export OPENCODE_GO_API_KEY=...
go run .                              # fullscreen TUI (ctrl+g for keys)
go run . ~/Code/something             # TUI rooted at another folder
go run . -dir ~/Code/something        # same, explicit (also works headless)
go run . -p "..."                     # one-shot answer on stdout
go run . -config other.toml < prompt.txt   # headless via pipe
```

TUI model controls: `/model [filter]` (ctrl+p) lists the OpenCode Go catalog and
switches; `/effort [level]` (`/thinking` alias) sets the thinking level for
the current model. Both persist to `config.toml`, so the last selection
sticks across runs; headless mode reads the same keys. The TUI footer also
shows Go plan usage (`5h` / `7d` / `30d`), yellow past 80% and pink at the cap.

## Extension points

- New builtin: add a `func() Tool` to the `builtins` map in `LoadBuiltins`.
- New transport: add a stream round in the style of `chatRoundStream` /
  `messagesRoundStream` / `responsesRoundStream` (SSE deltas via `onEvent`,
  unary-JSON body accepted, `isStreamRejected` fallback) and a
  `modelTransport` entry; `runTurn` dispatches on it.
