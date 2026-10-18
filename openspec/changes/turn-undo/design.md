# Design: turn-undo

## 1. Atomic write

```go
func writeFileAtomic(path string, data []byte, mode os.FileMode) error
```

- temp `.<name>.valen-tmp-<rand>` in `filepath.Dir(path)` (same filesystem,
  so `rename` is atomic);
- `chmod` the temp file to `mode` before rename (the mode of the file being
  replaced, or 0644 for a new file — `write` already computes this);
- `f.Sync()` then `os.Rename`; `dir.Sync()` best-effort;
- on any error remove the temp file and leave the target untouched.

Symlinks: resolve with `filepath.EvalSymlinks` when the target exists and
write the resolved path, so `/tmp/link -> work/x` keeps pointing at `x`;
a link resolving outside the sandbox root is refused (same rule as
`checkFileAccess`). `write`/`edit`/`patch` all switch to this helper; no
other call site changes.

## 2. Journal

Hooked in the same three tools, **after** the access check and **before**
the write (and after the old bytes are in memory, which they already are for
`edit`/`patch`).

```go
type undoEntry struct {
    Path   string `json:"path"`            // absolute, resolved
    Existed bool  `json:"existed"`
    Mode   uint32 `json:"mode,omitempty"`
    Before string `json:"before,omitempty"` // blob name (sha256) when Existed
    After  string `json:"after"`            // sha256 of written bytes
}

type undoJournal struct {
    Turn    int          `json:"turn"`      // session turn index
    Session string       `json:"session"`
    Partial bool         `json:"partial,omitempty"`
    Skipped int          `json:"skipped,omitempty"`
    Entries []undoEntry  `json:"entries"`
}
```

Location, per project + session (same `<base>-<hash>` scheme as
`sessionsDir`):

```
$XDG_DATA_HOME/valen/undo/<session>/<turn>.json
$XDG_DATA_HOME/valen/undo/<session>/blobs/<sha256>
```

Blobs are written once (content-addressed) and shared across turns. A
per-turn mutex (the existing `lockPath` is per file; add one per turn in
`runTurn`'s context) serializes journal appends from parallel `task`
subagents.

Capture is skipped for paths outside the root (approved-outside writes are
still journaled if they were written by `write`/`edit`/`patch` — the path is
explicit, and undo is user-initiated; the same access check runs again).

Caps enforced at capture time: file >1 MiB → not recorded (`Skipped++`);
>32 entries or >16 MiB of blobs → stop recording, `Partial = true`. Blob
write failure → log and continue (`Skipped++`), never fail the tool.

Retention: at session start, delete journal files with `Turn` older than
`max_turns` (10) and blobs no longer referenced by a kept journal.

## 3. `/undo`

`/undo` and `/undo 2` (also `/undo 2` = last two turns). While busy: refuse
(`Working… press esc twice to cancel`).

Algorithm:

1. take the last `n` journals (newest first) that exist for this session;
2. for each turn, entries in **reverse** order:
   - if `!Existed` (created): read the file; if its sha256 equals `After`,
     remove it; else skip (`modified since`);
   - else: read the file; if the sha256 equals `After`, or the file is
     missing (the turn deleted it), restore the blob → `writeFileAtomic` with
     `Mode`; a different hash is skipped and reported (`modified since`);
3. delete the journal files of the undone turns;
4. drop the turns from memory: history (last user message match), scrollback,
   `session.Turns`, usage totals, `chatCache` — the same sequence
   `rewindLastTurn` already does, looped `n` times;
5. commit one transcript line:

```
undo: restored 5 files, deleted 1, skipped 1 (a.go: modified since) · shell writes not tracked
```

Empty journal (turn changed nothing, or `[undo] enabled = false`) → notice
`nothing to undo` and the turn is **not** dropped (no fake rewind).

A landed `patch` whose cross-file write failed half-way (see
`file-tools/design.md` §2) is exactly the case this repairs: the journal
has every file the tool attempted before it wrote anything.

## 4. Interaction with other changes

- `file-tools` needs `writeFileAtomic`; land §1 first or together.
- `shell` writes are outside the journal. Turns that ran `shell` get the
  reminder line; a model that needs reversible edits should use `patch`.
- Plan mode refuses all three tools, so no journal entries can exist.

## 5. Tests

`TestWriteAtomicRename` (write error leaves the old bytes; failed rename
removes temp), `TestUndoTurnRestoresAndDeletes` (two files: one edited, one
created), `TestUndoRefusesModifiedFile` (edit after the turn → skip),
`TestUndoPartialCaps` (33 files → partial, 32 restored),
`TestUndoAcrossRestart` (new `Session` from the same JSONL, journal on
disk), `TestUndoNothing` (no journal → no rewind), `TestUndoDropsTurnState`
(history/usage/scrollback). `-race` for the journal mutex.
