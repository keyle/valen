# Change: skill-tool

Direction 2: on-demand instructions. A `SKILL.md` file the model loads only
when it needs it keeps the system prompt and `AGENTS.md` small — the whole
point of skills versus pasting procedures into the prompt.

## Why

Today every standing procedure either bloats `AGENTS.md` (32 KiB cap,
re-sent every round) or is re-explained by the user each time. Opencode and
Claude Code both solve this with a discoverable `ID: description` catalog
plus a tool that returns the body on demand. valen has neither (grep: no
`skill` tool). `workflow-tools` sketched this as W.2; this change is that
sketch, standalone and ready to implement.

## What changes

1. Discovery at startup, read-only, no network:
   `~/.config/valen/skills` (global), `.valen/skills` (project),
   `.opencode/skills`, `.claude/skills` (compat). Layout `<root>/<id>/SKILL.md`
   or `<root>/<id>.md`. Project beats global on duplicate ID.
2. Minimal frontmatter parse (`name:`, `description:`), no YAML dep; body is
   everything after the closing `---`.
3. `skill {id}` returns the body (cap 32 KiB, truncation note) plus up to 10
   supporting paths relative to the skill dir. Contents are **not**
   preloaded; the model `read`s what it needs.
4. Catalog cost control: the tool is registered only when ≥1 skill exists;
   its description lists at most 50 `id: description` lines, each truncated
   to 120 chars. No skills → no schema, no prompt bytes.
5. Permission: new action `skill`, resource = ID (ordered rules live in
   `openspec/changes/permissions`; with no rules the default is allow, like
   every other read-only tool).
6. Available to `task` subagents (both kinds — it reads files only).
7. Docs: SPEC builtins row, `config.toml` comment for the search roots,
   `/help` mention.

## Non-goals

- HTTP skill catalogs / marketplaces; per-skill argument schemas; hot
  reload; executing scripts shipped with a skill (a skill is instructions,
  and its shell examples still go through `shell`'s gate).

## Success criteria

- `TestSkillDiscoveryLoad` (four roots, project wins, malformed frontmatter
  tolerated, no-skills → no tool), `TestSkillBodyCap`,
  `TestSkillPathsCap`, `TestSkillPermissionEval`. No new runtime deps; caps
  in `project.md`.
