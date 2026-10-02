# Design: opencode-parity

Exploring only — no code. Each section names the current gap (file:line),
the opencode behavior, and the smallest shape that fits `project.md`
(single binary, stdlib-first, caps unchanged, `runTurn` UI-agnostic,
offline `httptest` tests).

## 1. Attachments (the known gap)

**Today:** history is `[]openai.ChatCompletionMessage` with string `Content`
(`main.go:seedHistory`, `chat()`). `splitChatHistory (messages.go:116)` and
`splitResponsesInput (responses.go)` assume text. `read` rejects directories
(`files.go:152-154`) and has no image path. Headless has no `-f` flag; TUI
input is a plain textarea with no composer attachments.

**Opencode:** `@path` fuzzy ref + attach-file/paste/drag in composer +
`opencode run -f file`; types: UTF-8 text → filename+text, directory →
non-recursive listing, PNG/JPEG/GIF/WebP → image media (bytes-sniffed, not
extension); SVG as text; PDF/AVIF/BMP/audio/video excluded with a "convert
first" message. Limits: 20 MiB decoded/item, picker 20 MiB total,
`attachment.image {auto_resize:true, max_width:2000, max_height:2000,
max_base64_bytes:5MiB}` with aspect-preserving downscale + progressive
PNG→JPEG retry; `read` tool applies the same image path (up to 20 MiB
source). After compaction, attachment data is replaced by a short
description.

**Fit for mods (stdlib-first problem: no image decoder in stdlib beyond
`image/png|jpeg|gif` + `x/image/webp`?):**

- New `Attachment {URI, Name, MediaType, Text?, DataBase64?}` on the turn
  input side, resolved once at submit time (not per round): stat → dir list
  vs text read (reuse `read` caps: 2000 lines/50 KiB pages) vs image sniff
  (magic bytes, 20 MiB cap). Reject oversize/unsupported with a one-line
  `error:` the model never sees (user-facing notice instead).
- Wire: `chat` → `MultiContent []ChatMessagePart` (`image_url data:` URLs);
  `messages` → `{"type":"image","source":{"type":"base64",…}}` blocks;
  `responses` → `input_image` items. Text/dir attachments inline as
  fenced `<attachment name=…>` text so all transports share one renderer.
- Config: `attachments = { max_bytes = 20MiB, image = { max_width,
  max_height, max_base64_bytes } }` in `config.toml`; omit = opencode
  defaults. Resizing needs a decoder/encoder — prefer `x/image` (already an
  indirect dep via glamour?) or shell out to nothing: if no resizer,
  pass-through-if-fits else reject (opencode documents pass-through when
  resizer unavailable).
- TUI: `@` triggers fuzzy `glob`-backed picker (reuse `matchGlob`), composer
  shows `+ file (12 KiB)` chips; `read` image results render as
  `[image diagram.png 800×600]` placeholder in transcript (no inline pixels
  in terminal). Headless: `-f/--file` repeatable flag + `@path` expansion in
  prompt args.
- Tests: offline fixtures (tiny PNG/GIF bytes, text, dir) through all three
  converters + cap/reject paths; no real images fetched.

## 2. Session lifecycle

**Today:** `newSessionID() ses-<unixnano> (main.go)`, one JSONL
(`session.go:56-65`), `/clear` resets memory (`tui.go`), history view lists
files read-only. No resume, fork, titles, or flags.

**Opencode:** `/sessions`|`/resume`|`/continue` picker, `--session`,
`--continue`, `--fork`, `--prompt`, `--title`, session titles (truncated
prompt), `import` from JSON/URL, share URLs (out of scope here).

**Fit:** add `--session ID` + `--continue` (last-modified JSONL) + `--fork`
(copy history, new ID) headless flags; TUI `/sessions` overlay reusing the
history view + `listSessions()` sorted newest-first (already there) with
Enter=resume, `f`=fork. Title = first user prompt truncated 60 runes,
stored as sidecar first line or filename suffix — keep JSONL append-only.
No server, no share.

## 3. Compaction (closes perf 4.4)

**Today:** history grows until provider 400s; `run*Turn` re-sends all of it
every round; `max_rounds=9999`. TUI shows full transcript.

**Opencode:** auto near limit (`keep.tokens 15000` recent + `buffer 10%`),
summary placed in front, later compactions update the same summary; manual
`/compact`|`/summarize`; native (Responses encrypted checkpoint,
same-provider/model only) vs summary; recovery: one compact-and-retry on
too-long rejection; earlier messages stay stored but unsent; long tool
output shortened + attachment data replaced by description in kept window.

**Fit:** summary-mode only (no native checkpoint — needs provider opaque
items + same-model pinning). Implementation: `compactSession(history,
keepTokens)` calls `runTurn` once with a summarizer system prompt using the
*session model*, prepends `Turn{Reply: summary}` marker, drops old rounds
past `keep.tokens` (tokens ≈ runes/4 heuristic, no tokenizer dep).
`/compact` forces it; auto triggers on 400-too-long or estimated
`promptTokens > modelLimit-buffer` (limits table per model family, fallback
128k). Config: `[compaction] auto=true, keep_tokens=15000, buffer=0.1`.
Stored JSONL keeps everything (compaction is send-side only) so `/history`
recall is lossless.

## 4. Permissions

**Today:** `checkFileAccess (files.go:84-116)` ask-on-escape via
`askPrompter`, remembered per-abs-path; `shell` runs unprompted by design
(SPEC non-goal). No patterns, no persistence, no repeat guard.

**Opencode v1→v2:** `permission.<tool> = allow|ask|deny` + object
pattern→action (`bash: {"git status *": allow}`, `read: {"*.env": ask}`),
wildcards (`mymcp_*: deny`), `external_directory` boundary rule,
`doom_loop` (3× identical input → guided recovery). Saved `always`
approvals: project-scoped allow rules proposed by the tool (shell proposes
command prefixes), never overriding `deny`; per-agent overlays.

**Fit (keep it a TOML table, no CEL/JS):**

```toml
[permissions]
default = "allow"          # or "ask" for paranoid mode
shell = "ask"              # shorthand
"shell:git status *" = "allow"
"read:*.env" = "ask"
"mcp_fs__*" = "deny"
[permissions.external_directory] = "ask"
[permissions.doom_loop] = "ask"
```

- Evaluator: last-match-wins over ordered rules (global → project →
  agent, mirroring opencode precedence); `deny` hard-stops, `ask` routes
  through `promptUser` (TUI overlay / headless stdin) with once/always/
  reject; `always` appends to project-local allowlist file (git-ignored,
  e.g. `.mods/approvals.toml`), never overriding `deny`.
- `doom_loop`: hash last 3 tool calls per turn; on triple-repeat, inject a
  `doom_loop` ask ("you called X 3× identically — change approach or
  confirm retry?") instead of running the 4th blindly.
- Default policy preserves today's behavior (`default allow`,
  `external_directory ask`, `shell allow`) so existing tests stay green;
  docs recommend `shell ask` for shared machines.
- Tests: pure evaluator table + `askPrompter` stub for once/always/reject,
  triple-repeat fixture; `-race` for the approval-map mutex.

## 5. File-tool parity

| Gap | Opencode | Fit |
|---|---|---|
| `read` on dirs | file *or* non-recursive listing | return `path/` immediate children (reuse `glob *`, cap 100) instead of "use glob" error |
| `patch` | atomic multi-file add/update/delete/move (`*** Add/Update/Delete File` hunks) | new builtin `patch {patchText}`; two-phase apply (parse→validate all→write all or none); permission resource = every affected path under `edit` action |
| formatter | runs configured formatter after edit | post-`write`/`edit`/`patch` hook: `gofmt -w` for `*.go` if on PATH, else no-op; `[format] enabled=true` + per-glob overrides later |
| `glob`/`grep` opts | `hidden`, `limit`, `literal`, `caseSensitive` | add `limit` (default 100, already capped), `literal` + `caseSensitive` to `grep` schema; `hidden` opt-in to `glob` (today skips only `.git`) |

All stay under `16k` truncate / `100` matches / `1MiB` file caps.

## 6. Workflow tools (phased)

- **`todowrite`/`todoread`** (build first): `Todo {id, content, status,
  priority}` in-memory + folded into session JSONL (no new files);
  schema mirrors opencode so models already know it. ~100 lines, highest
  ROI for deep tasks.
- **`skill`** (second): `SKILL.md` discovery (`~/.config/mods/skills`,
  `.mods/skills`, `.opencode/skills` compat, `.claude/skills` compat),
  advertised as `ID: name — description` list, `skill {id}` loads body +
  up-to-10 supporting paths (contents not auto-loaded). Permission action
  `skill`, ID as resource.
- **`task` subagents** (third, heaviest): `task {agent, prompt, background?}`
  spawns a nested `runTurn` with inherited-but-narrowed permissions
  (`explore`: reads/glob/grep/web only; `general`: full except `task`
  nesting depth ≤2). Foreground only in v1; background + result-callback
  needs TUI job view — deferred. Reuses `collectTools` + session scoping;
  MCP `mcpMu` shared.

## 7. Instructions + commands

- **AGENTS.md hierarchy:** load global (`~/.config/mods/AGENTS.md`) + walk
  workdir→root `AGENTS.md`s at session start (order: global, root→leaf),
  plus nested discovery on `read`/`glob` targets (nearest-first,
  deduped while in history; re-inject after compaction/revert). Edits
  detected by mtime before next turn → instruction-update notice. Keep the
  `system` override as the outer wrapper (agent/model system first, then
  instructions — opencode order).
- **Slash commands as files:** `.mods/commands/**/*.md` (plus
  `.opencode/commands` compat) → `/name` with frontmatter
  (`description`, `agent`, `model`, `subagent`); `{{args}}` + ````sh blocks
  evaluated at expansion (outside permission flow, trusted-source only).
  Built-ins to add: `/compact` (`/summarize` alias), `/sessions`
  (`/resume`|`/continue`), `/init` (AGENTS.md guided setup),
  `/export` (write md + open `$EDITOR`, replacing file-only `/save`).
  Existing `/model`|`/effort`|`/think`|`/history`|`/tools`|`/errors` stay.

## 8. TUI + CLI surfaces

- **`@` picker:** textarea key (`@`) opens fuzzy filter over `glob **`
  results (reuse `matchGoModel`-style substring match); Enter inserts
  `@rel/path` chip, resolved to an attachment at submit. No new deps
  (bubbles list reuses viewport+cursor patterns).
- **`/` palette:** input leading `/` filters built-in + file commands;
  preview shows `description`; Enter expands or runs picker (models/effort
  reuse `modelsLoadedMsg` async pattern).
- **Composer attachments:** pending chips row above input (`+ a.png
  800×600 120 KiB`); `esc` on chip removes; submit resolves via §1.
- **Headless:** `-f/--file` repeatable, `--agent build|plan|explore`,
  `--format default|json` (json = raw event stream for scripting),
  `--title`, `--continue/--session/--fork`. `opencode run` parity minus
  `--share/--attach`.

## Alternatives considered

- Full opencode config schema (`opencode.json`, plugins, themes, `tui.json`):
  rejected — TOML + comments-preserved persist stays; compat-shim only for
  `skills/` and `commands/` directory names.
- SQLite/WAL sessions, server split, background jobs v1: rejected — JSONL
  append-only + in-process Bubble Tea is the lightness story; revisit only
  with measured session-scale pain.
- LSP tool now: rejected for this change — needs 40+ server defs, downloads,
  long-lived JSON-RPC; track as `lsp-probe` follow-up reusing MCP stdio
  lifecycle if ever.
- Native (provider-side) compaction: rejected v1 — opaque checkpoints pin
  provider+model+endpoint and complicate the three-transport converters;
  summary-mode covers the 400-retry need.

## Verification (when built)

- Offline `httptest` per transport for attachments (text/dir/image parts,
  cap rejects), compaction (summary+keep window, send-side only),
  permissions (table + once/always/reject + doom-loop triple),
  `patch` atomicity (parse-fail writes nothing), skills/commands discovery,
  `@`/`-f` resolution. No real network, no image downloads.
- Final gate per `AGENTS.md`: `gofmt -l .` clean, `go vet ./...` green,
  `go test ./...` green (`-race` for parallel/approval paths); memory caps
  in `project.md` asserted by existing truncate/match/limit tests.
