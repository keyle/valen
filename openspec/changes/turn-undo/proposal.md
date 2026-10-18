# Change: turn-undo

Atomic file writes plus `/undo`: revert every file change a turn made, and
rewind the conversation to before it. This is the safety net that makes
letting a model edit freely feel reasonable.

## Why

`write`/`edit` call `os.WriteFile` directly (`files.go`), so a crash or a
full disk mid-write can truncate a file. Parallel `task` writers lock per
path but nothing makes a single write atomic. And there is no way back: a
turn that rewrote eight files can only be repaired by the model, from
memory, in another turn — which is exactly when the context is thinnest.
Opencode-style harnesses ship `/undo`; we don't.

## What changes

1. `writeFileAtomic(path, data, mode)`: temp file in the same directory,
   `chmod`, `fsync`, `rename`; used by `write`, `edit`, and `patch`.
   Preserves the existing mode and never follows a symlink out of the root.
2. A per-turn journal: before mutating, capture the file's bytes (≤1 MiB) or
   "did not exist". Journal is memory + a disk copy per session, so undo
   works after a resume.
3. `/undo [n]` (default 1) reverts the last `n` journaled turns: restore
   bytes and mode, delete files the turn created, then drop the turn(s) from
   the in-memory history, scrollback, and usage totals (the existing
   `rewindLastTurn` path). The JSONL stays append-only; the next checkpoint
   supersedes it.
4. A file modified after the turn is **not** clobbered: that path is skipped
   and reported (`skipped a.go: modified since the turn`). `/undo` prints the
   summary, including a reminder that `shell` writes are not journaled.
5. Caps: 1 MiB/file, 32 files/turn, 16 MiB/turn; overflow is recorded as a
   `partial` turn and `/undo` says how many files it could not restore.
6. `[undo] enabled = true`, `max_turns = 10` (journal retention) as keys.
   `/help` gains the command; SPEC-tui gains the flow.

## Non-goals

- Undo of shell-side effects (outside the journal; documented per turn).
- Redo, multi-step undo trees, or undoing turns from another session.
- Undoing compaction (send-side only) or checkpoint files.
- Git-based reverting: we do not touch the user's index or stash.

## Success criteria

- `TestWriteAtomicRename` (no partial file on write error),
  `TestUndoTurnRestoresAndDeletes`, `TestUndoRefusesModifiedFile`,
  `TestUndoPartialCaps`, `TestUndoAcrossRestart` (reload journal, revert).
  All offline, in `t.TempDir()`.
