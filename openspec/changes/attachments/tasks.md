# Tasks: attachments

- [ ] 1. `Attachment` resolve (`attach.go`): sniff + caps + `[attachments]`
  config. `TestAttachResolve` (txt/dir/PNG/oversize/unsupported).
- [ ] 2. Wire parts on chat/messages/responses converters; `read` image
  path. `TestAttachmentWire*` offline per transport.
- [ ] 3. TUI `@` picker + chips + `[image]` placeholder; headless `-f` +
  `@path` expand. `TestTUIAttachChip`, `TestHeadlessFileFlag`.
- [ ] 4. Docs: SPEC + help + `config.toml` comments; gate
  (`gofmt`, `go vet`, `go test ./...`).
