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

- Streaming deltas (the JSON client is non-streaming today; hook points are
  reserved, see "Streaming" below).
- Server/client process split, multi-agent orchestration.
- Full editing of history (recall is read-only; `/clear` resets).
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
 ✳ valen  ·  glm-5.3-flash  ·  ses-…  ·  3 tools

  You  15:04
  ┌ summarise the latest go release notes

  ✦ thinking
  │ checking release notes…

  ⏺ web_search  go release notes 2026
    → 8 results…

  ✳ glm-5.3-flash
   Go 1.27 shipped with … (rendered markdown)

   ↑ 412 · ↓ 89

 ─────────────────────────────────────
 ● ready  1 turn · ↑412 ↓89
 ╭───────────────────────────────────╮
 │ Type a message…                   │
 ╰───────────────────────────────────╯
 /help for keys and commands                          5h 90% ↻38m · 7d 36%
```

- Transcript is padded conversation blocks separated by blank lines; no role
  prefixes. `You` (orange) and `✳ <model>` (green) head each block.
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
- Tool calls render one compact cyan line plus a dim first-line preview;
  full args/output live in the Tools view.
- Replies render as markdown via glamour (Monokai style, wrapped to viewport).
- Errors render as a pink `✗ <friendly>` block; details in Errors view.
- Tokens per turn render dim (`↑ N · ↓ M`); session totals in the status bar.
- Go plan usage (`5h` rolling / `7d` / `30d` percent) sits dim at the right of
  the footer hint line, refreshed at startup, every minute, and after each
  turn; yellow past 80%, pink at 100% or rate-limited, with a `↻` reset hint
  on hot windows. Hidden when `{base_url}/usage` is unavailable (non-Go keys).
- Autoscroll follows new turns; PgUp/PgDn scrolls the viewport.

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
  "Working… press esc to cancel."
- `max_rounds` applies per user turn, same as `SPEC.md`.
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
  (~60ms, tail-capped) with the viewport pinned to the bottom; the committed
  turn replaces the live block with the markdown reply. Tool start/result
  events share the same throttle (a parallel batch emits 2N events at once);
  the committed `turnDoneMsg` always renders final state. Rendered committed
  turns are cached append-only (keyed on turn count, width, thinking display,
  model), so per-delta cost is the live pending block, not history depth.
- Go plan quota refreshes share one in-flight fetch: the minute tick and the
  post-turn refresh never overlap on slow networks.

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
JSONL, swaps in the grown history, and goes idle. `AskUserQuestion` blocks the
background turn on `askPrompter`; the UI thread serves `questionMsg` in the
question overlay and replies with confirmed answers, leaving history and the
transport converters unchanged. Slash commands
(`/clear` `/think` `/save` `/help` `/history` `/sessions` `/tools` `/errors` `/model`
`/effort` `/quit`) resolve inside `submit`/`handleCommand`; `/model` fetches
the catalog asynchronously via `modelsLoadedMsg`. The footer quota refreshes
via `usageMsg` from `Init`, a one-minute `usageTickMsg`, and each `turnDoneMsg`.

Entry point: `main()` keeps flag parsing; if `stdin`/`stdout` are TTYs and
`-p` was not given, it builds config/registry/MCP once and runs the Bubble
Tea program with alt screen; otherwise it falls back to one-shot `chat()`.

## History & recall

- Same `Turn` model + JSONL log at
  `$XDG_DATA_HOME/valen/sessions/<session_id>.jsonl`.
- History view: one row per turn (time, prompt snippet, tokens) plus older
  saved session files read-only. Sessions view (Ctrl+L): every saved session
  with its title (first prompt, 60 runes), model/effort, turn count — Enter
  resumes it (transcript + history rebuilt, same file continues). Titles and
  the model/effort in use live in `<id>.jsonl.meta.json` next to the
  append-only JSONL plus per-turn `model`/`effort`. Tools view (Ctrl+U):
  every tool record with status.
  Errors view: turns with `Error`. Enter opens full detail; Esc returns.
- `/clear` resets in-memory history and turns; the disk log is never deleted.
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

- `/model` (ctrl+p) fetches the live OpenCode Go catalog (`GET {base_url}/models`)
  and opens a picker: ↑↓ move, Enter switches, Esc backs out. Transports
  (`/chat`, `/responses`, `/messages`) are marked; all three converse.
- `/model <filter>` switches directly on an exact or unique-substring match,
  otherwise opens the filtered picker. `/model <known-id>` works offline.
- Switching keeps the conversation history; an incompatible thinking level
  resets to the provider default with a note.
- Both model and thinking persist to `config.toml` (comments and other keys
  untouched), so the last selection sticks across runs. Without a config path
  (tests) the switch is session-only.

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
ready, input focused. `/clear` resets intentionally.

## Keybindings

| Key | Chat | Overlays (history/sessions/tools/errors/help/detail) |
| --- | --- | --- |
| Enter | send turn | open selected row / resume session |
| Esc | cancel run / clear input / back | back to chat |
| Ctrl+C / Ctrl+D | cancel (busy) / quit (idle) | quit |
| Ctrl+J / Alt+Enter / Shift+Enter | newline in input (shift+enter where the terminal supports it) | — |
| PgUp/PgDn / Ctrl+D | scroll transcript | scroll view |
| Ctrl+G | help | help |
| Ctrl+R / Ctrl+L / Ctrl+U / Ctrl+X | history / sessions / tools / errors | same |
| Ctrl+Y | toggle thinking display | toggle thinking display |
| Ctrl+N | new blank session (fresh id + file; blocked while busy) | same |
| Ctrl+T | thinking level cycle low→high, wraps to low | same |
| Ctrl+O | toggle status details | toggle status details |
| Ctrl+P | model picker | model picker |
| Ctrl+S | save transcript | save transcript |
| Ctrl+Q | quit | quit |
| ↑↓ / j/k | textarea cursor (PgUp scrolls) | move selection |
| Space / Enter / Esc | — | question overlay: toggle / select+confirm / dismiss |

Function keys are gone; the footer only points at `/help`, which lists every
shortcut (and the shortcut line above).

Slash commands: `/help` `/history` `/sessions` (`/resume`, `/continue` — resume most recent) `/tools` `/errors` `/think` `/save`
`/clear` `/compact` (`/summarize` alias) `/model [filter]` `/effort [level]` (`/thinking` alias) `/quit`.

## Streaming (live)

Every transport round requests SSE (`stream: true`). The background turn
forwards `text_delta` / `thinking_delta` via `prog.Send(toolEventMsg)` plus
`tool_start` (with args) / `tool_result` (with output); `Update` accumulates a
`pendingTools` timeline plus `pendingText` / `pendingThinking` and re-renders
the pending block throttled with the viewport pinned to the bottom. Streamed
text renders plain with each `text_delta` on its own line until commit, when
the full reply renders as markdown.
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
  severity thresholds, footer text/reset hints, refresh command wiring.
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
