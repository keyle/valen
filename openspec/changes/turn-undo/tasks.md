# Tasks: turn-undo

Depends on `file-tools/` only for `patch` (tasks 1–3 work without it).

- [ ] 1. `writeFileAtomic` (+ symlink resolve) and switch `write`/`edit` to
  it. `TestWriteAtomicRename`.
- [ ] 2. Journal capture in `write`/`edit`/`patch` (blobs, caps, turn
  mutex, 10-turn retention). `TestUndoPartialCaps` unit level.
- [ ] 3. `/undo [n]`: restore/delete, modified-since guard, drop turn state,
  summary line, busy refusal. `TestUndoTurnRestoresAndDeletes`,
  `TestUndoRefusesModifiedFile`, `TestUndoNothing`,
  `TestUndoDropsTurnState`, `TestUndoAcrossRestart`.
- [ ] 4. `[undo]` config keys + `/help` row; journal cleanup on `/new` and
  `/clear`.
- [ ] 5. Docs: SPEC (atomic write, journal caps), SPEC-tui (`/undo` flow,
  shell-writes caveat), README limitation line; caps into `project.md`;
  gate (`gofmt -l .`, `go vet ./...`, `go test -race ./...`).
