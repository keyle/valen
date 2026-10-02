# Change: permissions

## Why

Today only outside-workdir escapes ask (`files.go:checkFileAccess`); `shell`
runs unprompted by design. Opencode gates every tool with ordered
`allow/ask/deny` (+ patterns like `shell:git status *`, wildcards like
`mymcp_*`), project-scoped saved `always` approvals (never overriding
`deny`), and a `doom_loop` guard (3× identical call → recover instead of a
blind 4th run).

## What changes

1. Ordered evaluator, last-match-wins (global → project → agent):
   `deny` hard-stops, `ask` routes through `promptUser` (TUI overlay /
   headless stdin), `allow` runs. TOML table:
   `shell="ask"`, `"shell:git status *"="allow"`, `"read:*.env"="ask"`,
   `"mcp_fs__*"="deny"`, `[permissions.external_directory]="ask"`.
2. Defaults preserve today (`default allow`, `external_directory ask`) so
   existing tests stay green; docs recommend `shell ask` on shared machines.
3. `always` appends a project-local allow rule to git-ignored
   `.mods/approvals.toml`; `deny` always wins.
4. `doom_loop`: hash consecutive identical tool calls per turn; on the 3rd
   repeat ask ("change approach or confirm retry?") instead of running blind.

## Non-goals

- CEL/JS policy expressions; per-agent overlays beyond the one table;
  console-managed org policies.

## Success criteria

- Pure evaluator table test + once/always/reject via stubbed `askPrompter`
  + triple-repeat fixture; `-race` green on the approval map.
