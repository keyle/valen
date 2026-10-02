# Change: workflow-tools

## Why

Deep tasks need progress tracking, reusable instructions, and scoped
delegation. Opencode ships `todowrite`, `skill`, and `task` (subagents);
today a valen run is one flat loop with no plan surface and no way to fence
off exploratory work. Build in ROI order so each step is independently
useful; foreground-only first (no job view, no background results plumbing).

## What changes

1. `todowrite`/`todoread` first (~100 lines): `Todo{id,content,status,
   priority}`, opencode schema so models already know it; in-memory +
   folded into the session JSONL, no new files.
2. `skill` second: `SKILL.md` discovery (`~/.config/valen/skills`,
   `.valen/skills`, plus `.opencode/`/`.claude/` compat); advertised as
   `ID: name — description`; `skill {id}` loads body + ≤10 supporting
   paths (contents not auto-loaded). Permission action `skill`, ID as
   resource.
3. `task` third: `task {agent,prompt}` spawns a nested foreground `runTurn`
   with narrowed permissions (`explore`: read/glob/grep/web only;
   `general`: full except nested `task`, depth ≤2). Shares `mcpMu`;
   interactive `task` batches stay serial like `AskUserQuestion`.

## Non-goals

- Background subagents + TUI job view; custom agent/mode authoring UI;
  HTTP skill catalogs.

## Success criteria

- Each tool round-trips offline (`TestTodo*`, `TestSkill*`,
  `TestTaskExploreFence` incl. depth cap + permission narrowing); no new
  runtime deps.
