# Tasks: text-selection

## Phase 1 — in-app selection + auto-copy

- [x] 1. New `tui_select.go`: `selPos`/`selection` model on `tuiModel`
  (`tui.go`); `gridLines()` (ANSI strip, grapheme widths, `contentStart`/
  `contentEnd`, decoration vs content), `cellToRune`/`runeToCell`, `mapCell`
  (screen → viewport line/col using the centering pad and viewport width),
  `selectedText()` (ordered range, per-line `TrimRight`, `\n` join).
  `TestTUISelectionTrimsPadding`, `TestTUISelectionMultiLine`.
- [x] 2. Wire mouse in `tui_update.go:674`: ignore `msg.Shift` for selection;
  plain left press starts, motion extends (clamped), release copies via
  `clipboardWrite` + notice and clears; wheel/pgup/pgdn clears the selection.
  Keep the copy-row click and wheel forwarding. `TestTUISelectionDragCopies`,
  `TestTUISelectionIgnoresShiftDrag`, `TestTUISelectionClearsOnScroll`.
- [x] 3. `highlightSelection()` reverse-video injection (`\x1b[7m`/`\x1b[27m`)
  and call it around `m.viewport.View()` in `View()` (`tui_view.go:91`).
  `TestTUISelectionHighlight`.

## Phase 2 — hyperlinks

- [x] 4. Link spans per stripped line (`https?://…`, trim trailing `)].,;:!?`);
  `urlAt(line, col)`. `TestTUILinkSpanParsing`.
- [x] 5. ctrl+left press → `openLinkAt` → `openURL` (darwin `open`, linux
  `xdg-open`, detached `exec.Command`, http(s) only). Test seam var like
  `clipboardWrite`. `TestTUILinkCtrlClickOpens`, `TestTUILinkCtrlClickNonLinkNoop`,
  `TestTUILinkRejectsNonHTTP`.
- [x] 6. (dropped) OSC 8 wrapping — ctrl+click is the whole feature.

## Phase 3 — drop alt+m + docs

- [x] 7. Remove `mouseOff` (`tui.go:429-435`), the `alt+m` case
  (`tui_key.go:332-344`), the `if m.mouseOff` early return
  (`tui_update.go:675-680`), and the test `TestTUIMouseToggleSelection`
  (`tui_key_test.go:465-517`). Reporting stays on.
- [x] 8. Docs together: `/help` (`tui_lists.go:402`) and `SPEC-tui.md:188-203`
  (in-app drag + auto-copy, shift+drag native bypass, ctrl+click links, alt+m
  gone); check `SPEC.md` mouse/copy references (`:35-37,567`).
- [x] 9. Gate: `gofmt -l .` clean, `go vet ./...` green, targeted
  `go test -run 'TestTUISelection|TestTUILink|TestTUIMouse|TestTUIWheel' .`
  then full `go test ./...` with `network=true`.
- [x] 10. Resolved (design §7): ctrl+click for links; shift-drag ignored;
  auto-copy on release with a status notice; no OSC 8.

## Phase 4 — follow-ups (styled-span highlight + drag auto-scroll)

- [x] 11. Highlight survives syntax/markdown colour resets: `reverseRange`
  re-asserts `\x1b[7m` after every SGR sequence while inside the selected cells,
  so the whole span stays lit instead of only the first word.
  `TestTUISelectionHighlightReassertsAfterReset`.
- [x] 12. Drag auto-scroll: reaching the top/bottom viewport row arms a
  `[tui] select_scroll_ms` tick (default 50ms; `selScrollMsg`/`scheduleSelScroll`)
  that scrolls one line and extends
  the selection to the revealed row, pinning the anchor to its content, until
  the edge is left or the transcript ends (`selScrollStep`). The mouse handler
  now returns a `tea.Cmd`. `TestTUISelectionAutoScrollStep`,
  `TestTUISelectionAutoScrollStopsAtEnd`, `TestTUISelectionAutoScrollArmsAndClears`,
  `TestTUISelectionAutoScrollTickExtends`, `TestTUISelectionDragArmsAutoScroll`.
- [x] 13. Docs: `SPEC-tui.md` mouse/selection paragraph + key table row.
- [x] 14. Gate: `gofmt -l .`, `go vet ./...`, `go test ./...` (`network=true`).

## Phase 5 — follow-ups (copy fidelity)

- [x] 15. Drag-copy keeps code indentation: `selectedText()` cuts each row from
  a selection-wide `gridBase` (least `contentStart`; bordered rows excluded)
  instead of each row's own `contentStart`, so a code sample no longer collapses
  to its first glyph. Bordered rows still cut at their content, so no border
  glyph leaks. `highlightSelection` uses the same base.
  `TestTUISelectionKeepsCodeIndent`, `TestTUISelectionNoBorderLeakOnMixed`.
- [x] 16. Docs: `SPEC-tui.md` selection paragraph, `SPEC.md` `tui_select.go`
  row, openspec design §3/§7/§8.
- [x] 17. Gate: `gofmt -l .`, `go vet ./...`, `go test ./...` (`network=true`).
