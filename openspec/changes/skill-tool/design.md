# Design: skill-tool

## Discovery

```
~/.config/valen/skills/            global
<workdir>/.valen/skills/           project
<workdir>/.opencode/skills/        compat
<workdir>/.claude/skills/          compat
```

Walk each root one level (`os.ReadDir`), accept `<id>/SKILL.md` or `<id>.md`.
`<id>` is the directory or file stem; it must match `^[a-zA-Z0-9_-]{1,64}$`
(same shape as tool names) so the catalog is safe to render. Project roots
are scanned after global ones and overwrite a duplicate ID.

No recursion, no symlink following, no `.git` walk: at most ~200 stat calls
at startup, once, cached in a slice. `/new` does not rescan (a new skill
needs a restart; documented).

## Frontmatter

```
---
name: Release checklist
description: Tag, build, and publish a release
---
body…
```

Parse only leading `---` … `---` lines with `key: value`; unknown keys are
kept verbatim in the body (a skill may use them for its own instructions).
`name` defaults to the id; `description` defaults to `""` (still listed, so
the model can see the ID). Malformed or missing frontmatter is not an error:
the whole file is the body.

## Catalog rendering

The tool description is the catalog:

```
Load a skill's instructions by ID. Available:
release-checklist: Tag, build, and publish a release
pdf-forms: Fill and validate PDF forms
```

- No skills → `skill` is not registered at all.
- >50 skills → first 50 by ID, plus `(+N more)`.
- Descriptions truncated to 120 runes, newlines collapsed.

This keeps a 50-skill catalog under ~5 KB, versus 1.6 MB of bodies.

## Loading

`skill {id}`:

1. exact ID lookup (case-insensitive, trimmed); unknown → error listing the
   available IDs (the model can self-correct);
2. body = file minus frontmatter, capped at 32 KiB on a rune boundary with
   `[... truncated N bytes]`;
3. supporting files = up to 10 paths **relative to the skill dir**, sorted,
   dirs suffixed `/`; no content;
4. result:

```
# skill: release-checklist (from .valen/skills)
<instructions>
[supporting files: scripts/tag.sh, REFERENCES.md]
```

Returned text is a normal tool result: 16k registry truncate still applies,
which is the tighter cap in practice. The 32 KiB body cap exists so a huge
skill cannot burn memory before truncation.

## Permission

`skill:<id>` is evaluated by the `permissions` evaluator if it lands first;
until then the gate is a plain allow (read-only, root-confined discovery).
A rule `"skill:release-*" = "ask"` must match. Plan mode allows `skill`
(it is a read).

## Subagents

`task` builds its registry per call; add `skill` to both `explore` and
`general` subsets (nested tools are recomputed, not shared). The catalog is
the same slice, so no extra discovery cost.

## Failure modes

- Skill root unreadable → skip it silently (debug log line), never fail boot.
- Body contains NUL/binary → returned as-is; the model sees gibberish and
  moves on. No validation.
- Two roots define the same ID → project wins, one debug log line.

## Tests

`TestSkillDiscoveryLoad` (four roots; duplicate; file and dir layouts),
`TestSkillMissingFrontmatter`, `TestSkillNoSkillsNoTool`,
`TestSkillBodyCap`, `TestSkillPathsCap`, `TestSkillUnknownID`;
`gofmt -l .`, `go vet ./...`, `go test ./...` green.
