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

## Code layout

`package main` throughout (plus `internal/prompts`, `internal/guard`,
`internal/dvlog`); the UI is split by concern so no single file dominates:

- `tui.go` — `tuiModel` state and message types, construction, view-mode, and
  the `runTUI` entry.
- `tui_update.go` — `Update` dispatch, `submit`, slash commands, plan-mode,
  open-selected, and stream/pending-tool mutation.
- `tui_key.go` — key/mouse/CSI parsing and handling.
- `tui_view.go` — chrome (header/status/footer/input) and `View`.
- `tui_render_turn.go` — committed-turn assembly/trim and turn rendering.
- `tui_render_tool.go` — tool/tool-pending rendering, summaries, edit diffs.
- `tui_stream.go` — streaming text assembly helpers.
- `tui_lists.go` — recall/picker/help/session list rendering and item builders.
- `tui_ask.go` — ask-overlay state and handling.
- `tui_layout.go` — width/height math, input wrapping, markdown, truncation.
- `tui_picker.go` — model/effort pickers, favourite slots, thinking/routing.
- `tui_files.go` — `@` file picker (fuzzy filter, preview, accept).
- `tui_attach.go` — attachment chips, `@`/pasted-path/file-picker tokens,
  ctrl+v clipboard paste, and submit-time reconcile.
- `tui_usage.go`, `tui_session.go` — footer quota polling; new/resume/compact/export.
- Shared, UI-agnostic: `runTurn` orchestration in `turn.go`, loop internals in
  `agent.go`, config in `config.go`, the Chat transport in `chat.go`.

## UX

```
 ✦ valen  ·  <session title>                          glm-5.3-flash · high

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
 ~/Code/valen                                  Go · 5h 90% ↻38m · 7d 36%
```

- Header: the ` ✦ valen` logo sits on the left with the version beside it
  (` ✦ valen 1.0.2`; `dev` unstamped) while the session is untitled. Once a
  title is shown it replaces the version: ` ✦ valen  ·  <session title>`. The
  session title is the same English name shown in the Sessions view (first
  prompt, 60 runes), set the moment that prompt is submitted so the header
  names the session while the first turn is still running. Only an untouched
  session (no prompt yet) keeps the version and omits the title segment
  entirely rather than falling back to the `ses-…` id. The bare model
  name and thinking level (`glm-5.3-flash · high`; no `go/`, `zen/` or
  `openrouter/vendor/` prefix) sit flush at the far right, aligned with the
  footer's right text. The OpenRouter routing affinity (`none` omitted;
  otherwise `floor`/`nitro`/`exacto`) trails the thinking level
  (`kimi-k3 · high · nitro`) and is shown only on OpenRouter models — Go and
  Zen ignore routing. The tools count is not shown, and the header carries no
  key hints (shortcuts are listed in `/help`).
- Transcript is padded conversation blocks separated by blank lines; no role
  prefixes. `You` (orange; `You (PLAN)` in cyan with a cyan left bar when sent in plan mode) and ` ✦ <model>` (green, leading space so it lines
  up with ` ✦ valen`) head each block.
  A blank session's transcript is empty; its `valen <version>` label lives in
  the header (above). The AGENTS.md-first behaviour lives in the default system
  prompt (`valen.toml`), not as intro text in the transcript.
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
- Thinking renders dim/italic with a left rule, shown by default;
  ctrl+o or `/think` collapses to `✦ thinking (N chars)`.
- Live busy state renders once, in the spinner row above the chatbox, with
  `esc to stop` pinned to its right edge; the pending scrollback block
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
  older turns collapse their reasoning to a `N chars trimmed` header, then
  the oldest turns drop behind a `… N earlier turn(s) trimmed — ctrl+r for
  history` row. User prompts, tool rows and replies are never cut, so the
  cap only removes in-between reasoning (and, past that, whole old turns) —
  the viewport stays cheap without truncating what was asked, run, or said.
  History on disk is untouched; ctrl+r and the recall/detail views expand
  everything.
- Errors render as a pink `✗ <friendly>` block; details in Errors view.
- Session totals render once, in the status row above the chatbox, in
  thousands (`N turn(s) · win W/B · ↑P ↓C`, e.g. `3 turns · win 45k/300k ·
  ↑276k ↓3k`); per-turn scrollback tallies are gone. While busy the spinner
  row also carries loop progress (`· rd 12/300`). Informational notices
  (e.g. the mouse toggle) are transient: the next chatbox edit clears them
  and restores the totals row; errors stay until resolved. A notice replaces
  the totals, then they return when it clears.
- Go plan usage (`Go · 5h` rolling / `7d` / `30d` percent) sits dim at the right
  of the footer hint line, refreshed at startup, every minute, and after each
  turn; yellow past 80%, pink at 100% or rate-limited, with a `↻` reset hint
  on hot windows. The `Go` label names the plan; narrow terminals keep
  `Go · 5h N%`. Hidden when `{base_url}/usage` is unavailable (non-Go keys).
  On `zen/…` models the right of the footer shows just `Zen` (pay-as-you-go,
  no plan quota, nothing fetched).
  On `openrouter/…` models the Go quota stays hidden and the footer shows the
  OpenRouter balance (`O.R. $X.XX`) once the credits fetch lands; plain API
  keys 403 on `/credits`, so only the locally-measured metadata below shows
  until then.
  When OpenRouter reports it, the balance is followed by best-effort turn
  metadata: the serving provider, throughput, the catalog's per-million
  input/output rates and the session's cumulative billed cost
  (`O.R. $X.XX · Infermatic · 42 TPS · $ 0.10/0.40 · $0.0034 used`);
  in/out and the per-million basis are implicit in the rates, and the cost
  label is just "$X used", the session scope being implicit.
  The provider name and billed cost come from the response (top-level
  `provider`, `usage.cost`); rates come from the model catalog's
  `pricing.prompt`/`pricing.completion`; throughput is measured locally from the
  stream (completion tokens over the first-to-last content delta), so it needs
  no extra request. The session cost sums each turn's billed cost across every
  tool-loop round, keeps four decimals below a cent, resets on a new session,
  and — being local — shows even without a balance. Resuming a session restores
  the last turn's provider/throughput, the cumulative spend and the recorded
  rates from the JSONL, so the footer looks as it did when the turn finished.
  Each footer segment is
  independently optional and the whole suffix yields to the width rule, so a
  narrow terminal or a missing field just drops that part.
- Autoscroll follows new turns while pinned to the bottom; PgUp/PgDn or the
  mouse wheel scrolls the transcript, which pauses autoscroll until scrolled
  back to the bottom (a new submit or committed turn re-pins). A plain
  left-drag over the transcript selects text in-app: motion extends the
  selection (reverse-video highlighted, re-asserted after each syntax/markdown
  colour reset so the whole span stays lit, not just the first word), and
  release copies it to the
  clipboard via OSC 52 and reports `Copied N bytes to clipboard.` in the status
  bar. Dragging to the top or bottom row auto-scrolls the transcript one line
  per `[tui] select_scroll_ms` (default 50ms; extending the selection to the
  revealed rows) until the drag
  leaves the edge or the transcript ends. Selection endpoints are absolute
  transcript lines, not window rows, so lines that auto-scroll out of view stay
  part of the copy — releasing after a long scroll yields the whole span, top
  to bottom. Copy is computed from the ANSI-stripped, display-width-aware line
  content with leading border/padding and trailing width padding trimmed, so a
  drag on a wide terminal no longer copies columns of spaces. Leading chrome is
  removed as a *common* prefix across the selected lines, so a code sample keeps
  its relative indentation instead of collapsing to each line's first glyph,
  while a bordered row still drops its border. This is the
  *raw rendered* text exactly as displayed (no markdown syntax, code fences or
  headings markers); it complements `/copy`, which yields the reply's markdown
  source — use drag for what you see, `/copy` for what is re-usable as markdown.
  Shift+drag is
  ignored (left to the terminal's native bypass modifier), and scrolling clears
  any active selection. Mouse reporting
  is on by default so the wheel scrolls. Split mouse
  reports misparsed as key runes (rapid wheeling across TTY reads) are
  held for reassembly and never typed: a completed `[<…M` report drops,
  anything else releases as typing; genuine wheel input still arrives as
  mouse events. Only pasted or typed chars may reach the input box.
- Link text in the transcript (glamour renders a markdown link as its text plus
  the resolved URL) opens on ctrl+left-click via the platform opener (`open` on
  macOS, `xdg-open` elsewhere); only http(s) URLs open, and a ctrl+click on
  non-link text is a no-op. ctrl is used rather than alt because
  iTerm2/Terminal.app bind Option to native selection bypass.
- After a finished turn with a reply, an underlined dim `copy response` row, right-aligned, ends the
  transcript; a left click on it (or `/copy`) writes the last reply's markdown
  source to the clipboard via OSC 52 (needs terminal support; works over ssh).
  This is the markdown *source*, unlike a drag-copy which yields the rendered,
  ANSI-stripped text; both are kept — drag for what you see, `/copy` for the
  re-usable markup.
  The row is absent while a turn is pending.
- Chatbox chrome is configurable via `[input]` in `valen.toml`: `blink`
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
- Plan quota refreshes share one in-flight fetch per provider: the minute tick
  and the post-turn refresh never overlap on slow networks. The refresh
  dispatches by provider (`fetchQuotaCmd`): the OpenRouter balance on
  `openrouter/…` models, the Go plan quota otherwise, and nothing on `zen/…`
  models (pay-as-you-go, no plan quota) — the hidden provider never refreshes.

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
(`/clear` `/think` `/save` `/help` `/history` `/sessions` `/tools` `/slots` `/errors` `/model`
`/effort` `/route` `/copy` `/new` `/compact` `/plan` `/quit`) resolve inside `submit`/`handleCommand`; `/model` fetches
the catalog asynchronously via `modelsLoadedMsg`. The footer quota refreshes
via `orCreditsMsg`/`usageMsg` from `fetchQuotaCmd` at `Init`, on each
one-minute `usageTickMsg`, after each `turnDoneMsg`, and on every model
switch.

Entry point: `main()` keeps flag parsing; if `stdin`/`stdout` are TTYs and
`-p` was not given, it builds config/registry/MCP once and runs the Bubble
Tea program with alt screen; otherwise it falls back to one-shot `chat()`.
`-q "<prompt>"` forces the TUI (stdout must be a TTY) and stashes the prompt
on `tuiModel.initialPrompt`, which `Init` submits once so the turn starts as
soon as the UI comes up; the positional arg is the prompt, never a workdir.

## History & recall

- Same `Turn` model + JSONL log at
  `$XDG_DATA_HOME/valen/sessions/<workdir>-<hash>/<session_id>.jsonl` (one
  folder per working directory, so projects never share a session list).
- History view: one row per turn (time, prompt snippet, tokens) plus older
  saved session files read-only. Sessions view (Ctrl+L): every saved session
  of this working folder: fixed datetime/model/turn columns with the title
  (first prompt, 60 runes) taking the remaining view width, model name as the
  last `/` segment with no thinking level — Enter
  resumes it (latest `Turn.History` checkpoint plus later legacy turns, same
  file continues). `r` opens an inline title editor prefilled with the
  selected title (`Enter` saves to the meta sidecar, `Esc` cancels); `x`
  deletes the selected log and its meta. Esc backs to the current session
  without arming the cancel confirm, so browsing sessions never counts toward
  stopping a running turn.
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
- Shown by default, dim + indented. Ctrl+O / `/think` toggles the display.
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

## File references (`@` picker)

Typing `@` at the start of a word opens an inline dropdown above the chatbox.
The query is everything between `@` and the cursor, matched with fzf-style
subsequence scoring (`github.com/sahilm/fuzzy`) over full relative paths, so
`notes` finds `src/deep/notes.txt` and queries may span folders. The walk is
recursive from the workdir, skips dependency/build dirs (`skippedDir`) and
secret `.env` names, caps at 20,000 entries, and is cached for 10 seconds or
until the next completed turn. Files and folders are listed together; a
folder carries a trailing `/` and can be inserted on its own.

↑↓ move (wrapping), Enter replaces the whole `@…` token with
`@relative/path ` and places the caret after it, Esc closes the dropdown and
keeps the text. Spaces are escaped as `\ ` so the token stays one word.
The dropdown reopens when the cursor moves back into an existing token and
closes on newline, submit, cursor exit, or leaving the chat view. Enter also
stages the picked file as a composer chip (see "Attachments"): the `@path`
text stays, and the file's text or image bytes ride the turn. A mention typed
by hand (no picker, no chip) stays plain reference text.

## Attachments

A turn can carry files, folders and screenshots beside the prompt. They are
turn-scoped and never stored in history: the typed prompt — with inlined text
attachments — is what persists in the session JSONL, so a resumed session
shows the tokens, not the bytes.

- **Chips.** Pending items render as one dim line above the chatbox,
  re-derived from the textarea on every keystroke:
  `attach: [img-1] 1280x720 84 KiB · [shot.png] 12 KiB`. Backspace deletes
  text only, so a chip disappears with its token before submit. Submit,
  `/clear`, `/new` and ctrl+c on a non-empty box all reset the composer.
- **Tokens.** Clipboard images get `[img-1]`, `[img-2]`, … (numbered per
  composer). Pasted files inside the workdir get `[basename]`, duplicates
  `[name-2.png]`; a drop from outside keeps its full path
  (`[/tmp/repro_test.go]`) so the model can tell it is not project-local.
  `@path` from the picker is its own token. Resolution is a map lookup over
  pending entries: `[README.md]` typed by hand (or generated by a model) is
  left verbatim unless a chip with that exact token is pending.
- **Paste (drag and drop).** All-path pastes become chips: lines are split,
  trimmed, wrapping quotes and `file://` stripped, `\ ` unescaped; every
  non-empty line must be an existing file or folder, else the paste stays
  literal text. Mixed prose and paths are never guessed at. An
  empty/whitespace-only paste is treated as a clipboard-image attempt.
- **Ctrl+V** runs the platform clipboard helper with a 3s timeout:
  `pbpaste -Prefer png` on macOS, `wl-paste` on Wayland, `xclip` on X11.
  Image bytes are cached content-addressed under
  `$XDG_STATE_HOME/valen/attachments/` and become an `[img-N]` chip; text
  falls back to the normal paste. On a miss the status row names what to
  install (`wl-clipboard`, `xclip`).
- **Cache.** `0600` files in a `0700` folder, swept at boot: entries older
  than 7 days are removed (one `ReadDir`, 200 entries max). The clipboard
  helper runs outside the `shell` tool, so it needs no network/permission
  gate for a local read.
- Caps, resizing and rejections come from `[attachments]` (see SPEC.md
  "Attachments"); a rejected item surfaces as a status notice and the turn
  still sends.

## Model switching

- `/model` (ctrl+p, or ctrl+m where the terminal sends disambiguated ctrl+m)
  fetches the live catalogs — OpenCode Go (`GET {base_url}/models`), OpenCode
  Zen (`GET https://opencode.ai/zen/v1/models`, same OpenCode key as Go) plus
  OpenRouter (`GET https://openrouter.ai/api/v1/models`) — and opens
  a picker: ↑↓ move, Enter switches, Space stars/unstars (focused or not — model IDs hold no spaces, so a typed space can never match the filter), j/k also move, Ctrl+F focuses the search field (ghost hint
  "ctrl+f to filter models"; Esc unfocuses it, a second Esc backs out). The
  picker search box is
  pinned to the full content width (like the chatbox) so typing never resizes
  it. All three providers list even
  without keys (curated fallbacks when offline/unkeyed); picking or sending
  with a model whose key is missing reports "OpenCode API key not found"
  or "OpenRouter key not found" naming the env var, instead of failing boot.
  Every row carries its
  provider prefix (`go/…`, `zen/…`, `openrouter/…`); Go and Zen transports
  (`/chat`, `/responses`, `/messages`) are marked, OpenRouter rows always
  converse over its OpenAI-compatible chat endpoint.
- The merged list is cached at `~/.config/valen/models.json` (beside
  `state.json`). With a cache the picker opens instantly from it and, if still
  open 5 seconds later, fetches live and swaps the catalog in place (search
  text and cursor row kept). Without one it fetches first, then writes it.
  No state path means no cache.
- Favourites are a plain YAML list (`- glm-5.3`, OpenRouter IDs keep their
  `openrouter/…` prefix) at
  `~/.config/valen/favourites.yaml` (`$XDG_CONFIG_HOME/valen/`). Starred
  models sort to the top of the picker with a ★, alphabetically within each
  group, and the cursor stays on the toggled row. Space writes the file;
  without a state path (tests, `-config <file>`) favourites are session-only.
- Favourite slots: ten assignable model + thinking-level + routing triples, like
  weapon slots. `alt+1`…`alt+9`, `alt+0` (the tenth) switch to the triple
  stored in that slot; `alt+shift+<digit>` stores the current model + thinking +
  routing there and says so in the status notice (`Slot 2 ← go/glm-5.3 · high.`).
  Stored at `~/.config/valen/slots.yaml`, one `<digit>: <model> [thinking]
  [floor|nitro|exacto]` line per slot (no thinking = provider default; no
  routing token = `none`; the two vocabularies are disjoint, so order is
  unambiguous); without a state path they are session-only. An empty slot
  reports it; a saved level the model no longer offers resets to the provider
  default with a note. Ctrl+S (or `/slots`) opens a Slots view (picker layout)
  listing all ten slots with model, thinking, routing and a `● current` marker:
  Enter switches to the slot, `x` unbinds it, Esc backs out. Works in chat and
  overlays (not during a question). Terminals must send Option/Alt as Meta;
  alt+shift+digit arrives as the US shifted symbol (`alt+!`) or, under Kitty
  disambiguate mode, CSI `<digit>;4u`.
- `/model <filter>` switches directly on an exact or unique-substring match,
  otherwise opens the filtered picker. `/model <known-id>` works offline.
- Switching keeps the conversation history; an incompatible thinking level
  resets to the provider default with a note.
- Both model and thinking persist to `~/.config/valen/state.json`
  (`$XDG_CONFIG_HOME/valen/`), never to `valen.toml`, and override the
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

| Key | Chat | Overlays (history/sessions/tools/errors/slots/help/detail) |
| --- | --- | --- |
| Enter | send turn | open selected row / resume session |
| Esc | cancel run / back | back to chat |
| Ctrl+C / Ctrl+D | cancel (busy) / quit (idle) | quit |
| Ctrl+J / Alt+Enter / Shift+Enter | newline in input (shift+enter where the terminal supports it) | — |
| PgUp/PgDn / wheel | scroll transcript (pauses autoscroll until back at bottom; clears any selection) | scroll view |
| Drag (left) over transcript | in-app select, auto-copy on release (status shows bytes); drag to top/bottom edge auto-scrolls | same |
| Shift+drag (Opt+drag in iTerm/Terminal) | terminal's native drag-select + copy (not intercepted) | same |
| Ctrl+click a link | open http(s) URL in the browser | same |
| Click `copy response` (right edge, under last reply) / `/copy` | copy last reply via OSC 52 | — |
| Ctrl+G | help | help |
| Ctrl+R / Ctrl+L / Ctrl+U / Ctrl+X | history / sessions / tools / errors | same |
| Ctrl+Y | cycle OpenRouter routing: none → floor → nitro → exacto, wraps (silent no-op on Go/Zen) | same |
| Tab | swap work ↔ plan mode (chatbox border cyan in plan mode, green in work mode; header shows `· plan`; placeholder becomes `Plan mode…  TAB to switch (/help)`; notices: `Plan Mode enabled: read only and planning` / `Work mode enabled`) | — |
| Ctrl+N | new blank session (fresh id + file; blocked while busy) | same |
| Ctrl+T | thinking level cycle low→high, wraps to low | same |
| Ctrl+V | paste a clipboard image as an attachment chip (text falls back to the normal paste) | same |
| Ctrl+O | toggle thinking display (shown by default) | toggle thinking display |
| Alt+1…9, Alt+0 / Alt+Shift+1…0 | favourite slots: switch to slot / store current model + thinking in slot | same |
| x | — | slots view: unbind the selected slot; sessions view: delete the selected session |
| r | — | sessions view: rename the selected title (Enter saves, Esc cancels) |
| Ctrl+B | toggle status details | toggle status details |
| Ctrl+P / Ctrl+M | model picker | model picker |
| `@` | file picker: ↑↓ move, Enter inserts `@path`, Esc closes | — |
| Ctrl+S | favourite slots view | favourite slots view |
| Ctrl+Q | quit | quit |
| ↑↓ / j/k | textarea cursor (PgUp scrolls); ↑ on an empty box recalls the last prompt | move selection (view scrolls to follow) |
| Space / Enter / Esc | — | question overlay: toggle / select+confirm / dismiss |

Function keys are gone; the footer only points at `/help`, which lists every
shortcut (and the shortcut line above).

Slash commands: `/help` `/history` `/sessions` (`/resume`, `/continue` — resume most recent) `/tools` `/slots` `/errors` `/think` `/save` `/copy`
`/clear` `/new` `/compact` (`/summarize` alias) `/plan` `/model [filter]` `/effort [level]` (`/thinking` alias) `/route [none|floor|nitro|exacto]` `/quit` (`:q` alias).

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
  detail, `/clear` `/think` `/model` `/effort` `/route` unknown-command paths, no
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
  OpenRouter footer hiding Go usage, the optional provider/tps/in-out
  metadata suffix (present and cleanly-dropped), Zen hiding the footer
  entirely,
  per-provider refresh dispatch.
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
