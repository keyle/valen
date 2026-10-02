# Change: opencode-parity (exploring)

## Why

`mods` is a lighter-weight harness (~2% of opencode memory, single binary,
stdlib-first, offline tests) that covers the happy path: prompt → tool loop
(`shell`, `read`/`write`/`edit`/`glob`/`grep`, `web_search`/`web_fetch`,
`AskUserQuestion`, MCP) → TUI/headless reply, across three transports.

Against opencode (`sst/opencode` docs: tools, permissions, attachments,
agents, skills, commands, compaction, TUI, CLI) we are missing a handful of
load-bearing pieces that make the difference between "demo harness" and
"fully fledged daily driver". The user already named one: **attachments**.
This change explores what else was forgotten and proposes the smallest
parity set that keeps the lightness story intact.

## What changes (exploring, not yet building)

1. **Attachments**: `@path` / `-f` / paste-or-drop file, directory, and image
   context into the prompt, as multimodal content parts on all three
   transports; `read` returns images within limits. Text, directory listing,
   PNG/JPEG/GIF/WebP visible to the model; everything else rejected with a
   clear message (opencode: 20 MiB decoded cap, `auto_resize`/`max_width`/
   `max_height`/`max_base64_bytes`).
2. **Session lifecycle**: resume/continue/fork/list (`--session`, `--continue`,
   `/sessions`|`/resume`), titles, `/clear` alias `/new` kept. No share URLs
   yet.
3. **Compaction**: `/compact` + auto-compact near the context limit
   (summary beside recent `keep.tokens`, lossy by design). This closes the
   deferred `perf-parity-opencode 4.4` hole: today `max_rounds=9999` grows
   history unbounded until the provider 400s.
4. **Permissions**: per-tool `allow`/`ask`/`deny` with patterns + persisted
   `always` approvals, `external_directory` guard generalized, `doom_loop`
   (3× identical call) guard. Keeps today's fail-soft + outside-workdir ask
   as the default policy.
5. **File-tool parity**: `read` does files *or* non-recursive directory
   listings (today it errors "use glob"), atomic multi-file `patch` tool
   (opencode exposes `patch` to GPT-family, `edit`/`write` otherwise),
   formatter hook after `write`/`edit` (`gofmt` first, rest opt-in).
6. **Workflow tools**: `todowrite`/`todoread` (tiny, high value for deep
   tasks), `skill` loader + `.opencode/skills/` discovery, `task` subagents
   (`general`/`explore` first; background/depth limits follow-up).
7. **Instructions + commands**: `AGENTS.md` hierarchy auto-load (global →
   root → nested on `read`/`glob`), markdown-file slash commands
   (`.opencode/commands/*.md` + `commands:` config), missing built-ins
   (`/compact`, `/sessions`, `/init`, export opens `$EDITOR`).
8. **TUI/headless parity**: `@` fuzzy file picker, `/` command palette,
   attachment composer, `-f/--file`, `--agent`, `--format json`, `--title`.

## Non-goals (stay light)

- `serve`/`web`/`attach` remote backend, share/unshare URLs, ACP server.
- Background subagents v1, Code Mode JS sandbox, SQLite/WAL session store,
- LSP tool (`goToDefinition`, `findReferences`, …) — real value for code
  intelligence, but new long-lived processes + downloads; explicit follow-up,
  not this change (perf change already excluded it).
- Plugin system, themes, desktop notifications/attention sounds, mouse capture.

## Context

Local capacity today (`SPEC.md` + `SPEC-tui.md` are source of truth):

- Transports: `chat`/`messages`/`responses` all stream SSE with unary
  fallback (`stream.go`, `messages.go`, `responses.go`); history stays
  OpenAI-shaped `[]ChatCompletionMessage` with string `Content` only —
  **no multimodal parts anywhere**, so attachments need a wire change on
  all three converters.
- Tools: 9 builtins + MCP (`<server>__<tool>`, `mcpMu` serialized);
  bounded-parallel (sem 8, order-preserving) with `AskUserQuestion` serial
  fallback (`main.go:runToolCalls`). No `patch`, `todowrite`, `task`,
  `skill`, `lsp`, `list`.
- Sandbox: workdir root + `checkFileAccess` ask-on-escape via `askPrompter`
  (`files.go`); `shell` runs as-you with no approval (SPEC non-goal).
  No pattern rules, no saved approvals, no `doom_loop` repeat guard.
- Sessions: one JSONL per `ses-<unixnano>` (`session.go`), `/clear` resets
  memory only, history/tools/errors recall views read-only. No resume/fork,
  no titles, no token-based pruning.
- TUI: single in-process Bubble Tea (`tui.go`), instant echo, live
  `text_delta`/`thinking_delta` + tool timeline, `/model` (ctrl+p) +
  `/effort` pickers persisted to `config.toml`, quota footer. No `@`
  picker, no composer attachments, no session picker.
- Instructions: `system` defaults to an AGENTS.md-first hint (`main.go`,
  `config.toml`); nothing is auto-loaded into history.

Prior opencode research (docs + `sst/opencode` code, Oct 2026):

- Builtins: `read` (file *or* dir listing, pages 2000 lines/50 KiB, warms
  LSP), `edit` (single exact replace), `write` (+formatter), `apply_patch`
  (multi-hunk add/update/delete/move, atomic), `glob` (`rg --json`,
  limit 100), `grep` (ripgrep, `literal`/`include`/`limit`), `bash`,
  `webfetch`, `websearch`, `task` (subagents, bg opt-in), `skill`,
  `question`, `todowrite`/`todoread`, `lsp` (experimental flag),
  `external_directory` + `doom_loop` guards.
- Attachments (v2 docs): text file → filename+text, directory →
  non-recursive listing, PNG/JPEG/GIF/WebP → image media (SVG as text;
  PDF/AVIF/BMP/audio/video excluded); 20 MiB decoded/item, picker 20 MiB
  total; image normalize (`auto_resize`, `max_width/height 2000`,
  `max_base64_bytes 5 MiB`, progressive PNG→JPEG retry).
- Permissions v1 (`permission.*`: `allow`/`ask`/`deny` per tool + wildcards
  like `mymcp_*`) → v2 (`permissions[]`: `{action, resource, effect}` +
  base policy per agent + saved `always` approvals, project-scoped, never
  overriding `deny`).
- Sessions: `/sessions`|`/resume`|`/continue`, fork, titles, share URLs,
  `opencode run -f file --agent --format --title --continue`,
  `import` from JSON/URL. Compaction: auto near limit, summary + recent
  `keep.tokens 15000` + `buffer 10%`, manual `/compact`|`/summarize`,
  native (Responses encrypted checkpoint) vs summary modes.
- TUI: `@` fuzzy file ref, `/` commands (`/compact`, `/sessions`,
  `/share`, `/init` AGENTS.md setup, `/export` to `$EDITOR`), attention
  events, `tui.json` customization.

## Success criteria (for the exploring spec)

- A reviewer can answer "what are we missing to be fully fledged, and in
  what order do we build it?" from `proposal.md` + `design.md` + `tasks.md`
  alone, without re-reading opencode docs.
- Every item maps to a concrete SPEC/config/TUI surface (no "add plugins"
  hand-waving) and states its lightness cost (deps, memory caps, new
  processes).
- No code changes in this change; gates are doc review + follow-up
  task selection.
