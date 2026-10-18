# Change: attachments

## Why

History is string-`Content` only (`main.go`, `messages.go:splitChatHistory`,
`responses.go`); there is no way to give the model a file, screenshot, or
directory. Opencode covers this with `@path` + composer attach + `-f`:
text → filename+text, dir → non-recursive listing, PNG/JPEG/GIF/WebP →
image media (bytes-sniffed; PDF/audio/video rejected), 20 MiB/item.

## What changes

1. `Attachment` resolve at submit time: stat → dir list (cap 100) vs text
   (reuse `read` caps) vs image sniff (magic bytes, 20 MiB cap). Reject with
   a user-facing notice, never a model-visible error.
2. One renderer: text/dir inline as fenced `<attachment name=…>`; images as
   parts per transport (chat MultiContent `data:` URL, messages image
   blocks, responses `input_image`).
3. `read` gains the same image path; transcript shows `[image name WxH]`.
4. TUI `@` fuzzy picker (over `glob **`) + composer chips; headless `-f`
   repeatable + `@path` expansion in prompt args.
5. Config `[attachments]` (`max_bytes` 20 MiB, `max_base64_bytes` 8 MiB,
   `max_text_bytes` 64 KiB); no resizer dep and no width/height keys —
   over-limit images are rejected, in-limit pass through.

## Non-goals

- Inline terminal image rendering; PDF/audio/video ingestion (reject with
  "export page as PNG first"); share URLs. Paste/drop input is
  `openspec/changes/attach-input/`.

## Success criteria

- Offline converter tests per transport (txt/dir/PNG/reject); `-f` +
  `@path` round trip; caps in `project.md` unchanged.
