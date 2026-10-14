# Change: text-selection

## Why

Copy-paste is the weakest part of the TUI. Selection is delegated to the
terminal, which sees the *rendered* cells lipgloss emits: every transcript line
is padded to a fixed width and carries a left border / `PaddingLeft`, so dragging
on a wide terminal copies a column of trailing spaces after each line (and often
a border glyph or indent before it). The result is unusable for pasting a reply
into an issue, a commit message or another program.

There is a manual escape hatch (`alt+m` disables mouse reporting so the terminal
selects natively) and a `/copy` command for the last reply, but neither gives
clean, arbitrary selection. We should select text ourselves, from the logical
line content, and auto-copy on release.

## What changes

1. **In-app drag selection over the chat transcript.** A plain left-press starts
   a selection; motion extends it; release copies to the clipboard via the
   existing OSC 52 `clipboardWrite` and shows a status notice
   (`Copied N bytes to clipboard.`). Selection is computed against the *plain*
   line content (ANSI-stripped, display-width aware), with the common leading
   border/padding and trailing padding trimmed, so wide terminals no longer
   leak columns of spaces while a code sample keeps its relative indentation.
   Selected cells are highlighted with reverse video
   while dragging. Auto-copy on release is unconditional — no confirmation.
2. **Shift+drag stays the terminal's native selection.** Mouse reporting stays
   on, so terminals whose bypass modifier is Shift (most) still select natively;
   we ignore shift-modified drags and never auto-copy them.
3. **Hyperlinks: ctrl+click opens.** Link spans are detected in the rendered
   line text; `ctrl+left-click` on one opens it with the platform opener
   (`open` / `xdg-open`), http(s) only. ctrl is chosen over alt because
   iTerm2/Terminal.app use Option as their native selection-bypass modifier,
   which would swallow alt+click before we see it. Non-link ctrl+clicks are a
   no-op (never a selection). OSC 8 wrapping is dropped from scope: ctrl+click
   is the whole feature.
4. **Drop `alt+m`.** Once plain drag selects in-app and shift+drag stays native,
   the toggle is redundant. Remove the `mouseOff` state, the `alt+m` key, and the
   `tea.DisableMouse` path; keep `tea.WithMouseCellMotion()` always on.
5. **Docs together:** `/help` (TUI) and `SPEC-tui.md` mouse/copy section describe
   in-app selection, the shift bypass, auto-copy and ctrl+click links.

## Non-goals

- Selection inside overlays/lists (help, history, sessions, tools, errors,
  models, detail, question). This change covers the chat transcript only.
- Multi-click word/line selection, shift-click extend, drag-autoscroll past the
  viewport edge, or a selection that survives scroll/stream (scrolling clears it).
- Changing what `/copy` and the `copy response` row copy (still the last reply's
  markdown source).
- A clipboard abstraction beyond OSC 52; no new runtime dependency.

## Success criteria

- `go test -run 'TestTUISelection|TestTUILink|TestTUIMouse|TestTUIWheel' .` green;
  full `go test ./...` green under `network=true` (existing loopback fixtures).
- `TestTUISelectionTrimsPadding`: a drag across a full width-padded transcript
  line yields the logical text with no leading border/indent and no trailing
  spaces (the reported bug).
- `TestTUISelectionKeepsCodeIndent`: a drag across a rendered code sample keeps
  the code's relative indentation (the reported bug).
- `TestTUISelectionIgnoresShiftDrag`: shift+left press/motion/release leaves no
  selection and never calls `clipboardWrite`.
- `TestTUILinkCtrlClickOpens`: a ctrl+left-click on a rendered URL calls the
  opener with that URL; on non-link text it does not.
- `alt+m` no longer appears in `/help`, `SPEC-tui.md`, or the key handler.
