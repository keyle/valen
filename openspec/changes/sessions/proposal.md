# Change: sessions (TUI-only)

## Why

Every run mints `ses-<unixnano>` and `/clear` wipes memory. A dropped TUI
means starting over. No headless/terminal flags — TUI overlay only.

## What changes

1. Title = first user prompt, 60 runes, in `<id>.jsonl.meta.json` sidecar
   (JSONL stays append-only) + per-turn `model`/`effort`.
2. TUI `/sessions` overlay (Ctrl+L): titles + model/effort, Enter resumes,
   `f` forks (copy history under a new ID).

## Non-goals

- Headless `--session/--continue/--fork` flags; share URLs, `import`,
  server-backed sync.

## Success criteria

- Overlay resume/fork test on seeded JSONLs; no session file rewritten.
