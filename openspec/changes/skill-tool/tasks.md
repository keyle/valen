# Tasks: skill-tool

Split out of `workflow-tools` W.2 so the change is one small tool, one test
file.

- [ ] 1. `skill.go`: discovery over four roots + frontmatter parse + cache.
  `TestSkillDiscoveryLoad`, `TestSkillMissingFrontmatter`,
  `TestSkillNoSkillsNoTool`.
- [ ] 2. `skill` tool: catalog description, body cap, ≤10 supporting paths,
  unknown-ID error. `TestSkillBodyCap`, `TestSkillPathsCap`,
  `TestSkillUnknownID`.
- [ ] 3. Register in the main loop and in both `task` subsets; `skill:<id>`
  permission action (allow until `permissions` lands).
  `TestSkillPermissionEval`.
- [ ] 4. Docs: SPEC builtins row, `config.toml` skill-root comment, help
  text; caps into `project.md`; gate (`gofmt -l .`, `go vet ./...`,
  `go test ./...`).
