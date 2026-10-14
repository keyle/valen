# Design: text-selection

## 1. Current state (verified)

- **Viewport / origin.** `View()` (`tui_view.go:84`) joins header, viewport,
  status, input, footer, then centers the block with
  `pad = (m.width - m.columnWidth()) / 2` (`tui_view.go:107`). The header is one
  row, so the viewport's screen origin is `(pad, 1)`; its width is
  `columnWidth()-4`, min 40 (`tui_update.go:373-378`), and mouse coordinates from
  bubbletea are absolute screen cells.
- **Mouse reporting.** `tea.WithMouseCellMotion()` is always on (`tui.go:687`);
  `Update` handles `tea.MouseMsg` at `tui_update.go:674`, routing the copy-row
  click (`copyClicked`, `tui_render_turn.go:64`) then forwarding to the viewport
  for wheel/scroll. `mouseOff` (`tui.go:429-435`) short-circuits all mouse input
  when `alt+m` (`tui_key.go:332-344`) disabled reporting.
- **Clipboard.** `clipboardWrite` writes OSC 52 to stdout
  (`tui_render_turn.go:39-41`); `/copy` and the inline row reuse it.
- **Rendering is the source of the padding bug.** Bodies render with
  `Width(bodyW)` (`tui_render_turn.go:243,261,277`) and left borders/padding
  (`userBodyStyle` `tui.go:40-42`, `asstBodyStyle` `tui.go:58`), so the styled
  line carries a border glyph and trailing spaces the terminal copies verbatim.
- **`ansi` is already imported** (`charmbracelet/x/ansi`: `Strip`, `StringWidth`,
  `SetSystemClipboard`) and `rivo/uniseg` is already in the module graph.
- **No OSC 8 anywhere.** Glamour renders a markdown link as its text plus a space
  and the resolved URL, both plain text (`glamour/ansi/link.go`), so URLs are
  recoverable from the stripped line without parsing markdown.

## 2. Selection model (new `tui_select.go`)

```go
type selPos struct{ line, col int } // col is a display cell, 0-based

type selection struct {
    active bool
    anchor selPos
    cursor selPos
}
```

Stored on `tuiModel` (UI state stays in `tui.go`). Positions index **visible
viewport lines** (row 0 = top of `m.viewport.View()`), so extraction never has to
reason about `YOffset` or the turn cache. `contentLine = YOffset + (msg.Y - 1)`.

Lifecycle in `tui_update.go` `case tea.MouseMsg`:

1. `msg.Shift` → ignore for selection (let the terminal's native bypass handle
   it); still forward wheel.
2. `msg.Alt && left press` → `openLinkAt(x, y)`; return (never a selection).
3. Plain left press → `selection{active:true, anchor:cursor:mapCell(x,y)}`.
4. Left motion while active → extend `cursor` (clamped to viewport).
5. Left release while active → `clipboardWrite(selectedText())`, set the notice
   (`Copied N chars.`), clear `active`.
6. Wheel / PgUp / PgDn → clear `active` (the line indexes would be stale).

`selectedText()` and `openLinkAt` rebuild the grid from the *current*
`m.viewport.View()` on demand; cost is bounded by the visible height.

## 3. Grid, copy fidelity, and hit-testing

`gridLines(viewport string) []gridLine` maps display columns to runes:

- Strip ANSI with `ansi.Strip`; keep only the plain runes.
- Width per grapheme cluster via `uniseg` (`ansi.StringWidth` as fallback), so
  CJK/emoji hit-test and copy correctly.
- `contentStart`: the first rune that is neither a space nor a box-drawing border
  glyph (`│ ┃ ┆ ┊ ┋`); `contentEnd`: one past the last non-space rune. Leading
  border/pad and trailing padding are decoration, not content.

`cellToRune(line, col)` and `runeToCell(line, i)` use the cumulative widths.

`selectedText()` walks rows `anchor.line..cursor.line` (ordered), slices each
line's content runes between the anchors (first line from `anchor.col`, last to
`cursor.col`, middles whole), `TrimRight`s spaces per line, and joins with `\n`.
This is exactly what removes the wide-terminal padding the user reported:
trailing `Width()` padding and leading border/`PaddingLeft` never enter the
result. Empty lines stay as blank lines.

## 4. Highlight while dragging

`highlightSelection(view string, sel selection, yOffset int) string` post-processes
the styled viewport output without rewriting existing escape sequences:

- Walk bytes; when a printable rune is crossed, increment the display column
  (same width function as §3). Insert `\x1b[7m` on entering the selected range and
  `\x1b[27m` on leaving, at rune boundaries. Reverse video composes with the
  surrounding color/bold instead of resetting them.
- **Re-assert after resets.** Markdown/syntax highlighting ends every coloured
  span with `\x1b[0m`, which clears reverse video. The first naive version
  therefore lit only the first word of each line. While inside the selected
  range, `reverseRange` re-emits `\x1b[7m` immediately after every SGR sequence,
  so the highlight survives the per-span resets (the surrounding colours still
  render, since reverse is now a mode applied on top).
- Applied in `View()` around `m.viewport.View()` (`tui_view.go:91`); a no-op when
  no selection is active, so the common render path is unchanged.

## 4a. Auto-scroll during a drag

The selection indexes visible viewport rows, so dragging past an edge has to
scroll the viewport *and* keep the anchor pinned to its content:

- On motion, if the cursor lands on row `0` or `Height-1`, `scheduleSelScroll`
  arms a `tea.Tick` loop paced by `[tui] select_scroll_ms` (default 150ms,
  `tuiSelScrollInterval`). `selScrollTicking` prevents a second ticker
  from a motion burst.
- Each `selScrollMsg` runs `selScrollStep`: `ScrollUp/Down(1)`, shifts
  `anchor.line` by ∓1 (a content row moves one row the other way), and parks the
  cursor on the edge row. It stops at `AtTop`/`AtBottom` — "keep adding unless
  past the end".
- The loop ends when the cursor leaves the edge, the selection is cleared, or the
  end is reached; `clearSelection` also drops `selScrollTicking`.
- `handleMouseSelection` now returns `(handled bool, cmd tea.Cmd)` so the arming
  tick rides back to Bubble Tea from the `tea.MouseMsg` case.
- Auto-scroll sets `m.follow = false` so a streaming turn can't yank the
  viewport back to the bottom mid-drag; the next submit/committed turn re-pins.

## 5. Hyperlinks

- Spans: on each stripped grid line, regex `https?://[^\s]+` (trim a trailing
  `)`/`]`/`.`/`,`), producing display-column spans.
- ctrl+click: `openLinkAt` finds the span containing the cell and calls
  `openURL(u)` — `open` on darwin, `xdg-open` on linux, detached
  (`exec.Command`, precedent `sandbox.go:266`); reject any scheme other than
  http/https before spawning. Notice `Opening link…` or a failure notice.
- ctrl is used rather than alt: iTerm2/Terminal.app bind Option to native
  selection bypass, so alt+click never reaches the app there. ctrl+click is
  also the familiar "open link in new tab" gesture.
- No OSC 8. The injector stays single-purpose (reverse video only); native
  link handling was a nice-to-have, not a requirement.

## 6. Removing `alt+m`

- Delete `mouseOff` (`tui.go:429-435`), the `alt+m` case (`tui_key.go:332-344`),
  and the `if m.mouseOff` early return (`tui_update.go:675-680`). Reporting stays
  on via the existing `tea.WithMouseCellMotion()`; drop the `tea.DisableMouse` /
  `tea.EnableMouseCellMotion` commands.
- Update `/help` (`tui_lists.go:402`) and `SPEC-tui.md:188-203` to describe
  in-app drag + auto-copy, shift+drag as the native bypass, and alt+click links.
- Remove `TestTUIMouseToggleSelection` (`tui_key_test.go:465-517`); keep the
  wheel tests (`tui_key_test.go:277-360`).

## 7. Decisions (resolved)

- **Selection is transcript-only and clears on scroll** (simplest, bounded,
  matches the reported use case).
- **Copy trims leading border/indent per line**; code-block indentation is
  therefore lost past the left edge. The alternative (keep indent, keep border
  spaces) reintroduces the reported bug, so rejected.
- **OSC 52 remains the only clipboard path**; `/copy` and the copy row are
  untouched.
- **Link modifier: ctrl+click** (was open: alt vs ctrl). iTerm2/Terminal.app use
  Option for native selection bypass, so alt+click is swallowed there; ctrl is
  unambiguous and familiar. See §5.
- **Shift-drag is ignored** (not forwarded as a selection), so terminals with a
  Shift bypass select natively and we never auto-copy a shift drag. In
  Option-bypass terminals (iTerm2/Terminal.app) use Option+drag for native
  selection; shift+drag is a deliberate no-op.
- **Auto-copy on release, no confirmation** (was open). The status bar shows
  `Copied N bytes to clipboard.`

## 8. Verification

- Targeted: `go test -run 'TestTUISelection|TestTUILink|TestTUIMouse|TestTUIWheel' .`.
- Grid/copy unit tests build a padded, bordered line and assert
  `selectedText()` has no leading border/indent/trailing spaces; a partial
  selection asserts exact slicing across multiple lines.
- Link tests inject a `openURL` test hook (like `clipboardWrite`) and assert the
  URL and that non-link or non-http(s) clicks are no-ops.
- Final gate: `gofmt -l .`, `go vet ./...`, `go test ./...` with `network=true`
  (existing loopback fixtures only; no new real network).
