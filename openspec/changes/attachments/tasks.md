# Tasks: attachments

- [x] 1. `Attachment` resolve (`attach.go`): sniff + caps + `[attachments]`
  config. `TestAttachResolve` (txt/dir/PNG/oversize/unsupported).
- [x] 2. Wire parts on chat/messages/responses converters; `read` image
  path. `TestAttachmentWire*` offline per transport.
- [x] 3. TUI `@` picker + chips; headless `-f` + `@path` expand.
  `TestTUIAttachChip`, `TestHeadlessFileFlag`.
- [x] 4. Docs: SPEC + help + `config.toml` comments; gate
  (`gofmt`, `go vet`, `go test ./...`).
