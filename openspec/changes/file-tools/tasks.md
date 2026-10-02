# Tasks: file-tools

- [ ] 1. `read` dir listings (cap 100, sorted). `TestReadDir`.
- [ ] 2. `patch` two-phase atomic builtin + `edit`-per-path permission.
  `TestPatchAtomic` (multi-file ok; parse-fail writes nothing).
- [ ] 3. `gofmt -w` hook + `[format]` toggle (no-op off-PATH).
  `TestFormatHook`.
- [ ] 4. `grep`/`glob` opts (`literal/caseSensitive/limit`, `hidden/limit`).
  `TestGrepOpts`, `TestGlobOpts`; gate (`gofmt`, `go vet`, `go test ./...`).
