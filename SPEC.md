# valen — spec

A single-binary Go agent harness for OpenCode Go, OpenCode Zen and OpenRouter models. No server, no runtime
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
- Per-command permission prompts for `shell`. It is sandboxed by the OS instead (below).

## Layout

| File | Responsibility |
| --- | --- |
| `main.go` | CLI flags, TTY/client/transport setup, HTTP identity headers, `main`/`start` entry, prompt reading |
| `turn.go` | Turn orchestration: `runTurn`, per-transport dispatch, `Event`/OpenRouter metadata, request timeouts, key resolution |
| `agent.go` | Agent-loop internals: token budget, doom-loop guard, parallel tool fan-out, `runModelTurn`/`runChatTurn` |
| `config.go` | Config schema (`Config`/`TUIConfig`/`InputConfig`/`MCPServer`) with defaults and `loadConfig` |
| `chat.go` | Chat-completions transport: headers, wire body, unary + SSE rounds |
| `internal/prompts` | Agent guidelines: per-family `prompt_*.txt` (opencode-adapted, embedded) + `<env>` header + plan overlay; plan-mode ctx gating |
| `internal/guard` | Repeat-limit default and stripping of legacy ephemeral notices; execution policy lives in `runModelTurn` |
| `internal/dvlog` | Debug-verbose trace (`-dv`/`-dvlog`): timestamped round/tool/size log, off by default |
| `tui.go` | Bubble Tea UI model: state/message types, construction, view-mode, `runTUI` entry |
| `tui_update.go` | `Update` dispatch, `submit`, slash commands, plan-mode, open-selected, stream/pending mutation |
| `tui_key.go` | Key/mouse/CSI parsing and handling (`handleKey`, overlays, esc/ctrl/mouse-hold) |
| `tui_view.go` | Chrome rendering: `View`, header/status/footer/input, viewport + chat |
| `tui_render_turn.go` | Conversation rendering: committed-turn assembly/trim, turn headers, durations, copy helpers |
| `tui_select.go` | Transcript drag selection: ANSI-stripped cell grid, copy trimming that strips the common leading chrome while keeping code indentation (raw rendered text, vs `copy response`/`/copy` which copies the markdown source), reverse-video highlight, ctrl+click link opening |
| `tui_render_tool.go` | Tool rendering: summaries, edit/write diffs, tool + pending lines |
| `tui_stream.go` | Streaming text assembly (`appendStream*`, thinking deltas, round breaks) |
| `tui_lists.go` | Recall/picker/help/session list rendering and item builders |
| `tui_ask.go` | Ask-overlay state, prompts, key handling, answer formatting |
| `tui_layout.go` | Width/height math, input wrapping, markdown renderer, truncation |
| `tui_picker.go` | Model/effort pickers, favourite slots, thinking/routing switches |
| `tui_usage.go` | Footer quota/credits polling commands |
| `tui_session.go` | New-session, compaction trigger, transcript export |
| `models.go` | Model ids, transport map, context windows, thinking families, OpenRouter routing variants, request wiring |
| `model_thinking.go` | Thinking-level cache, error parsing, learning, and live probe |
| `model_catalog.go` | OpenCode Go/Zen/OpenRouter catalog fetching (live lists + pricing) |
| `messages.go` | Anthropic `/messages` transport: wire format, thinking, tool loop |
| `responses.go` | OpenAI `/responses` transport (stateless): wire format, reasoning, tool loop |
| `stream.go` | Shared streaming layer: raw POST, SSE scanner, unary-JSON fallback |
| `session.go` | Turn model + JSONL session log at `$XDG_DATA_HOME/valen/sessions/<workdir>-<hash>/` (one folder per working directory) |
| `compaction.go` | Summary-mode compaction (prune + cheap-model summary, send-side only) |
| `usage.go` | Quota for the TUI footer: Go plan quota (`GET /usage`) and OpenRouter balance (`GET /credits`) |
| `errors.go` | Friendly one-line errors mapped from status codes/causes (ctrl+x has details) |
| `think.go` | Splits `<think>` blocks out of replies so the UI can collapse them |
| `tools.go` | Tool contract, ordered registry, builtin loader, output limits, `shell` |
| `todos.go` | Session plan (`todowrite`/`todoread`, opencode schema, in-memory + session fold) |
| `task.go` | Foreground-only `task` subagent (explore/general, isolated history, depth 1) |
| `sandbox*.go`, `shellgate.go` | Shell sandbox: policy in `sandbox.go`, `sandbox-exec` (macOS), `bwrap` (Linux), other OSes fail to compile; once-per-session prompts for network and destructive commands |
| `files.go` | File builtins (`read`, `write`, `edit`, `glob`, `grep`): tool implementations, glob matching, background grep |
| `fileaccess.go` | File-access gate: workdir/worktree roots, outside-access approval, secret/data guards |
| `ask.go` | `AskUserQuestion` contract, headless prompting/confirmation, answer formatting |
| `search.go` | `web_search` (DuckDuckGo HTML) and `web_fetch` (HTML → text) |
| `mcp.go` | stdio MCP client lifecycle, tool registration, result flattening |
| `config.toml` | Default configuration, embedded in the binary and seeded to `~/.config/valen/` on first run |
| `userconfig.go` | Config dir (`$XDG_CONFIG_HOME/valen`, else `~/.config/valen`): first-run seeding, `config-ori.toml`, `state.json` (last model/thinking/routing) |

## Config location

Config lives in `~/.config/valen/` (`$XDG_CONFIG_HOME/valen/`), created on
first run: `config.toml` (yours; never overwritten), `config-ori.toml` (pristine
copy of the shipped default, refreshed on upgrade so you can diff), and
`state.json` (ephemeral state written by valen: last model/thinking/routing). Sessions
and caches stay under `$XDG_DATA_HOME/valen` (`~/.local/share/valen`).
`-config <file>` uses that file only: no seeding and no state overlay.

## Config schema (`config.toml`)

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `model` | string | — | **required.** Model ID, e.g. `glm-5.3-flash`, `kimi-k3`, `zen/claude-haiku-5-5`, or `openrouter/moonshotai/kimi-k3`. The `zen/` prefix selects OpenCode Zen (`https://opencode.ai/zen/v1`, same OpenCode key as Go; transport chosen per model: gpt/grok/muse → responses; claude and qwen3.5–3.8 plus/flash (all qwen except qwen3.8-max) → messages; qwen3.8-max and the rest → chat; Gemini models are not supported); the `openrouter/` prefix selects the OpenRouter provider (needs its key, below); bare IDs ride OpenCode Go. Switch live with `/model` (ctrl+p); persisted to `config.toml` |
| `thinking` | string | empty (provider default) | Reasoning level, per-model (see `/effort` picker): muse-spark `minimal`/`low`/`medium`/`high`/`xhigh`, gpt/grok `low`…`max` (+`none` on gpt), glm/deepseek `low`/`high`/`max` or `high`/`max`, kimi-k3 `max` only, toggle models `none`/`high`, fixed no-control models omit. Switch live with `/effort` (`/thinking` alias) or ctrl+t (cycles low→high, wraps); persisted to `config.toml`. Unknown models fall back to the full vocabulary until a 400 teaches the valid list, cached at `$XDG_DATA_HOME/valen/thinking_levels.json` |
| `routing` | string | empty (`none`) | OpenRouter provider-routing variant, sent as a model-slug suffix: `floor` (cheapest; `provider.sort: "price"`), `nitro` (fastest throughput; `provider.sort: "throughput"`), `exacto` (quality-first tool-use sorting), or empty/`none` (OpenRouter's default load balancing). Switch live with `/route [none\|floor\|nitro\|exacto]` or ctrl+y (cycles none → floor → nitro → exacto, wraps); a **silent no-op on Go/Zen**. Persisted to `state.json` alongside `thinking` and stored in favourite slots; ignored on non-OpenRouter models |
| `provider_ignore` | string[] | `["baseten"]` | OpenRouter `provider.ignore`: provider slugs skipped per request (`openrouter/…` models only; a silent no-op on Go/Zen). Unset keeps the default `["baseten"]`, an explicit `[]` disables the blacklist. Mutually exclusive with `provider_only` (which wins when both are set). The `provider` object merges with the `routing` suffix, which sets `provider.sort` |
| `provider_only` | string[] | unset | OpenRouter `provider.only`: an allowlist instead of the blacklist. Sent as the request `provider` object alongside `routing` |
| `opencode_key` | string | empty | OpenCode API key itself, shared by Go and Zen. Empty falls back to the `OPENCODE_API_KEY` env var. Missing keys never fail boot — the TUI still opens and `/model` lists all providers; picking or sending with a Go/Zen model and no key says "OpenCode API key not found" |
| `openrouter_key` | string | empty | OpenRouter API key itself. Empty falls back to the `OPENROUTER_API_KEY` env var. Second provider: `/model` always lists OpenRouter models (`openrouter/<vendor>/<model>`, fallback when offline) served via OpenRouter's OpenAI-compatible chat endpoint. Picking or sending with an `openrouter/…` model and no key says "OpenRouter key not found" |
| `session_id` | string | `ses-<unixnano>` | Stable per conversation; sent as `x-opencode-session` |
| `plan` | bool | `false` | Plan mode: read-only investigation — the plan overlay joins the system prompt, `write`/`edit`/`shell` and every MCP tool refuse with guidance, `task` is explore-only. TUI `/plan` or Tab in the chatbox toggles at runtime; `-plan` sets it headless |
| `max_rounds` | int | `300` | Per-turn round backstop; on exhaustion the turn compacts send-side and gets exactly one more window, then stops ("across 2 compacted windows") |
| `max_stagnant_rounds` | int | `25` | Maximum consecutive identical ordered tool batches allowed; changed tool names/normalized arguments/order reset the count. Not a limit on read-only research; the single-call third-repeat guard may stop earlier |
| `max_context_tokens` | int | `0` (auto) | Fail-fast budget per round (est. runes/4): `0` resolves the model's own input window when known (gemini 1M, claude 500k, gpt 400k, grok/qwen/kimi 256k, glm/minimax 200k, deepseek 128k, else 300k); explicit values override and survive `/model`. Over-budget history aborts the turn with guidance instead of a doomed send |
| `request_timeout_seconds` | int | `300` | Optional deadline for each HTTP attempt, including response-body reads (SSE or unary), not the whole turn; retries each get a fresh deadline |
| `temperature` | float | unset | Pointer semantics: unset omits the field, `0.0` sends zero |
| `max_tokens` | int | `16384` | Explicit output cap on all three transports (`max_tokens` on Chat/Messages, `max_output_tokens` on Responses); positive configured values are honored without a floor |
| `compaction` | table | `auto=true, keep_tokens=15000, buffer=10%, thinking=medium` | Summary-mode, send-side only (prior disk records remain intact): `auto` prunes and summarizes before send and retries once after provider context overflow; `keep_tokens` is a soft whole-turn tail target, always protecting the entire current turn; `buffer` headroom below `max_context_tokens`; `model` summarizer override (empty = session model); `thinking` independent of top-level effort (default `medium` when accepted, cheapest fallback per model) |
| `[tui]` | table | `max_lines=1000`, `sound=true`, `select_scroll_ms=50` | Transcript rendering (TUI only). `max_lines` bounds the rendered committed scrollback so the viewport never re-parses an ever-growing transcript (the slowdown after long sessions). Over budget, older turns collapse their reasoning (user prompts, tool lines and replies are kept whole, never cut), then drop oldest-first behind a `… N earlier turn(s) trimmed` row; disk history stays lossless and ctrl+h/recall views still expand everything. `0` disables trimming. `sound` (default `true`) rings the terminal bell (BEL) when a question appears or a turn finishes (not on cancel); `false` mutes it. `select_scroll_ms` (default `50`) paces the selection drag auto-scroll: one viewport line per interval while dragging past the top/bottom edge |
| `[sandbox]` | table | `allow`, `deny`, `allow_bind`, `ipc_allow` (all unset = built-in defaults) | Shell sandbox policy (see "Shell sandbox"); declared before `[input]` |
| `[input]` | table | `blink=true, height=3` | Chatbox chrome (TUI only): block-cursor blink plus input box height in lines |
| `tools` | string[] | empty | Subset of `shell`, `web_search`, `web_fetch`, `read`, `write`, `edit`, `glob`, `grep`, `AskUserQuestion`, `todowrite`, `todoread`. `AskUserQuestion` plus `todowrite`/`todoread` plus `task` are also registered automatically when absent, so model-driven questions, the session plan, and subagent delegation work in every frontend |
| `mcp` | table[] | empty | Each entry needs `name` + `cmd`; optional `args`, `env` |

The system prompt is built in per model family (opencode-adapted,
valen-tuned: muse→meta, claude→anthropic, old reasoning→beast, gpt-6→astra,
gpt/grok→gpt, codex/copilot/gemini/kimi/trinity→their file, else default)
plus a small `<env>` header (model, workdir, platform, date). Standing
instructions come from `~/.config/valen/AGENTS.md` (global, appended as
"User Instructions") and `<workdir>/AGENTS.md` (project, appended as
"Project Instructions"), each capped at 32768 bytes and applied silently on
new sessions and every refreshed system message.

Validation errors (missing `model`, MCP entry without `cmd`, unreadable file)
exit non-zero before any network call. Missing API keys are not validation
errors: boot always succeeds and the missing key surfaces on model pick
(TUI notice) and on send (friendly transcript error naming
`opencode_key` vs `openrouter_key`).

## Provider contract

Chat requests are `POST {base_url}/chat/completions` with
`Authorization: Bearer $KEY`. OpenCode Go additionally requires, on every
request:

- `x-opencode-session: <stable id>` — routing and prompt-cache affinity
- `User-Agent: valen/<version> (opencode-compatible agent harness)` — an
  honest SDK identity (`version` stamps via `make prod` ldflags, else `dev`).
  `web_search`/`web_fetch` instead send a browser UA, where scraped sites'
  bot defenses expect one.

On chat both are stamped by `headerTransport` in `main.go`, so they survive
retries and apply to every call the OpenAI client makes. The `messages` and
`responses` transports stamp the same two headers per request, plus
`anthropic-version: 2023-06-01` and `x-api-key` on messages.

`GET {base_url}/usage` returns the Go plan quota (`rolling` / `weekly` /
`monthly`, each with `status`, `percent`, `resetsAt`). The TUI polls it at
startup, once a minute, and after each turn to render the footer; non-200s and
non-Go keys are ignored so the footer simply stays hidden; on `zen/…` models (pay-as-you-go, no plan quota) the footer shows just `Zen`. On
`openrouter/…` models the Go quota stays hidden and the footer shows the
OpenRouter balance instead (`GET {openrouter_base}/credits` →
`openrouter $X.XX left`); plain API keys 403 on that endpoint, so only the
locally-measured metadata below shows until a management-key balance lands.
Each `openrouter/…` completion also feeds best-effort footer metadata: the
serving provider (top-level `provider`) and billed cost (`usage.cost`) from the
body or last SSE usage chunk, the catalog's `pricing.prompt`/`completion`
rates, locally-timed throughput (completion tokens over the first-to-last
content delta, no extra request), and the session's cumulative billed cost
(`usage.cost` summed across turns and tool-loop rounds; shown as
`$X.XXXX used`, four decimals below a cent, with the session scope implicit).
The session total is measured
locally, so it renders without a balance and resets on a new/resumed session.

Transport is selected per model by `modelTransport` (live Go table, Oct 2
2026): `grok-4.x`, `gpt-*-luna` and `muse-spark-*-contributor` are
`/responses` (`@ai-sdk/openai`); `minimax-*` and all `qwen*`
(`qwen3.8-max`, `qwen3.8-flash`, `qwen3.7-plus`) are `/messages`
(`@ai-sdk/anthropic`); everything else (`glm-*`, `kimi-*`, `deepseek-*`,
`mimo-*`, `longcat-*`, `hy*`, `space-bunny-free`) is `/chat/completions`.
`zen/…` models ride OpenCode Zen's own gateway
(`https://opencode.ai/zen/v1`, same OpenCode key as Go) with the bare model
ID on the wire; `zenTransport` picks the endpoint per model (Zen docs, Oct
2026): gpt/grok/muse-spark → `/responses`, claude plus the qwen3.5–3.8
plus/flash family (all `qwen*` except `qwen3.8-max`) → `/messages`, and
`qwen3.8-max` plus everything else (deepseek, glm, kimi, minimax, mistral,
big-pickle) → `/chat/completions`. Zen's native-API Gemini models are
filtered out of the catalog as unsupported.
`openrouter/…` models always use OpenRouter's OpenAI-compatible
`POST https://openrouter.ai/api/v1/chat/completions` (bearer key plus
`X-Title: valen`, no Go session header) with the vendor/model ID on the
wire and `reasoning_effort` for thinking; family levels resolve through
the vendor segment (`openrouter/moonshotai/kimi-k3` → `kimi-k3` rules).
OpenRouter returns reasoning as `reasoning` (not `reasoning_content`); it is
captured into `ReasoningContent` and sent back on assistant messages as both
fields, since thinking-mode upstreams 400 when it is not passed back.
DeepSeek models (any provider) also get an explicit `reasoning_content: ""` on
assistant tool-call messages that have none, unless `thinking = "none"`.
History stays OpenAI-shaped in memory; the non-chat transports convert each
round and fold the reply back, so the TUI, session log and tool registry are
transport-agnostic.

- `messages` (`messages.go`): Anthropic wire format. `POST {base}/messages`
  with `model`, required `max_tokens` (`max_tokens` config, default 16384),
  `system` (single cached text block), `messages`, `tools` (`input_schema`),
  `tool_choice: {type: auto}` when tools exist. Assistant `tool_calls` become
  `tool_use` blocks, tool messages become user `tool_result` blocks. Three
  prompt-cache breakpoints ride every request (system block, last tool, last
  message block) for prefix caching. During the active tool loop, native
  content blocks replay in order, including thinking signatures and opaque
  redacted blocks. Display thinking folds back as `<think>`; redacted data
  is not displayed or stored in ordinary history.
- `responses` (`responses.go`): OpenAI Responses, stateless (`store: false`,
  history replayed client-side, no `previous_response_id`). `POST
  {base}/responses` with `model`, `input` array, `instructions` (system),
  `tools` (`type: function` + `parameters`), `tool_choice: auto`.
  Assistant text before a call is tagged `phase: commentary` (Meta requires
  it); active-turn native output preserves provider phases and reasoning
  items, requesting `reasoning.encrypted_content` for stateless replay.
  Reasoning summaries fold back as `<think>` for display.

Native Messages/Responses metadata is retained only inside the active
transport loop, not across later turns, overflow redispatch, or session
resume. Ordinary history/checkpoints preserve text and tool transactions,
not those native blocks/items. Chat's native `ReasoningContent` remains in
history and is checkpointed. Display `<think>` text is not a substitute for
signed or encrypted replay.

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

### OpenRouter routing variants

`routing` selects OpenRouter's provider-sorting variant and is appended to the
wire model as a suffix by `wireModelID`: `floor` → `:floor` (cheapest;
`provider.sort: "price"`, flex-tier eligible), `nitro` → `:nitro` (fastest
throughput; `provider.sort: "throughput"`, priority-tier eligible), `exacto` →
`:exacto` (quality-first provider sorting for tool use), empty/`none` → the
bare slug (OpenRouter's default inverse-square price-weighted load balancing,
unchanged). It applies only to `openrouter/…` models; on Go and Zen the setting
is ignored and ctrl+y is a **silent no-op** (no notice, no state write). Switch
live with `/route` or ctrl+y (cycles none → floor → nitro → exacto, wraps). The
suffix lives on the wire only, so the stored/config model never carries it and
a model switch or `none` cannot leave a stale suffix.

## Agent loop

```
messages = [system?] + [user prompt]
repeat up to max_rounds:
  resp = backend(messages, tools)   # chat | messages | responses per model
  require completed response, visible answer or valid tool_calls
  reject repeated calls/batches before executing them
  messages += resp.message
  if no tool_calls: print content, print token tally, exit 0
  run read-only builtin batches in parallel (max 8), otherwise serially
  append tool results in request order
error: "stopped after N rounds without a final answer"
```
On that stop `runTurn` compacts send-side and grants exactly one more
`max_rounds` window when compaction shrank history (auto-compaction on and a
compactable head exists); a second exhaustion is terminal ("across 2
compacted windows"). A lone oversized turn has no compactable head, so the
backstop stays terminal there.

- `runModelTurn` owns execution policy for all transports; transport closures
  own request conversion, streaming fallback, and active-turn native replay.
  No synthetic batching/act/claim nudges or final no-tools recovery rounds
  are inserted. Legacy notices are stripped when entering the loop.
- Only batches consisting entirely of known read-only builtins (`read`,
  `glob`, `grep`, `web_search`, `web_fetch`) and `task` overlap, with at most
  8 non-task calls and 5 `task` subagents running (the rest queue). `write`
  and `edit` serialize per canonical path, so parallel subagents cannot lose
  each other's updates; `shell` writes are not locked (the prompts warn).
  Any mutating, interactive, stateful, MCP, or unknown tool makes the
  whole batch execute serially in request order. File-access approvals also
  serialize. Parallel `tool_start` events emit in request order;
  `tool_result` events emit as calls complete, matched by call ID. History
  results always append in request order, after the entire batch finishes.
- Fresh registry-capped tool results are sent in full; there is no secondary
  2k live-send truncation. Pruning protects the latest two genuine user turns
  plus a 40k-token shield of older tool outputs. Beyond that shield, reads
  become path/range descriptors and other outputs shorten to 2000 chars;
  user decisions do not prune. The 2000-char summarizer serialization cap is
  separate. `task` keeps research in isolated history and returns only its
  visible final answer, not private thinking or intermediate tool output.

- Assistant text goes to stdout; `[tool name]` traces and the token tally go to
  stderr, so `valen "..." > out.md` stays clean.
- `resp.Choices` empty is an error, not a panic.
- API errors are returned wrapped (`api: ...`) with the provider's message.
  Transient 429/502/503/504 and network errors retry with backoff (3 attempts)
  before surfacing, so overloads back off instead of failing the turn.
  Each HTTP attempt has a `request_timeout_seconds` deadline including body
  reads; connection setup also has 10s dial/TLS and 60s response-header limits.
  Retryable bodies close without a blocking drain. This is not an idle timeout
  or a whole-turn deadline; tools retain their separate limits.
- Ctrl-C cancels the request context and unwinds MCP closers.
- Every round requests `stream: true` SSE. Text/thinking deltas stream to the
  frontend via `onEvent` (`text_delta`, `thinking_delta`) while tool calls
  accumulate per round. A provider answering with a complete unary JSON body
  folds through the same validation with no deltas; a 400 blaming streaming
  retries the round once without `stream: true`.
- SSE requires a transport terminal marker (Chat `[DONE]` or finish reason,
  Messages `message_stop`, Responses `response.completed`). Provider errors,
  failed/incomplete responses, unfinished blocks and invalid tool calls are
  errors, never executable partial tool work. Tool calls need unique IDs,
  names, function type and JSON-object arguments. A thinking-only response
  with no tools is an error, not a successful final answer.
- `AskUserQuestion` is part of the same loop: the model may ask multiple
  questions, the active frontend collects answers (options, type-your-own, and
  confirmation), and the formatted answers return as an ordinary tool result.
  Transports encode that result with their existing converters (`role: tool`,
  `tool_result`, or `function_call_output`).
- Guards run every round: `checkContextBudget` includes history (including
  Chat native reasoning), roughly 40 tokens of envelope per message and the
  full tool-schema block. `doomLoopTool` stops a third identical consecutive
  single call; `max_stagnant_rounds` bounds consecutive identical ordered
  batches, with normalized JSON arguments. Read-only progress is not
  stagnation. Limits return explicit errors and retain completed work.
- Plan mode (`plan`, `/plan`, `-plan`, Tab in the TUI chatbox) adds the
  read-only overlay and gates tools centrally via ctx: `write`/`edit`/`shell`
  and all MCP tools (opaque, so never assumed read-only) refuse with guidance
  and `task` accepts only `explore`; the reply is the plan.
- With auto-compaction enabled, `maybeAutoCompact` prunes before each turn
  and summarizes older whole turns past min(budget minus buffer,
  `keep_tokens` times 3), using the independent compaction effort and no
  tools. The entire current turn and every kept call/result transaction
  remain verbatim; `keep_tokens` is soft, not a hard bound. A lone oversized
  turn has no compactable head and can fail the context-budget guard or
  provider limit. Between tool rounds only local pruning runs, never a
  synchronous summary. Provider overflow permits one compaction/redispatch
  if the head can shrink; a second failure is terminal. `/compact`
  (`/summarize`) uses the same path. Repeated summaries update in place.
- TUI success, failure and cancellation commit tool records captured from
  `Event`s plus a `Turn.History` continuation checkpoint. Disk retains all
  turn records and checkpoints; memory keeps only the latest checkpoint.
  Resume uses that checkpoint (including compaction), then reconstructs
  subsequent legacy turns with their tool calls/results. Prior JSONL records
  are never rewritten by compaction. This is not crash-time checkpointing or
  persistent native Messages/Responses replay.

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
  `...[truncated N bytes]` suffix. Fresh results use this same cap on the wire;
  only older unprotected history and summarizer payloads shorten further.
  Session tool records preserve the registry-capped output independently of
  send-side compaction.

### Builtins

| Tool | Args | Behaviour |
| --- | --- | --- |
| `shell` | `command`, `network` (bool, default false) | `sh -c` in the workdir, 60s timeout, combined stdout+stderr; non-zero exit is returned as content with `[exit N]`, a timeout as partial output with `[timed out after …]`, not as an error |
| `web_search` | `query` | DuckDuckGo HTML endpoint, up to 8 results as `N. title / url / snippet`; unwraps `/l/?uddg=` tracker links; 1 MiB body cap |
| `web_fetch` | `url` | GET with 512 KiB cap, HTML tokenized to text (script/style/noscript dropped), truncated to 8000 chars |
| `read` | `path`, `offset`, `limit` | Line-windowed read (default 2000 lines, max 5000, ~12KB per call with an offset hint to continue); lines prefixed with their number for `edit` context; long lines trimmed at 1000 chars |
| `write` | `path`, `content` | Create/overwrite a file, creating parents; preserves the existing mode |
| `edit` | `path`, `oldString`, `newString`, `replaceAll` | Exact-string replace; refuses ambiguous multi-matches unless `replaceAll` |
| `glob` | `pattern`, `path` | Recursive file list supporting `*`/`?`/`[...]`/`**`; top-level subtrees walk in parallel, results sorted; skips `.git` plus dependency/build dirs (`node_modules`, `target`, `dist`, `build`, `bin`, `vendor`, `.venv`/`venv`, `__pycache__`, `.next`, `out`, `coverage`) |
| `grep` | `pattern`, `path`, `include` | Go regexp content search as `file:line: text`, sorted by file then line; directory walk collects candidates while file reads/matches run on a bounded worker pool (8); skips dependency/build dirs (same list as `glob`)/binaries/>1 MiB files, capped at 100 matches (max 20 per file, so one noisy file can't starve others) |
| `task` | `prompt`, `subagent_type` (`explore`/`general`), `description` | Foreground-only subagent inheriting current turn config and `effectiveSystem` plus kind-specific instructions, with isolated history (explore: read-only subset; general: file+shell subset, no ask/todo/task). At most 25 rounds, depth 1 (subagents cannot delegate); several `task` calls in one round run concurrently, at most 5; only the visible final answer folds into main history. Always registered like `AskUserQuestion` |
| `AskUserQuestion` | `questions[]` | Presents each question with options plus type-your-own, confirms all answers, and returns `User has answered your questions: ...` |
| `todowrite` | `todos[]` (`content`, `status`: `pending`/`in_progress`/`completed`/`cancelled`, `priority`: `high`/`medium`/`low`) | Replaces the in-memory session plan (opencode schema); the call records fold into the session JSONL, `/clear`/`/new` reset it, resume restores the last plan. The model must call it FIRST before any file reads/edits on 3+ step tasks and update it after each step: one `in_progress` at a time, mark `completed` only after verification |
| `todoread` | — | Returns the current session plan as JSON (`[]` when empty) |

File tools and `shell` share one sandbox root: the startup directory,
`-dir <path>`, or a positional directory in TUI mode
(`./valen ~/Code/something`). Paths escaping the root pause for approval
through the active `AskUserQuestion` frontend (one answer covers the whole
folder; allow and deny are both remembered for the session, including by
parallel subagents, which wait on one shared prompt gate and reuse the answer
instead of re-asking; shell `network`/destructive-label approvals behave the
same; see "Access prompts" below). A read approval grants read only; write
and edit ask separately and grant read and write. Sibling git worktrees of the
same repo (`git worktree list`) are in scope and never prompt. Without an
interactive frontend they are denied with a clear error, so the model can
explain and ask.

`shell` runs under an OS sandbox (`sandbox.go`), enforced by the kernel so
`eval`, `$(...)` and scripts cannot escape it:

- **Writes** only in the workdir, sibling git worktrees, folders approved for
  write (this session or in `access.toml`), the private temp dir and tool caches
  (`~/go`, `~/.cache`, `~/.npm`, `~/.cargo`, `~/fvm`, `~/.fvm`, `~/.pub-cache`,
  `~/.rustup`, `~/.gradle`, the user cache dir). Shared `/tmp` and `$HOME` are not
  writable by default: explicit use asks once per folder via `shellgate.go`,
  like `git push` does. `$HOME` itself is never a writable root, however it was
  granted (a folder approval for a file directly in `$HOME`, `[sandbox] allow`,
  or `access.toml write` naming it): the grant is dropped so home confinement
  stays; subdirs of `$HOME` still grant normally.
- **Reads**: `$HOME` is hidden (reads ask once per folder, then the approved
  folder is re-allowed read-only), as are credential files (`~/.ssh`, `~/.aws`,
  `~/.gnupg`, `~/.kube`, `~/.docker`, `~/.azure`, `~/.netrc`, `~/.npmrc`,
  `~/.pypirc`, `~/.config`, session data), other users' home directories
  (`/Users/*`, `/home/*`), and `.env` / `.env.*` secrets anywhere (reads and
  writes denied; templates `.env.example|sample|template` stay readable).
  `read`, `write`, `edit` and `grep` all refuse `.env` secrets. Both platforms scan the workdir at
  command start for `.env` files and any hardlinks to them (Linux masks them,
  Darwin adds literal denies beside the name regex). System reads
  outside `$HOME` (toolchain, `/usr`, libraries) stay allowed so
  `go test -run 'Test' .`, `go vet` and `gofmt` never prompt. `/etc` is denied
  wholesale on Darwin so obfuscated reads fail closed; a few world-readable
  files (`/etc/gitconfig`, `/etc/paths`, `/etc/paths.d`, `/etc/ssl/cert.pem`)
  are re-allowed read-only and never prompt, while `/etc/passwd` still asks.
- **Environment**: secret-looking variables (`*_KEY`, `*_TOKEN`, passwords,
  DSNs, `DATABASE_URL`, ...) are stripped from the `sh -c` child, so
  `printenv`/`env` cannot leak valen's own keys or the user's exported secrets.
- **Network off** (loopback egress and, on macOS, loopback bind work; see
  below) unless the call sets `network: true`;
  the user is asked once per session.
- **Ask once per session** before `git push`, `git reset --hard`, `git clean`,
  `rm -r` and `sudo`, and once per folder before explicit outside-workdir
  paths (`/tmp/...`, `/etc/passwd`, `~/...`) (`shellgate.go`). Text matching
  is best-effort; the sandbox is the boundary.

Built-in writable roots also include the toolchain dirs `~/fvm`, `~/.fvm`,
`~/.pub-cache`, `~/.rustup` and `~/.gradle` (never prompt). Two narrow
capabilities replace the all-or-nothing `network` toggle, both default on:

- **Loopback bind**: with network off, listening on the loopback addresses in
  `[sandbox] allow_bind` (default `localhost`) is allowed so `go test ./...`
  (httptest) needs no egress grant. macOS only (one
  `(allow network-bind (local ip "<addr>:*"))` per address); bubblewrap's
  private netns has `lo` down, so on Linux use `network: true`. Off with
  `[sandbox] allow_bind = []`.
- **IPC sockets**: connecting to the docker unix sockets (`/var/run/docker.sock`,
  `~/.colima/default/docker.sock`, `~/.colima/docker.sock`,
  `~/.docker/run/docker.sock`) is allowed by exact path (testcontainers, sqlc);
  the socket's folder stays hidden. Replace or disable with
  `[sandbox] ipc_allow`.

The optional `[sandbox]` table in `config.toml` (before `[input]`) tunes this:
`allow` (extra rw roots), `deny` (extra hidden paths, win over every allow),
`allow_bind`, `ipc_allow`. Omitted keys keep the defaults; relative paths and
non-loopback bind addresses are rejected at load.

Platform differences (same policy in `sandbox.go`, different enforcement):

- **Loopback bind**: macOS only; ignored on Linux (use `network: true`).
- **Denied or hidden folders**: macOS denies the path, so access fails with
  "Operation not permitted"; Linux masks it (empty tmpfs for a dir, `/dev/null`
  for a file), so it looks empty or missing. The `[sandbox: …]` hint matches
  `Operation not permitted` / `Read-only file system`, so on Linux it fires for
  blocked writes but not for reads of a masked folder.
- **IPC sockets**: re-allowed by exact path after every deny/mask on both
  platforms, so a socket inside a denied folder stays reachable.
- **`.env` files**: Linux masks the ones that exist at command start; macOS
  also matches by name regex.
- **Status**: the Linux path (`sandbox_linux.go`: mask order, socket bind,
  denied-folder hiding) has no coverage; the sandbox-exec and bwrap tests skip
  where nesting is blocked. Parity is by design, not yet verified.

### Access prompts and `access.toml`

Every folder or shell-capability approval is one fixed question (no free-text
row; the folder or command is shown on its own line, bold yellow in the TUI):
the 4-choice `Grant access this session`, `Grant access FOR ALL SESSIONS`,
`Deny access this session`, `Deny access FOR ALL SESSIONS`, or just the two
session choices when there is no rules file (`-config <file>` runs are
session-only). An all-sessions answer is followed by a yes/no confirm (default
yes; an empty Enter accepts it) naming the rule and
`~/.config/valen/access.toml`; `No`, or cancelling the confirm, downgrades to
the session outcome; an unrecognized answer fails closed to deny-this-session.

`access.toml` (seeded empty, hand-editable, written atomically, deduplicated and
sorted):

```toml
read  = []   # read-only forever
write = []   # read + write forever
deny  = []   # blocked forever (read and write)
[shell]
allow = []   # network, git push, rm -r, sudo, ...
deny  = []
```

Folders are absolute (`~/` expands) and prefix-matched. Precedence: secrets
(`.env*`, credential dirs) always blocked; then `deny` (this file, `[sandbox]
deny`, or this session) beats any allow, the workdir included; then
`read`/`write`/session approvals.
Rules apply at once to the file tools, the shell gate and the kernel policy
(a denied folder is hidden from shell). A malformed file grants nothing and is
never overwritten.

macOS uses `sandbox-exec` (Seatbelt); Linux needs `bwrap` (bubblewrap) on PATH,
else `shell` errors with an install hint. Other OSes do not compile
(`sandbox_other.go`): there is no unsandboxed mode. A failed command whose
output shows a write or network denial gets a `[sandbox: …]` hint.

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
- `access_test.go` — 4-choice access prompt + confirm, `access.toml` round-trip, deny-wins, seeded roots, loopback bind / ipc / `[sandbox]` policy
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
  persist round-trip, `thinking` validation in `loadConfig`, plus OpenRouter:
  provider prefix/wire/display/base forms, family levels through the vendor
  segment, provider validation in `loadConfig`, catalog fetch/filter,
  the routing suffix/validation (`floor`/`nitro`/`exacto`, no-op on Go/Zen),
  and the OpenRouter chat wire shape (stripped model, OR key, no session
  header, `reasoning_effort`). Zen: `zenTransport` per model, provider
  prefix/wire/base forms, catalog fetch filtering Gemini, and the shared
  OpenCode key resolving Go and Zen alike. TUI picker merge/fallback across
  all three catalogs rides with the other picker tests.
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
- `tui_test.go`, `tui_render_test.go`, `tui_key_test.go`, `tui_view_test.go`,
  `tui_select_test.go` —
  the Bubble Tea UI offline: model/session lifecycle, slash commands and
  named sessions; committed-turn trim, tool/thinking rendering and streaming
  assembly (`tui_render_test.go`); key/mouse/CSI handling, overlays,
  autoscroll and input-`[tui]` config tables (`tui_key_test.go`); layout,
  status rows, favourite slots and routing footer/session-cost metadata
  (`tui_view_test.go`); drag selection, padding-trimmed copy, highlight and
  ctrl+click links (`tui_select_test.go`).
- `todos_test.go` — `todowrite`/`todoread` round trip (opencode schema),
  validation rejects, empty/reset, restore-from-turns, scrollback checklist
  formatting (glyphs, args shape, cap tail, non-todo fallback);
  `lean_test.go` — per-round prune without network, never summarizes mid-turn,
  auto-off no-op.
- `task_test.go` — `task` arg/kind validation, depth guard, explore read-only
  subset, offline delegation loop, error propagation.
- Output-preservation/estimate/cache tests: `TestFitSendBudget`,
  `TestEstimateCountsEnvelopeAndSchemas`, `TestMessagesCacheBreakpoints`,
  `TestRunToolCallsParallel`, `TestRunToolCallsMCPSerial`,
  `TestReadDefaultsWideWithByteCap`, `TestGrepGlobSkipDependencyDirs`,
  `TestCompactionThinkingExplicitAndFallback`.
- `model_loop_test.go`, `guards_test.go`, `messages_native_test.go`,
  `responses_transport_test.go`, `session_regression_test.go` cover shared
  policy, repeat detection, completion/error validation, active native replay
  and checkpoint/tool-record resume. All use offline fixtures.

## Usage

```sh
export OPENCODE_API_KEY=...
go run .                              # fullscreen TUI (ctrl+h for keys)
go run . ~/Code/something             # TUI rooted at another folder
go run . -dir ~/Code/something        # same, explicit (also works headless)
go run . -p "..."                     # one-shot answer on stdout
go run . -q "..."                     # start the TUI and run the prompt immediately
go run . -config other.toml < prompt.txt   # headless via pipe
go run . -p "..." -plan               # one-shot plan: read-only, reply is the plan
go run . -p "..." -dv 2> trace.log     # timestamped round/tool/size trace
go run . -dvlog trace.log              # same, to a file (use in TUI mode)
```

Debug trace (`-dv` to stderr, `-dvlog <file>` implies `-dv`): one
`[dv HH:MM:SS.mmm +elapsed]` line per event — turn start/end with usage,
per-round history sizes + wire bytes + tail detail, per-round latency with
server usage, per-tool args/output bytes with durations, batch totals,
prune/compact actions, and HTTP attempts (status, retry sleeps). Off by
default; zero hot-loop cost when off.

TUI model controls: `/model [filter]` (ctrl+p) lists the OpenCode Go, OpenCode
Zen and OpenRouter catalogs and
switches; `/effort [level]` (`/thinking` alias) sets the thinking level and
`/route [none|floor|nitro|exacto]` (ctrl+y) the OpenRouter routing variant for
the current model (routing is a no-op elsewhere). Both persist to `state.json` in the config dir, so the last selection
sticks across runs and overrides `config.toml`'s `model`/`thinking`/`routing`; headless
mode reads the same overlay (not with `-config <file>`). `/plan` toggles plan
mode at runtime (read-only investigation, reply is the plan; header shows
`· plan`); it does not persist. The TUI footer also
shows Go plan usage (`5h` / `7d` / `30d`), yellow past 80% and pink at the cap —
or the OpenRouter balance (`openrouter $X.XX left`) on `openrouter/…` models, followed
by best-effort serving-provider, throughput and per-million in/out rate
metadata when OpenRouter reports it (throughput is measured locally from the
stream — no extra request); it stays
hidden on `zen/…` models (pay-as-you-go).

## Extension points

- New builtin: add a `func() Tool` to the `builtins` map in `LoadBuiltins`.
- New transport: add a stream round in the style of `chatRoundStream` /
  `messagesRoundStream` / `responsesRoundStream` (SSE deltas via `onEvent`,
  unary-JSON body accepted, `isStreamRejected` fallback) and a
  `modelTransport` entry; `runTurn` dispatches on it.
