# Change: file-tools

## Why

`read` errors on directories ("use glob"), edits are single exact-string
replaces, writes never format, and `grep`/`glob` lack the filters models
expect. Opencode: `read` returns files *or* non-recursive dir listings,
`patch` applies a multi-file hunk atomically, a formatter runs after edits,
and `grep`/`glob` take `literal/caseSensitive/limit` + `hidden/limit`.

## What changes

1. `read {path}` on a directory → non-recursive child listing (cap 100,
   same sort as `glob`), not an error.
2. New `patch {patchText}` builtin: parse → validate all paths (permission
   resource = every affected path under `edit`) → write all or none.
   Parse-fail writes nothing.
3. Post-write formatter hook: `gofmt -w` for `*.go` when on PATH, else
   no-op; `[format] enabled` toggle. More formatters only as follow-ups.
4. Schema-only opts (no new walk): `grep literal/caseSensitive/limit`,
   `glob hidden/limit`; existing caps (16k truncate, 100 matches, 1 MiB
   skip) unchanged.

## Non-goals

- Multi-formatter matrix, `.gitignore`-aware walks, mandatory `rg` dep.

## Success criteria

- `files_test.go` additions (dir list, atomic patch incl. fail-writes-
  nothing, fmt hook on/off, opt flags); caps unchanged.
