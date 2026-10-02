# Tasks: opencode-parity (exploring — none started)

Ordered by "fully fledged" ROI per lightness cost. Each is small and
verifiable; stop after any task if it regresses memory (`project.md` caps)
or needs a new runtime dep (requires explicit approval).

## 0. Spec review (this change)

- [ ] 0.1 Review `proposal.md` + `design.md` against opencode docs (tools,
  permissions, attachments, compaction, agents, skills, commands, TUI, CLI)
  and confirm the P0/P1/P2 split. Decide: build P0 first, or split into
  per-track openspecs (`attachments`, `sessions-compaction`,
  `permissions`, `file-tools`, `workflow-tools`)?
- [ ] 0.2 Confirm non-goals hold: no `serve`/`web`/`share`/ACP, no SQLite,
  no LSP in this track, no plugin/themes system.

## P0 — fully-fledged blockers

- [ ] 1. Attachments
  - [ ] 1.1 `Attachment` resolve (text/dir/image sniff, 20 MiB/item cap,
    unsupported-type message) + `config.toml [attachments]` defaults.
  - [ ] 1.2 Wire text/dir as `<attachment>` text + images as parts on all
    three transports (`chat` MultiContent, `messages` image blocks,
    `responses` input_image); `read` image path with same limits.
  - [ ] 1.3 TUI `@` picker + composer chips + transcript placeholder;
    headless `-f/--file` + `@path` expansion. Offline converter tests.
- [ ] 2. Sessions
  - [ ] 2.1 `--session/--continue/--fork` flags + title capture; `/sessions`
    (`/resume`|`/continue`) overlay (resume/fork), JSONL stays append-only.
- [ ] 3. Compaction (unblocks perf 4.4)
  - [ ] 3.1 `compactSession` summary-mode (session model, `keep.tokens`
    15000, `buffer` 10%), `/compact` (`/summarize`), auto on too-long
    400 + token estimate; send-side only, disk log lossless.
- [ ] 4. Permissions
  - [ ] 4.1 Ordered `allow/ask/deny` evaluator + TOML table + once/always/
    reject via `promptUser` + project approvals file (never overrides
    `deny`) + `doom_loop` triple-repeat guard. Default preserves today.
- [ ] 5. File tools
  - [ ] 5.1 `read` dir listings (non-recursive, cap 100).
  - [ ] 5.2 `patch` atomic multi-file builtin (permission: `edit` × every
    path). Parse-fail writes nothing.
  - [ ] 5.3 Post-write formatter hook (`gofmt -w` when on PATH, no-op
    otherwise; `[format]` toggle).
  - [ ] 5.4 `grep` `literal`/`caseSensitive`/`limit`, `glob` `hidden`/`limit`.

## P1 — important, phased after P0

- [ ] 6. `todowrite`/`todoread` (in-memory + session fold, no new files).
- [ ] 7. `skill` loader + skills discovery (`~/.config/mods/skills`,
  `.mods/skills`, `.opencode/` + `.claude/` compat) + permission action.
- [ ] 8. `task` subagents foreground-only (`general`, `explore` policies,
  depth ≤2, shared `mcpMu`); background deferred.
- [ ] 9. AGENTS.md hierarchy (global→root→nested discovery, mtime update
  notices, compaction-safe re-inject).
- [ ] 10. File slash commands (`.mods/commands/**/*.md` + compat,
  frontmatter `agent/model/subagent`, `{{args}}`/sh expansion) + `/init`,
  `/export` via `$EDITOR`.
- [ ] 11. `/` palette + headless `--agent/--format/--title` parity.

## P2 — explicitly deferred (rationale in design.md)

- [ ] 12. LSP tool probe (server defs, downloads, JSON-RPC lifecycle) —
  separate openspec only if code-intelligence ROI beats process cost.
- [ ] 13. Share URLs, `serve`/`web`/`attach`, ACP, background jobs, SQLite,
  plugins/themes — out of scope for the light harness.

## Gate (when a track builds)

- [ ] SPEC.md + SPEC-tui.md + `config.toml` comments updated for new
  behavior/keys; `gofmt -l .` clean, `go vet ./...` green,
  `go test ./...` green (`-race` for concurrency-adjacent changes).
