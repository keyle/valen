# SPEC-tui — fullscreen loop UI (bubbletea)

Fullscreen terminal UI that keeps the conversation alive: the model answers,
the UI returns to an input prompt, and the user keeps chatting in the same
process, reusing the same message history. No server/client split — one
in-process Bubble Tea program drives the existing `runTurn` loop directly.

Depends on: `SPEC.md` (provider contract, tool contract, MCP lifecycle) stays
in force. Everything below is additive to it.

## Goals

- Personal-opencode feel: a padded terminal conversation, not a Borland
  desktop. No `user:` / `assistant:` prefixes.
- Continuous conversation loop: prompt → assistant → prompt, in one session.
- User message renders instantly on submit, before the response arrives.
- Thinking blocks shown by default (toggleable), tools inline, markdown
  replies rendered.
- Clear run state at all times: ready, working + active tool, error.
- In-flight requests cancelable without quitting the app.
- Recall older turns and tool-call results, across the session and runs.
- Friendly error presentation: one-line friendly message plus details view.
- Monokai-dark theme across chrome and markdown.
- Headless one-shot mode preserved for scripts (`-p` / non-TTY stdin).

## Non-goals

- Server/client process split, multi-agent orchestration.
- Full editing of history (recall is read-only; `/clear` resets). The one exception: ↑ on an empty chatbox reloads the last prompt, and sending it replaces that turn.
- Changes to the provider/tool/MCP contracts in `SPEC.md`.

## Library choice

`github.com/charmbracelet/bubbletea` + `bubbles` + `lipgloss` + `glamour`:

- `bubbletea` — Elm-style event loop, single in-process model.
- `bubbles/viewport` — scrollable transcript + recall views.
- `bubbles/textarea` — multiline prompt input.
- `bubbles/spinner` — working indicator while the turn runs.
- `lipgloss` — Monokai-dark chrome (borders, padding, status).
- `glamour` — markdown replies with a Monokai JSON style + word wrap.

Rationale: the old tview Borland UI felt dated and its role-prefix transcript
(`user:` / `assistant:`) read like a log, not a conversation. Bubble Tea maps
better onto an opencode-style chat: padded blocks, instant user echo, inline
tool activity, overlay recall views — all in one `tuiModel` with async
`turnDoneMsg` / `toolEventMsg` messages.

## UX

```
 ✦ valen  ·  glm-5.3-flash  ·  <session title>  ·  alt+m select

  You  15:04
  ┌ summarise the latest go release notes

  ✦ thinking
  │ checking release notes…

  ⏺ web_search  go release notes 2026
    → 8 results…

   ✦ glm-5.3-flash
   Go 1.27 shipped with … (rendered markdown)

 ─────────────────────────────────────
 ● ready · 8s  1 turn · win 2k/200k · ↑412 ↓89
 ╭───────────────────────────────────╮
 │ Type a message…                   │
 ╰───────────────────────────────────╯
 /help for keys and commands                          5h 90% ↻38m · 7d 36%
```

- Header: ` ✦ valen  ·  <model>  ·  <session title>  ·  alt+m select` — the
  session title is the same English name shown in the Sessions view (first
  prompt, 60 runes), set the moment that prompt is submitted so the header
  names the session while the first turn is still running. Only an untouched
  session (no prompt yet) omits the title segment entirely rather than falling
  back to the `ses-…` id, so the header reads
  ` ✦ valen  ·  <model>  ·  alt+m select`. The
  tools count is not shown, and `alt+m select` names the plain-drag selection
  toggle.
- Transcript is padded conversation blocks separated by blank lines; no role
  prefixes. `You` (orange) and ` ✦ <model>` (green, leading space so it lines
  up with ` ✦ valen`) head each block.
  A blank session shows only a `No turns yet.` label; the AGENTS.md-first
  behaviour lives in the default system prompt (`config.toml`), not as
  intro text in the transcript.
- Pending user message + working status render immediately on Enter, before
  `runTurn` completes. Spinner lives in the status bar so the transcript does
  not re-render every tick.
- While a turn runs, the pending block grows live: thinking so far, each tool
  call in order (running, then its first-line result), then the assistant
  reply as plain text (markdown applies on commit). The viewport stays pinned
  to the bottom so searches and thinking visibly arrive sequentially.
- Open tool threads show the animated spinner frame plus `· running` in the
  scrollback, so parallel batches read as concurrent work; finished threads
  keep the usual line plus preview. The status line counts them
  (`⏺ fetch… · 2 running`, `✓ search · 1 still running`). The scrollback
  repaints at the ~10fps spinner tick only while threads are open; committed
  turns stay cached.
- Thinking renders dim/italic with a left rule, shown by default; ctrl+y or
  `/think` collapses to `✦ thinking (N chars)`.
- Live busy state renders once, in the spinner row above the chatbox, with
  `esc esc to stop` pinned to its right edge; the pending scrollback block
  carries no duplicate tool/status line.
- Tool calls render one compact cyan line plus a dim first-line preview;
  full args/output live in the Tools view. Headers use plain English, not
  raw JSON (`⏺ read foo.go`, `⏺ grep "pat" in src`, `⏺ edit foo.go`).
  `edit` rows render the change as a `-`/`+` diff (old/new strings, capped
  at 16 lines — 8 del + 8 add — with a `+N more` tail), live from the call args while running
  and committed on the stored args; `write` rows preview added `+` lines
  the same way. `todowrite`/`todoread` rows render
  the session plan as a `TODO` header, a blank line, then one `[x]` completed /
  `[.]` in progress / `[ ]` pending / `[-]` cancelled line per item instead of
  raw JSON (every item is listed, no cap), live from the call args while running and from the stored result on
  commit — so todo progress reads in the scrollback next to thinking.
- Replies render as markdown via glamour (Monokai style, wrapped to viewport).
- The committed scrollback is bounded by `[tui] max_lines` (default `1000`;
  `0` disables). Past the budget, the newest turn stays full while it fits;
  older turns collapse their reasoning to a `N chars trimmed` header and
  their reply to its first 8 lines, then the oldest turns drop behind a
  `… N earlier turn(s) trimmed — ctrl+h for history` row. User prompts and
  tool rows are never trimmed, so the cap only removes in-between reasoning
  — the viewport stays cheap without losing what was asked or run. History
  on disk is untouched; ctrl+h and the recall/detail views expand everything.
- Errors render as a pink `✗ <friendly>` block; details in Errors view.
- Session totals render once, in the status row above the chatbox, in
  thousands (`N turns · win W/B · ↑P ↓C`, e.g. `3 turns · win 45k/300k ·
  ↑276k ↓3k`); per-turn scrollback tallies are gone. While busy the spinner
  row also carries loop progress (`· rd 12/300`).
- Go plan usage (`5h` rolling / `7d` / `30d` percent) sits dim at the right of
  the footer hint line, refreshed at startup, every minute, and after each
  turn; yellow past 80%, pink at 100% or rate-limited, with a `↻` reset hint
  on hot windows. Hidden when `{base_url}/usage` is unavailable (non-Go keys).
  On `openrouter/…` models the Go quota stays hidden and the footer shows the
  OpenRouter balance (`OR $X.XX left`) once the credits fetch lands; plain API
  keys 403 on `/credits`, so the footer shows only the hint until then.
- Autoscroll follows new turns while pinned to the bottom; PgUp/PgDn or the
  mouse wheel scrolls the transcript, which pauses autoscroll until scrolled
  back to the bottom (a new submit or committed turn re-pins). Mouse reporting
  is on by default so the wheel scrolls; the terminal bypass modifier
  (shift in most terminals, opt in iTerm2/Terminal.app) drag-selects +
  copies without toggling. alt+m toggles plain-drag selection: reporting
  off means a plain drag selects + copies natively (wheel paused) until
  alt+m re-enables it. Split mouse
  reports misparsed as key runes (rapid wheeling across TTY reads) are
  held for reassembly and never typed: a completed `[<…M` report drops,
  anything else releases as typing; genuine wheel input still arrives as
  mouse events. Only pasted or typed chars may reach the input box.
- Chatbox chrome is configurable via `[input]` in `config.toml`: `blink`
  (default `true`; `false` parks the block cursor steady with no blink
  timer) and `height` (default `3` box lines; the transcript viewport
  shrinks to fit).

## State machine

```
                send (user echo instantly)
   idle ─────────────────▶ working ── resp ──▶ tools? ──yes──▶ running_tools ──┐
     ▲                      │                        │no                         │
     │                      │ err                    ▼                           │
     │                      └─────────────┐   finish (append reply, tallies)    │
     │                                    ▼                 │                   │
     └────── continue chat ◀────────── failed (append [error], stay alive)     │
                          ▲                                                     │
                          └────────────────── tool result ◀─────────────────────┘
```

Rules:

- At most one in-flight run; Enter while working shows
  "Working… press esc twice to cancel."
- `max_rounds` and consecutive-identical-batch guards apply per turn through
  the shared `runModelTurn`, same as `SPEC.md`; no synthetic recovery rounds.
- Token totals accumulate per session.
- Esc / Ctrl+C during working cancels the run's context → the turn ends with
  a friendly "Cancelled" error; the app does not quit.
- `AskUserQuestion` opens a question overlay while the turn stays busy:
  ↑↓/j/k moves, Enter selects, Space toggles multi-select, type-your-own
  accepts free text, and a final confirmation submits or cancels.
  Esc / Ctrl+C / Ctrl+D first dismisses the pending answers and returns them
  to the model as a tool error; it does not cancel the run.
- Ctrl+C / Ctrl+D at idle quits. Ctrl+Q quits. MCP closers run on exit.
- Text/thinking deltas render into the pending transcript throttled
  (~60ms, rolling 32768-byte UTF-8-safe text/thinking tails) with the viewport
  pinned to the bottom; the committed turn replaces the live block with the
  markdown reply. Tool start/result events share the same throttle.
  Read-only builtin result events arrive in
  completion order and match rows by call ID; history remains request-ordered.
  The committed `turnDoneMsg` always renders final state. Rendered committed
  turns are cached append-only per turn (keyed on turn count, width, thinking
  display, model), each keeping a full and a lazily-built compact form; the
  `[tui] max_lines` budget picks between them and bounds the assembly, so
  per-delta cost is the bounded scrollback plus the live pending block, not
  history depth or total history bytes.
- Go plan quota refreshes share one in-flight fetch: the minute tick and the
  post-turn refresh never overlap on slow networks. The refresh dispatches by
  provider (`fetchQuotaCmd`): the OpenRouter balance on `openrouter/…` models,
  the Go quota otherwise — the hidden provider never refreshes.

## Architecture

```go
type tuiModel struct {
    viewport viewport.Model
    input    textarea.Model
    spinner  spinner.Model

    cfg      Config
    session  *Session
    history  []openai.ChatCompletionMessage
    busy     bool
    pendingPrompt string
    toolStatus    string
    mode     viewMode // chat, history, tools, errors, help, detail
    ...
}

type toolEventMsg struct{ kind, text string } // "tool_start" | "tool_result"
type turnDoneMsg struct{ prompt, thinking, answer string; tools []ToolRecord; usage openai.Usage; err error; history []openai.ChatCompletionMessage }
```

`submit` snapshots history (copy + user message), flips to busy, renders the
pending block, and returns a `tea.Cmd` that runs the existing `runTurn`
off the UI thread. `onEvent` forwards progress via `prog.Send(toolEventMsg)`.
Completion returns `turnDoneMsg`; `Update` commits the `Turn` to the session
JSONL with a continuation checkpoint on success, failure or cancel, swaps in
the current history, and goes idle. Tool records are captured directly from
Events, so compaction cannot erase them. `AskUserQuestion` blocks the
background turn on `askPrompter`; the UI thread serves `questionMsg` in the
question overlay and replies with confirmed answers, leaving history and the
transport converters unchanged. Slash commands
(`/clear` `/think` `/save` `/help` `/history` `/sessions` `/tools` `/errors` `/model`
`/effort` `/quit`) resolve inside `submit`/`handleCommand`; `/model` fetches
the catalog asynchronously via `modelsLoadedMsg`. The footer quota refreshes
via `orCreditsMsg`/`usageMsg` from `fetchQuotaCmd` at `Init`, on each
one-minute `usageTickMsg`, after each `turnDoneMsg`, and on every model
switch.

Entry point: `main()` keeps flag parsing; if `stdin`/`stdout` are TTYs and
`-p` was not given, it builds config/registry/MCP once and runs the Bubble
Tea program with alt screen; otherwise it falls back to one-shot `chat()`.

## History & recall

- Same `Turn` model + JSONL log at
  `$XDG_DATA_HOME/valen/sessions/<session_id>.jsonl`.
- History view: one row per turn (time, prompt snippet, tokens) plus older
  saved session files read-only. Sessions view (Ctrl+L): every saved session
  with its title (first prompt, 60 runes), model/effort, turn count — Enter
  resumes it (latest `Turn.History` checkpoint plus later legacy turns, same
  file continues). Esc backs to the current session without arming the cancel
  confirm, so browsing sessions never counts toward stopping a running turn.
  Legacy records reconstruct tool calls and results as well
  as prompt/reply text. Titles and the model/effort in use live in
  `<id>.jsonl.meta.json` next to the
  append-only JSONL plus per-turn `model`/`effort`. Tools view (Ctrl+U):
  every tool record with status.
  Errors view: turns with `Error`. Enter opens full detail; Esc returns.
- Disk retains every turn/checkpoint; memory keeps only the latest checkpoint.
  Compaction changes continuation history, not earlier JSONL records or full
  registry-capped tool records. Native Chat reasoning is checkpointed;
  Messages signatures and Responses encrypted/native items remain scoped to
  the active transport loop, not later turns or resume. End-of-turn saving
  does not guarantee crash-time recovery; save failures remain visible.
- Historical headers use the originating turn's model/effort, not the current
  picker selection. A switch during a request applies to future requests;
  the in-flight turn keeps its original attribution.
- `/clear` resets in-memory history and turns; the disk log is never deleted.
  `/clear` and `/new` also reset the session todo list; resuming a session
  restores its last `todowrite` plan.
  `/new` (ctrl+n) mints a fresh `ses-<unixnano>` session with its own JSONL:
  history reseeds, pending/live state and render cache drop, `x-opencode-session`
  clients rebuild around the new id, and the old file stays resumable via
  `/sessions`. Blocked while a turn runs (esc first).

## Thinking / reasoning display

- `<think>...</think>` parsed into `Turn.Thinking` via `splitThinking`.
- Shown by default, dim + indented. Ctrl+Y / `/think` toggles the display.
- `/effort [level]` (`/thinking` alias) sets the reasoning level sent to the
  provider for the current model — not the display. Levels come from
  `AvailableThinkingLevels` (fixed well-known table per models.dev, plus a
  disk cache at `$XDG_DATA_HOME/valen/thinking_levels.json`): muse-spark
  `minimal`/`low`/`medium`/`high`/`xhigh`, gpt/grok `low`…`max`, glm/deepseek
  `low`/`high`/`max` or `high`/`max`, kimi-k3 `max` only, toggle models
  `none`/`high`, fixed models none. Empty clears back to the provider
  default. Ctrl+t cycles low→high and wraps to low (ctrl+[ is esc in the
  terminal, so up/down chords cannot work). Unknown models probe with a bogus
  effort on switch and learn from any 400 listing valid levels.
- Tools always visible inline (compact); full output via Tools view.
- Transcript, thinking, question and detail content wraps to the viewport
  width (long single tokens are force-broken) so nothing runs off-screen.

## Model switching

- `/model` (ctrl+p, or ctrl+m where the terminal sends disambiguated ctrl+m)
  fetches the live catalogs — OpenCode Go (`GET {base_url}/models`) plus
  OpenRouter (`GET https://openrouter.ai/api/v1/models`) — and opens
  a picker: ↑↓ move, Enter switches, Space stars/unstars, Esc backs out. The
  picker search box is
  pinned to the full content width (like the chatbox) so typing never resizes
  it. Both providers list even
  without keys (curated fallbacks when offline/unkeyed); picking or sending
  with a model whose key is missing reports "OpenCode Go API key not found"
  or "OpenRouter key not found" naming the env var, instead of failing boot.
  Every row carries its
  provider prefix (`go/…`, `openrouter/…`); Go transports (`/chat`,
  `/responses`, `/messages`) are marked, OpenRouter rows always converse
  over its OpenAI-compatible chat endpoint.
- Favourites are a plain YAML list (`- glm-5.3`, OpenRouter IDs keep their
  `openrouter/…` prefix) at
  `~/.config/valen/favourites.yaml` (`$XDG_CONFIG_HOME/valen/`). Starred
  models sort to the top of the picker with a ★, alphabetically within each
  group, and the cursor stays on the toggled row. Ctrl+F writes the file;
  without a state path (tests, `-config <file>`) favourites are session-only.
- `/model <filter>` switches directly on an exact or unique-substring match,
  otherwise opens the filtered picker. `/model <known-id>` works offline.
- Switching keeps the conversation history; an incompatible thinking level
  resets to the provider default with a note.
- Both model and thinking persist to `~/.config/valen/state.json`
  (`$XDG_CONFIG_HOME/valen/`), never to `config.toml`, and override the
  config's `model`/`thinking` on the next boot (an invalid saved pair is
  ignored). Without a state path (tests, `-config <file>`) the switch is
  session-only.

## Friendly errors

Same mapping table as before (`errors.go`): 401/402/404/429/5xx/timeout/
cancelled/empty-choices/max-rounds/MCP. Each is one `✗` block in the
conversation plus a details record; Errors view shows the raw cause.
The last prompt is retained (pending echo / committed turn) so cancel and
rate limits are retryable.

## Continuing the conversation

The `messages` slice lives in the model and grows across turns:

```
user → assistant(+tool rounds) → assistant  = turn 1, kept
user → …                                    = turn 2, appended to the same history
```

End of response: reply appended to `history`, tokens shown, state back to
ready (the status row keeps the turn's working time: `● ready · 1m3s`), input focused. `/clear` resets intentionally. A bell rings when a turn finishes (not on cancel) or a question appears (`[tui] sound`).

With an empty chatbox, ↑ reloads the previous prompt for editing; sending it
rewinds that turn (history and scrollback) and continues from before it. The
disk log stays append-only; the retry's checkpoint supersedes the dropped
turn on resume. Emptying the box or a `/command` cancels the retry.

The question overlay hides the chatbox and footer (like the pickers) so it gets
the full height, starts at its heading and scrolls to follow the selected option.

## Keybindings

| Key | Chat | Overlays (history/sessions/tools/errors/help/detail) |
| --- | --- | --- |
| Enter | send turn | open selected row / resume session |
| Esc | cancel run / clear input / back | back to chat |
| Ctrl+C / Ctrl+D | cancel (busy) / quit (idle) | quit |
| Ctrl+J / Alt+Enter / Shift+Enter | newline in input (shift+enter where the terminal supports it) | — |
| PgUp/PgDn / wheel | scroll transcript (pauses autoscroll until back at bottom) | scroll view |
| Shift+drag (Opt+drag in iTerm/Terminal) | native drag-select + copy (wheel keeps scrolling) | same |
| Alt+M | toggle plain-drag selection (mouse reporting off: drag selects, wheel paused) | — |
| Ctrl+G | help | help |
| Ctrl+R / Ctrl+L / Ctrl+U / Ctrl+X | history / sessions / tools / errors | same |
| Ctrl+Y | toggle thinking display | toggle thinking display |
| Ctrl+N | new blank session (fresh id + file; blocked while busy) | same |
| Ctrl+T | thinking level cycle low→high, wraps to low | same |
| Ctrl+O | toggle status details | toggle status details |
| Ctrl+P / Ctrl+M | model picker | model picker |
| Ctrl+S | save transcript | save transcript |
| Ctrl+Q | quit | quit |
| ↑↓ / j/k | textarea cursor (PgUp scrolls); ↑ on an empty box recalls the last prompt | move selection (view scrolls to follow) |
| Space / Enter / Esc | — | question overlay: toggle / select+confirm / dismiss |

Function keys are gone; the footer only points at `/help`, which lists every
shortcut (and the shortcut line above).

Slash commands: `/help` `/history` `/sessions` (`/resume`, `/continue` — resume most recent) `/tools` `/errors` `/think` `/save`
`/clear` `/compact` (`/summarize` alias) `/model [filter]` `/effort [level]` (`/thinking` alias) `/quit` (`:q` alias).

## Streaming (live)

Every transport round requests SSE (`stream: true`). The background turn
forwards `text_delta` / `thinking_delta` via `prog.Send(toolEventMsg)` plus
`tool_start` (with args) / `tool_result` (with output); `Update` accumulates a
`pendingTools` timeline plus `pendingText` / `pendingThinking` and re-renders
the pending block throttled with the viewport pinned to the bottom. Text
fragments concatenate exactly, without inserted newlines; the first delta of
a new reasoning part starts on its own line. Rolling
32768-byte buffers keep the newest UTF-8-safe tail instead of freezing at the
cap; only a short tail is displayed live. On commit the full reply renders as
markdown. Display buffers are not provider-native reasoning replay state.
Providers answering unary JSON fold with no deltas; a 400 blaming streaming
retries the round unary.

## Testing

- Pure model tests (no TTY): instant user echo before `turnDoneMsg`, reply +
  headers + usage on success, friendly 402, history/tools/model/effort views +
  detail, `/clear` `/think` `/model` `/effort` unknown-command paths, no
  `user:`/`assistant:` prefixes, thinking visible by default.
- Question overlay tests: multi-select, type-your-own, confirmation submit,
  and dismiss paths.
- `models_test.go`: transport map, per-model thinking levels, request wiring
  (`reasoning_effort` vs `enable_thinking`, messages thinking, responses
  reasoning), config persist round-trip,
  `thinking` validation in `loadConfig`.
- `usage_test.go`: `/usage` parsing + identity headers, 404 hides the footer,
  severity thresholds, footer text/reset hints, refresh command wiring;
  `/credits` parsing, 403 hides the footer, balance flooring + footer text,
  OpenRouter footer hiding Go usage, per-provider refresh dispatch.
- Fake completions server drives the real `runTurn` through `submit`.
- `stream_test.go`: per-transport SSE rounds, stream-rejection fallback, and
  the pending tool/text timeline accumulation.
- Session JSONL round-trip, ctrl+x details, `/save` export.
- Keep CLI-path tests (`main_test.go` et al.); `go vet`, `gofmt`,
  `go test -race ./...` green.

## Migration plan

1. Refactor `chat()` into `runTurn(..., onEvent)`; keep headless working.
2. Replace `tui.go` (tview) with the bubbletea `tuiModel` above; drop
   `tview`/`tcell` deps, add `bubbletea`/`bubbles`/`lipgloss`/`glamour`.
3. Rewrite `tui_test.go` against the model (instant echo, no prefixes,
   thinking shown, recall views).
4. Wire `main()`: TUI when TTY && !`-p`, else headless; shared setup.
5. JSONL session log + recall views + thinking toggle + friendly errors.
6. Update `SPEC.md` layout/usage and this spec for both modes.
