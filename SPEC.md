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
| `session.go` | Turn model + JSONL session log at `$XDG_DATA_HOME/valen/sessions/<workdir>-<hash>/` (one folder per working directory); both TUI turns and headless `-p` runs append, so every call is resumable from `/sessions` or headless `-r` |
| `compaction.go` | Summary-mode compaction (prune + cheap-model summary, send-side only) |
| `usage.go` | Quota for the TUI footer: Go plan quota (`GET /usage`) and OpenRouter balance (`GET /credits`) |
| `errors.go` | Friendly one-line errors mapped from status codes/causes (ctrl+x has details) |
| `privacy.go` | OpenRouter `zdr`/`data_collection` filters, data-policy refusal detection, and the session-only relaxation prompt |
| `think.go` | Splits `<think>` blocks out of replies so the UI can collapse them |
| `tools.go` | Tool contract, ordered registry, builtin loader, output limits, `shell` |
| `todos.go` | Session plan (`todowrite`/`todoread`, opencode schema, in-memory + session fold) |
| `task.go` | Foreground-only `task` subagent (explore/general, isolated history, depth 1) |
| `sandbox*.go`, `shellgate.go` | Shell sandbox: policy in `sandbox.go`, `sandbox-exec` (macOS), `bwrap` (Linux), other OSes fail to compile; once-per-session prompts for network and destructive commands |
| `files.go` | File builtins (`read`, `write`, `edit`, `glob`, `grep`): tool implementations, glob matching, background grep |
| `fileaccess.go` | File-access gate: workdir/worktree roots, outside-access approval, secret/data guards |
| `attach.go` | Attachments: `-f`/`@`/pasted-path resolution, opencode-style text and folder inlining, image sniffing, resizing and caps, and the wire-only image parts per transport (chat `image_url`, messages `image`, responses `input_image`) |
| `clipboard.go` | Screenshot paste: `pbpaste`/`wl-paste`/`xclip` probe with timeout and output cap, content-addressed cache under `$XDG_CACHE_HOME/valen/attachments`, 7-day / 200-entry prune |
| `ask.go` | `AskUserQuestion` contract, headless prompting/confirmation, answer formatting |
| `search.go` | `web_search` (DuckDuckGo HTML) and `web_fetch` (HTML → text) |
| `mcp.go` | stdio MCP client lifecycle, tool registration, result flattening |
| `config.toml` | Default configuration, embedded in the binary and seeded as `valen.toml` to `~/.config/valen/` on first run |
| `userconfig.go` | Config dir (`$XDG_CONFIG_HOME/valen`, else `~/.config/valen`): first-run seeding, `valen.toml`, legacy `config.toml` fallback, `config-ori.toml`, `state.json` (last model/thinking/routing) |

## Config location

Config lives in `~/.config/valen/` (`$XDG_CONFIG_HOME/valen/`), created on
first run: `valen.toml` (yours; never overwritten), `config-ori.toml` (pristine
copy of the shipped default, refreshed on upgrade so you can diff), and
`state.json` (ephemeral TUI state written by valen: last model/thinking/affinity;
`-p` ignores it and follows `valen.toml`). A
pre-rename `config.toml` is read as-is when `valen.toml` is absent; it is never
moved or rewritten. Sessions
and caches stay under `$XDG_DATA_HOME/valen` (`~/.local/share/valen`).
`-config <file>` uses that file only: no seeding and no state overlay.

## Config schema (`valen.toml`)

| Key | Type | Default | Notes |
| --- | --- | --- | --- |
| `model` | string | — | **required.** Model ID, e.g. `glm-5.3-flash`, `kimi-k3`, `zen/claude-haiku-5-5`, `openrouter/moonshotai/kimi-k3`, or `openai/gpt-6`. The `zen/` prefix selects OpenCode Zen (`https://opencode.ai/zen/v1`, same OpenCode key as Go; transport chosen per model: gpt/grok/muse → responses; claude and qwen3.5–3.8 plus/flash (all qwen except qwen3.8-max) → messages; qwen3.8-max and the rest → chat; Gemini models are not supported); the `openrouter/` prefix selects the OpenRouter provider (needs its key, below); the `openai/` prefix selects the optional OpenAI API (`https://api.openai.com/v1`, `OPENAI_API_KEY`, always the Responses transport); bare IDs ride OpenCode Go. Switch live with `/models` (ctrl+p); the TUI persists the choice to `state.json`, while headless `-p` always follows this file |
| `thinking` | string | empty (provider default) | Reasoning level, per-model (see `/effort` picker): muse-spark `minimal`/`low`/`medium`/`high`/`xhigh`, gpt/grok `low`…`max` (+`none` on gpt), glm/deepseek `low`/`high`/`max` or `high`/`max`, kimi-k3 `max` only, toggle models `none`/`high`, fixed no-control models omit. Switch live with `/effort` (`/thinking` alias) or ctrl+t (cycles low→high, wraps); the TUI persists the choice to `state.json`. Unknown models fall back to the full vocabulary until a 400 teaches the valid list, cached at `$XDG_DATA_HOME/valen/thinking_levels.json` |
| `affinity` | string | empty (`none`) | OpenRouter provider affinity/routing variant, sent as a model-slug suffix: `floor` (cheapest; `provider.sort: "price"`), `nitro` (fastest throughput; `provider.sort: "throughput"`), `exacto` (quality-first tool-use sorting), or empty/`none` (OpenRouter's default load balancing). `routing` is an accepted alias; set only one (conflicting values are a load error). Switch live with `/route [none\|floor\|nitro\|exacto]` or ctrl+y (cycles none → floor → nitro → exacto, wraps); a **silent no-op on Go/Zen/OpenAI**. Cleared at load and on model switches for non-OpenRouter models. The TUI persists the choice to `state.json` alongside `thinking`; also stored in favourite slots. Ignored on non-OpenRouter models |
| `provider_ignore` | string[] | `["baseten"]` | OpenRouter `provider.ignore`: provider slugs skipped per request (`openrouter/…` models only; a silent no-op on Go/Zen). Unset keeps the default `["baseten"]`, an explicit `[]` disables the blacklist. Mutually exclusive with `provider_only` (which wins when both are set). The `provider` object merges with the `affinity` suffix, which sets `provider.sort` |
| `provider_only` | string[] | unset | OpenRouter `provider.only`: an allowlist instead of the blacklist. Sent as the request `provider` object alongside `affinity`. A 404 naming "No allowed providers" means the list excludes every provider serving the model; the error names the serving providers, the allowlist, and this setting |
| `zdr` | bool | `true` | OpenRouter `provider.zdr`: route only to zero-data-retention endpoints (`openrouter/…` models only; a silent no-op elsewhere). Every request sends it explicitly |
| `data_collection` | bool | `false` | OpenRouter `provider.data_collection`: `false` sends `"deny"` (no provider storage/training), `true` sends `"allow"`. Same scope as `zdr` |
| `opencode_key` | string | empty | OpenCode API key itself, shared by Go and Zen. Empty falls back to the `OPENCODE_API_KEY` env var. Missing keys never fail boot — the TUI still opens and `/models` lists all providers; picking or sending with a Go/Zen model and no key says "OpenCode API key not found" |
| `openrouter_key` | string | empty | OpenRouter API key itself. Empty falls back to the `OPENROUTER_API_KEY` env var. Second provider: `/models` always lists OpenRouter models (`openrouter/<vendor>/<model>`, fallback when offline) served via OpenRouter's OpenAI-compatible chat endpoint. Picking or sending with an `openrouter/…` model and no key says "OpenRouter key not found" |
| `openai_key` | string | empty | OpenAI API key itself. Empty falls back to the `OPENAI_API_KEY` env var. Optional third provider: when a key is configured, `/models` lists `openai/<model>` from the live `/v1/models` catalog (curated fallback offline), served over the OpenAI Responses API. Without a key the provider stays hidden; picking or sending an `openai/…` model anyway says "OpenAI key not found" |
| `session_id` | string | `ses-<unixnano>` | Stable per conversation; sent as `x-opencode-session` |
| `saved_sessions` | int | `50` | Saved session logs kept per project folder (`$XDG_DATA_HOME/valen/sessions/<workdir>-<hash>/`). The oldest JSONL plus its `.meta.json` are pruned when a new session starts; `0` or less uses the default. TUI `/sessions` (ctrl+l) lists them and `x` deletes the selected one |
| `plan` | bool | `false` | Plan mode: read-only investigation — the plan overlay joins the system prompt, `write`/`edit`/`shell` and every MCP tool refuse with guidance, `task` is explore-only. TUI `/plan` or Tab in the chatbox toggles at runtime; `-plan` sets it headless |
| `max_rounds` | int | `300` | Per-turn round backstop; on exhaustion the turn compacts send-side and gets exactly one more window, then stops ("across 2 compacted windows") |
| `max_stagnant_rounds` | int | `25` | Maximum consecutive identical ordered tool batches allowed; changed tool names/normalized arguments/order reset the count. Not a limit on read-only research; the single-call third-repeat guard may stop earlier |
| `max_context_tokens` | int | `0` (auto) | Fail-fast budget per round (est. runes/4): `0` resolves the model's own input window when known (gemini 1M, claude 500k, gpt 400k, grok/qwen/kimi 256k, glm/minimax 200k, deepseek 128k, else 300k); explicit values override and survive `/models`. Over-budget history aborts the turn with guidance instead of a doomed send |
| `request_timeout_seconds` | int | `300` | Optional deadline for each HTTP attempt, including response-body reads (SSE or unary), not the whole turn; retries each get a fresh deadline |
| `temperature` | float | unset | Pointer semantics: unset omits the field, `0.0` sends zero |
| `max_tokens` | int | `65536` | Explicit output cap on all three transports (`max_tokens` on Chat/Messages, `max_output_tokens` on Responses); positive configured values are honored without a floor |
| `[extra_body]` | table | unset | Keys merged into each chat-transport request body (e.g. `repetition_penalty = 1.1`). Chat transport only: a non-chat model with it set is a load error. `model`, `messages`, `stream`, `stream_options`, `tools`, `tool_choice` and `provider` are rejected because valen sets them |
| `compaction` | table | `auto=true, keep_tokens=15000, buffer=10%, thinking=medium` | Summary-mode, send-side only (prior disk records remain intact): `auto` prunes and summarizes before send and retries once after provider context overflow; `keep_tokens` is a soft whole-turn tail target, always protecting the entire current turn; `buffer` headroom below `max_context_tokens`; `model` summarizer override (empty = session model); `thinking` independent of top-level effort (default `medium` when accepted, cheapest fallback per model); `routing` (alias `affinity`) is the summarizer's OpenRouter provider-sorting variant, independent of the top-level affinity and dropped for non-`openrouter/…` summarizer models |
| `[tui]` | table | `max_lines=1000`, `sound=true`, `bell_limit_seconds=600`, `bell_spacing_seconds=20`, `select_scroll_ms=50` | Transcript rendering (TUI only). `max_lines` bounds the rendered committed scrollback so the viewport never re-parses an ever-growing transcript (the slowdown after long sessions). Over budget, older turns collapse their reasoning (user prompts, tool lines and replies are kept whole, never cut), then drop oldest-first behind a `… N earlier turn(s) trimmed` row; disk history stays lossless and ctrl+h/recall views still expand everything. `0` disables trimming. `sound` (default `true`) rings the terminal bell (BEL) when a question appears or a turn finishes (not on cancel); it repeats every `bell_spacing_seconds` for up to `bell_limit_seconds`, stopping on any key, click or refocused terminal, and `false` mutes it. `select_scroll_ms` (default `50`) paces the selection drag auto-scroll: one viewport line per interval while dragging past the top/bottom edge |
| `[sandbox]` | table | `allow`, `deny`, `allow_bind`, `ipc_allow` (all unset = built-in defaults) | Shell sandbox policy (see "Shell sandbox"); declared before `[input]` |
| `[input]` | table | `blink=true, height=3` | Chatbox chrome (TUI only): block-cursor blink plus input box height in lines |
| `[attachments]` | table | `max_bytes=20971520, max_base64_bytes=5242880, max_text_bytes=65536, max_width=2000, max_height=2000, auto_resize=true` | Attach caps (see "Attachments"), shared by `-f`/`--file`, headless `@path` and TUI paste: `max_bytes` per item before base64, `max_base64_bytes` encoded image payload ceiling, `max_width`/`max_height` pixel ceilings, `auto_resize` shrinks an over-cap image to fit (false rejects it), `max_text_bytes` inlined text/folder body per item |
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
`opencode_key`, `openrouter_key` or `openai_key`).

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
`O.R. $X.XX`); plain API keys 403 on that endpoint, so only the
locally-measured metadata below shows until a management-key balance lands.
Each `openrouter/…` completion also feeds best-effort footer metadata: the
serving provider (top-level `provider`) and billed cost (`usage.cost`) from the
body or last SSE usage chunk, the catalog's `pricing.prompt`/`completion`
rates (rendered in the footer as `$ 0.10/0.40`, in/out and per-million implicit), locally-timed throughput (completion tokens over the first-to-last
content delta, no extra request), and the session's cumulative billed cost
(`usage.cost` summed across turns and tool-loop rounds; shown as
`$X.XXXX used`, four decimals below a cent, with the session scope implicit).
The session total is measured
locally, so it renders without a balance. It resets on a new session and is
restored on resume: each turn's serving metadata, billed cost and the model's
catalog rates are written to the JSONL, so reloading a session repopulates the
footer as if the last turn had just finished.

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
`openai/…` models always use OpenAI's Responses API
(`POST https://api.openai.com/v1/responses`, bearer `OPENAI_API_KEY`, no Go
session header) with the bare model ID on the wire and `reasoning.effort` for
thinking; the catalog comes from `GET https://api.openai.com/v1/models`,
filtered to text chat families (gpt/o-series/chatgpt).
OpenRouter returns reasoning as `reasoning` (not `reasoning_content`); it is
captured into `ReasoningContent` and sent back on assistant messages as both
fields, since thinking-mode upstreams 400 when it is not passed back.
DeepSeek models (any provider) also get an explicit `reasoning_content: ""` on
assistant tool-call messages that have none, unless `thinking = "none"`.
During a tool loop, OpenRouter's provider-native state also replays on the next
request: the assistant's `reasoning_details` array verbatim (signed and
encrypted entries included) and each tool call's
`extra_content.google.thought_signature`, matched by tool-call ID. Only the
snake_case `extra_content` form is sent back. This state is bare wire data:
it is not stored in history and never crosses into a fresh turn.
History stays OpenAI-shaped in memory; the non-chat transports convert each
round and fold the reply back, so the TUI, session log and tool registry are
transport-agnostic.

- `messages` (`messages.go`): Anthropic wire format. `POST {base}/messages`
  with `model`, required `max_tokens` (`max_tokens` config, default 65536),
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
signed or encrypted replay; Chat's `reasoning_details` and per-call
`extra_content` are active-loop-only like the other transports' native state.

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

`affinity` (alias `routing`) selects OpenRouter's provider-sorting variant and
is appended to the wire model as a suffix by `wireModelID`: `floor` → `:floor` (cheapest;
`provider.sort: "price"`, flex-tier eligible), `nitro` → `:nitro` (fastest
throughput; `provider.sort: "throughput"`, priority-tier eligible), `exacto` →
`:exacto` (quality-first provider sorting for tool use), empty/`none` → the
bare slug (OpenRouter's default inverse-square price-weighted load balancing,
unchanged). It applies only to `openrouter/…` models; on Go, Zen and OpenAI
the setting is ignored and cleared at load and on model switches, and ctrl+y is
a **silent no-op** (no notice, no state write). Switch
live with `/route` or ctrl+y (cycles none → floor → nitro → exacto, wraps). The
suffix lives on the wire only, so the stored/config model never carries it and
a model switch or `none` cannot leave a stale suffix.

### OpenRouter privacy filters

Every `openrouter/…` request sends `provider.zdr` and
`provider.data_collection` from the `zdr` (default `true`) and
`data_collection` (default `false` → `"deny"`) settings. The filters narrow
OpenRouter's eligible provider pool, so a model that normally works may have
no endpoint left. When OpenRouter refuses with a 404/403 whose message (or
routing metadata) says no endpoint matches the data policy, valen asks once
whether to relax both filters for the rest of the session. Relaxing widens the
provider pool but may let a provider retain the prompts/replies and train on
them; the choice is session-only and never written to config. Keeping the
filters (or having no interactive prompter, e.g. `--no-question`) returns the
original refusal, whose title names the filters instead of blaming the plan.
A second refusal after relaxing is terminal.

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

- The final answer goes to stdout; `[tool] <summary>` lines, the live answer
  and the token tally go to stderr, so `valen "..." > out.md` stays clean.
- `resp.Choices` empty is an error, not a panic.
- API errors are returned wrapped (`api: ...`) with the provider's message.
  Transient 429/502/503/504 and network errors retry with backoff (up to 5
  attempts) before surfacing, so overloads back off instead of failing the
  turn. A provider stop keeps its native reason (OpenRouter's
  `native_finish_reason`, e.g. `MALFORMED_FUNCTION_CALL`) and any provider
  error body; the transcript names that cause instead of a generic failure.
  Deterministic `MALFORMED_*` stops are not retried; a bare `error` stop
  without a native reason gets one retry. A round stopped at the output cap
  (`length`, `max_tokens`, `max_output_tokens`) is not retried: it reports the
  friendly "ran out of output tokens" error, and its usage still counts.
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
  errors, never executable partial tool work. A bare `error` finish reason is a
  stop, not a completion: the scan keeps reading for the provider's later
  top-level error chunk, which names the cause. Tool calls need unique IDs,
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
| `shell` | `command`, `network` (bool, default false) | `<shell> -c` in the workdir (`sh` unless `-shell <path>` is given), 60s timeout, combined stdout+stderr; non-zero exit is returned as content with `[exit N]`, a timeout as partial output with `[timed out after …]`, not as an error |
| `web_search` | `query` | DuckDuckGo HTML endpoint, up to 8 results as `N. title / url / snippet`; unwraps `/l/?uddg=` tracker links; 1 MiB body cap |
| `web_fetch` | `url` | GET with 512 KiB cap, HTML tokenized to text (script/style/noscript dropped), truncated to 8000 chars |
| `read` | `path`, `offset`, `limit` | Line-windowed read (default 2000 lines, max 5000, ~12KB per call with an offset hint to continue); lines prefixed with their number for `edit` context; long lines trimmed at 1000 chars. An image returns a text descriptor instead, since tool results carry no bytes: when the image rides this turn it says so (`[image shot.png 1280x720, 84 KiB] attached to this message — inspect the image part, no need to read the file`), otherwise it names the way to attach it (`… ask the user to attach it with @shot.png` inside the workdir, `… ask the user to paste the image or its path` outside it) |
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
(`./valen ~/Code/something`). `-dir` is repeatable: the first path is the
workdir, the rest are extra read+write roots for the run (like a temporary
`[sandbox] allow`); they apply to file tools, the shell gate and the kernel
policy, and are listed in the system prompt. Paths escaping every root pause
for approval through the active `AskUserQuestion` frontend (one answer covers
the whole folder; allow and deny are both remembered for the session, including
by parallel subagents, which wait on one shared prompt gate and reuse the
answer instead of re-asking; shell `network`/destructive-label approvals behave
the same; see "Access prompts" below). Paths that do not exist are refused
without a prompt (a write may create the file, but its parent folder must
already exist). A read approval grants read only; write
and edit ask separately and grant read and write. Sibling git worktrees of the
same repo (`git worktree list`) are in scope and never prompt. Without an
interactive frontend they are denied with one clear
`[sandbox: read|write "<path>" — reason. Fix: …]` line naming `-dir`, and the
`tool_result` event carries the same blocker as `denied: {capability, subject}`,
so the model can explain and the caller can retry with the access granted.

`shell` runs under an OS sandbox (`sandbox.go`), enforced by the kernel so
`eval`, `$(...)` and scripts cannot escape it. Commands run as
`<shell> -c <command>` where `<shell>` is `sh` by default; `-shell <path>`
selects another interpreter (e.g. `-shell /bin/bash`):

- **Writes** only in the workdir, extra `-dir` roots, sibling git worktrees,
  folders approved for write (this session or in `access.toml`), the private
  temp dir and tool caches
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
  `read`, `write`, `edit` and `grep` all refuse `.env` secrets. Both platforms scan the workdir and extra `-dir` roots at
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
  below) unless the call sets `network: true` or the run passes `--network`,
  which pre-approves network for shell commands without a prompt; otherwise
  the user is asked once per session.
- **Ask once per session** before `git push`, `git reset --hard`, `git clean`,
  `rm -r` and `sudo`, and once per folder before explicit outside-workdir
  paths (`/tmp/...`, `/etc/passwd`, `~/...`) (`shellgate.go`). `--exec`
  pre-approves those destructive commands for the run without a prompt. Both
  run flags are session-only, apply to subagents too, and still lose to a
  persistent `[shell] deny` / `deny` rule. Text matching is best-effort; the
  sandbox is the boundary.

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

The optional `[sandbox]` table in `valen.toml` (before `[input]`) tunes this:
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

`/allow-all` (TUI) skips every access prompt for the rest of the session:
folder approvals are booked like session grants (so the shell sandbox inherits
them), and shell-capability and destructive-command prompts are skipped. It is
the bulk-work escape hatch (installing or terraforming a machine). Hard rails
stay: persistent `deny` rules, `.env`/credential hiding, the OS sandbox and
plan mode still apply. `/allow-all off` restores prompts.

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

## Attachments

Files, folders and screenshots can ride one turn beside the prompt (TUI: `@`
picker, drag-and-drop paste, ctrl+v; headless: repeatable `-f`/`--file` plus
`@path` mentions in the prompt). Resolution happens once at submit, in
`attach.go`, after the usual per-folder access check.

- Text files and folders are inlined into the user message in opencode's
  shape: `Attached file: <name>` (a folder is `Attached directory: <path>`), a
  blank line, then the body — so every transport sends identical bytes
  (stable cache prefix). Caps: `[attachments] max_text_bytes` and the first
  2000 lines; a folder is a non-recursive listing capped at 100 entries. A cut
  body is labelled `(truncated)`. Inlined text persists in history like any
  prompt text.
- PNG/JPEG/GIF/WebP images are sniffed by magic bytes and ride the wire copy
  of the last user message — `image_url` parts on chat, `{type: "image",
  source: {type: "base64"}}` on messages, `input_image` on responses — each
  preceded by an `Attached file: <path>` text part, and are re-sent on every
  round of the turn. Image limits match opencode's `media.image`:
  `max_width`/`max_height` (2000 px) and `max_base64_bytes` (5 MiB). With
  `auto_resize = true` (the default) an over-limit image is scaled down with
  its aspect ratio preserved, then encoded as PNG and JPEG at progressively
  smaller scales until it fits, so the wire MIME can change; with
  `auto_resize = false` it is rejected instead. A source over 64 MP is
  rejected before decoding to bound memory.
- PDF, audio, video and other binaries are rejected with a user-facing notice;
  the turn still sends. Rejections never reach the model.
- Stored history stays string-content only. The typed prompt (plus inlined
  text) persists in the session JSONL; image bytes never do, so a resumed
  session does not show the model the screenshots again.
- `-f`/`--file` is headless-only: in TUI mode (interactive or `-q`) it is
  rejected with a hint to use `@` or paste. Clipboard screenshots need
  `pbpaste` (macOS), `wl-paste` (Wayland) or `xclip` (X11); the cached file
  lives under `$XDG_CACHE_HOME/valen/attachments` and bypasses the
  folder-access prompt.

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
- `attach_test.go` — attachment resolution and caps, opencode-style
  text/folder inlining, image resizing (dimensions, base64 ceiling, disabled
  auto-resize), the WebP decoder, `@`/pasted-path parsing, per-transport image
  wire shapes, `read` on an image, TUI chips and reconcile, the clipboard probe
  (stubbed `execCommand`), cache prune, headless `-f`, and the `[attachments]`
  config table.
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
  the routing suffix/validation (`floor`/`nitro`/`exacto`, no-op and cleared
  on Go/Zen/OpenAI),
  and the OpenRouter chat wire shape (stripped model, OR key, no session
  header, `reasoning_effort`). Zen: `zenTransport` per model, provider
  prefix/wire/base forms, catalog fetch filtering Gemini, and the shared
  OpenCode key resolving Go and Zen alike. OpenAI: `openai/` prefix/wire/base
  forms, Responses transport, catalog fetch filtering non-chat models, the
  Responses endpoint + bare model + OpenAI key with no Go session header, and
  the optional key gating the picker. TUI picker merge/fallback across the
  catalogs rides with the other picker tests.
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
go run . -dir ~/Code/a -dir ~/Code/b  # workdir + extra read+write roots (repeatable)
go run . -p "..." --network           # pre-approve network for shell commands this run
go run . -p "..." --exec              # pre-approve destructive shell commands this run
go run . -p "..."                     # one-shot: final answer on stdout, progress on stderr
go run . -q "..."                     # start the TUI and run the prompt immediately
go run . -config other.toml < prompt.txt   # headless via pipe
go run . -p "..." -plan               # one-shot plan: read-only, reply is the plan
go run . -p "..." -f shot.png         # attach an image (repeat -f/--file)
go run . -p "review @main.go"         # @path mentions attach too (headless)
go run . -p "..." -dv 2> trace.log     # timestamped round/tool/size trace
go run . -dvlog trace.log              # same, to a file (use in TUI mode)
go run . -p "..." -v                   # add round markers + thinking summaries to stderr
go run . -p "..." -vv                  # also raw analysis, full args and tool results
go run . -p "..." --stream             # key events as NDJSON on stdout, a done line last
go run . -p "..." --history h.json     # continue from prior user/assistant messages (JSON array)
go run . -p "..." -r <session-id|path> # continue a saved session in place (headless)
go run . -p "..." --no-question --no-task --no-todo   # autonomous: continue unless totally blocked
go run . -p "..." --one-shot-task      # background job bundle: -p plus the three --no-* switches
```

`--stream` writes each turn event as one JSON object per line (`kind`, plus the
event's own fields); the last line is `{"kind":"done","usage":...}`, with an
`error` object when the turn failed and a `session` path when the turn was
saved. Live answer text is not in the stream: it goes to stderr and
`done.reply` carries the final answer. A `tool_result` whose output embedded
a `[sandbox: …]` line also carries
`denied: {"capability":"read|write|network|exec","subject":"…"}` so a driver
can surface the blocker without parsing text. On OpenRouter, `or_meta` carries the generation `id`
for `/generation` lookups (billing and cancellation checks). `--history` reads only `user` and `assistant`
messages (`role`, `content`), before the new prompt; other roles are rejected.
Plain `-p` prints `[session <abs path>]` on stderr after the turn is saved, so
an external driver can resume without guessing the project session folder; a
save failure warns instead and omits the path. `-r`/`--resume` takes that path
or a bare session ID and appends the new turn to the same JSONL (title, turn
count and `x-opencode-session` stay), using the current config's model and
thinking; it is headless only and cannot be combined with `--history`.
`--no-question` also appends the "continue unless totally blocked" policy to
the system prompt (nobody can ask or answer questions, so the model assumes,
continues, and reports any hard blocker in plain English) and denies
out-of-scope file and shell approvals, since nobody can answer them. A blocked
turn returns the `[sandbox: …]` line naming the access needed; the caller can
re-run with `-dir <folder>` (repeatable), `--network` or `--exec` to grant it.
Tool names from MCP servers are `<server>__<tool>`.

Headless `-p` streams tool calls (one-line summaries) and the live answer to
stderr; the final answer stays on stdout (and in `done.reply` with `--stream`).
`-p` takes exactly one quoted argument; unquoted words are rejected rather
than silently joined (`valen -p review main.go` is an error, not a prompt).
`-v` adds round markers and thinking summaries, hiding raw chain-of-thought
when the provider sends both (`reasoning_text` vs `reasoning_summary_text` on
`/responses`). `-vv` adds raw analysis, full tool arguments and tool results.
Both are one-shot only; the TUI already renders these live.

Debug trace (`-dv` to stderr, `-dvlog <file>` implies `-dv`): one
`[dv HH:MM:SS.mmm +elapsed]` line per event — turn start/end with usage,
per-round history sizes + wire bytes + tail detail, per-round latency with
server usage, per-tool args/output bytes with durations, batch totals,
prune/compact actions, and HTTP attempts (status, retry sleeps). Off by
default; zero hot-loop cost when off.

TUI model controls: `/models [filter]` (ctrl+p) lists the OpenCode Go, OpenCode
Zen and OpenRouter catalogs and
switches; `/effort [level]` (`/thinking` alias) sets the thinking level and
`/route [none|floor|nitro|exacto]` (ctrl+y) the OpenRouter routing variant for
the current model (routing is a no-op elsewhere). Both persist to `state.json` in the config dir, so in the TUI the last selection
sticks across runs and overrides `valen.toml`'s `model`/`thinking`/`affinity`.
Headless `-p` ignores that overlay and follows `valen.toml`; `-config <file>`
never overlays. `/plan` toggles plan
mode at runtime (read-only investigation, reply is the plan; header shows
`· plan`); it does not persist. The TUI footer also
shows Go plan usage (`5h` / `7d` / `30d`), yellow past 80% and pink at the cap —
or the OpenRouter balance (`O.R. $X.XX`) on `openrouter/…` models, followed
by best-effort serving-provider and throughput metadata when OpenRouter reports
it (throughput is measured locally from the stream — no extra request), and
the per-million in/out rates (`$ 0.10/0.40`) when the catalog has them. It stays
hidden on `zen/…` models (pay-as-you-go).

## Extension points

- New builtin: add a `func() Tool` to the `builtins` map in `LoadBuiltins`.
- New transport: add a stream round in the style of `chatRoundStream` /
  `messagesRoundStream` / `responsesRoundStream` (SSE deltas via `onEvent`,
  unary-JSON body accepted, `isStreamRejected` fallback) and a
  `modelTransport` entry; `runTurn` dispatches on it.
