# Design: file-tools

Direction 1 (`edit-loop`) package, part two: fewer rounds per edit. Each
decision below is reversible and stays inside the existing caps.

## 1. `read` on a directory

`os.ReadDir`, non-recursive, sorted, dirs suffixed `/`, cap 100 with a
`[... N more]` tail. Same refusal rules as `read` (secrets, outside root →
approval). This is a 20-line branch in `ReadTool`; `glob` stays for recursive
walks.

## 2. `patch`: many hunks, one call

Opencode's skill models already know, so reuse its envelope:

```
*** Begin Patch
*** Update File: path/to/a.go
@@ func main() {
-old
+new
*** Add File: path/to/b.go
+package b
*** Delete File: path/to/c.go
*** End Patch
```

Parse strictly into `[]filePatch{path, op, hunks}`. Any syntax error, any
missing `@@`/context, any unknown directive → one error, **zero writes**
(the model retries, as with a failed `edit`).

Per-path preconditions, in this order:

1. plan mode → refuse (same refusal text as `edit`);
2. `checkFileAccess(edit, abs)` per path (outside folders ask once);
3. resolve symlinks; a link that escapes the root is refused;
4. read the current bytes (cap 1 MiB) in memory; apply hunks in order;
   a hunk that does not match exactly fails the whole patch;
5. take per-file locks in sorted path order (`lockPath`) so parallel
   `task` writers cannot interleave and cannot deadlock;
6. write each result with the atomic helper (`turn-undo` §1), mode
   preserved.

Cross-file atomicity is honest: every file lands by rename, but a disk error
between files can leave the first files written. The tool reports which
paths landed; `/undo` restores the turn. No rollback journal inside `patch`.

Result text stays short: `patched 3 files: a.go (+12 -3), b.go (new),
c.go (deleted)`.

## 3. Format hook

After a successful `write`/`edit`/`patch` write batch:

- enabled by default, `[format] enabled = false` disables;
- only `*.go` → `gofmt -w` (PATH lookup, 5s timeout, cwd = root);
- runs on files inside the root only (never on approved-outside writes);
- failures append `[format] gofmt: …` to the tool output but never fail the
  tool (a syntax error mid-edit is normal);
- one process per batch, not per file.

The hook takes its own per-file lock after the write lock is released, so a
concurrent `read` never sees a half-formatted file.

## 4. Schema-only options

`grep`: `literal` (QuoteMeta), `caseSensitive` (default true today),
`limit` (clamped to 100). `glob`: `hidden` (dotfiles, default false),
`limit` (clamped to 1000). No new walk, no `.gitignore` parsing (TODO D2);
the skip lists and 1 MiB binary check are unchanged.

## 5. Tests

`TestReadDir` (sorted, dirs marked, cap), `TestPatchAtomic` (multi-file
update+add+delete in one call; parse failure writes nothing; one bad hunk
writes nothing), `TestPatchLocksSorted` (two goroutines, no interleaving),
`TestFormatHook` (formats a misindented file, no-op when disabled, exit 0
when gofmt missing), `TestGrepOpts`, `TestGlobOpts`.
