# Design: attachments

Direction 1 (`edit-loop`) package: resolver + three converter paths. Terse,
no new dependency, history stays OpenAI-shaped.

## 1. History stays `Content string`

`Turn.History` is `[]openai.ChatCompletionMessage` with string content, and
it is what gets checkpointed to JSONL. Images must not change that shape.

- Attachments are **turn-scoped request decoration**, not history metadata.
- `Turn` gains an in-memory-only field (`Attachments []AttachmentRef
  json:"-"`): `{Name, Kind: text|dir|image, MIME, Path, W, H, Bytes,
  Text}`.
- `runTurn` hands the slice to the transport builders; each round of the
  active turn renders the same image parts again, so the model never loses
  sight of the screenshot between tool calls.
- After the turn, only `Prompt` (the user text) and the text of text/dir
  attachments (already inlined in `Prompt`) survive on disk. A resumed
  session shows the token text, not the bytes — same rule as native
  Messages signatures / Responses items.

## 2. Resolve at submit, one place

`resolveAttachments(paths []string, cfg Config) ([]AttachmentRef, error)`
in new `attach.go`:

| input | result |
| --- | --- |
| dir | non-recursive listing, sorted, cap 100, `kind=dir` |
| text file | first 2000 lines / 64 KiB, `kind=text`, truncation note |
| PNG/JPEG/GIF/WebP by magic bytes | `kind=image`, decoded W×H |
| PDF/audio/video/other binary | user-facing notice, skipped |
| >20 MiB | notice, skipped |

`checkFileAccess(read)` runs first, so an outside file asks once per folder
like `read` does. Resolve errors never reach the model: they become a TUI
notice / stderr line and the turn still sends.

Size caps: file 20 MiB; base64 8 MiB (`[attachments] max_base64_bytes`);
images over the base64 cap are **rejected**, never resized (no resizer dep).

## 3. One text renderer, three image renderers

Text/dir content is appended to the user message as one fenced block so the
bytes are identical across transports (stable cache prefix):

```
<attachment name="notes.txt" lines="1-812">
…
</attachment>
```

Images ride beside the last user message:

- **chat**: `runModelTurn` builds the request with the last user message
  swapped for `MultiContent: [{Type: text}, {Type: image_url, ImageURL:
  data:<mime>;base64,… Detail:"auto"}]`. The history entry itself stays a
  plain string; only the wire copy changes, for every round of the turn.
- **messages**: `splitChatHistory` grows a last-message variant producing
  `messagesBlock{Type: "image", Source: {type: "base64", media_type,
  data}}`. `messagesBlock` needs a `Source json.RawMessage` field (it is
  currently field-by-field and has no `source`).
- **responses**: content item `{type: "input_image", image_url: "data:…"}`
  in the same user item as `input_text`. `responsesContent` grows
  `ImageURL string `json:"image_url,omitempty"``.

Sampling/thinking semantics are untouched: an attachment never adds a
request field beyond the user content.

## 4. `read` on an image

Tool results are text-only on chat, so `read` cannot return an image. It
returns a descriptor instead:

```
[image shot.png 1280x720, 84 KiB] ask the user to attach it with @shot.png
```

The model has the dimensions and can decide; the user attaches in one
keystroke. Images never sneak into a tool result mid-loop.

## 5. Input surfaces

- TUI: `@` fuzzy picker over `glob **` (reuse `tui_picker.go` list), chips
  rendered under the chatbox (`[image shot.png] 84 KiB`), token text in the
  textarea.
- Headless: `-f/--file` repeatable plus `@path` expansion in prompt args.
- `paste`/drag-drop tokens are `attach-input`, layered on top of this
  resolver and **not** in this change.

## 6. Config

```toml
[attachments]
# max_bytes = 20971520        # per item, before base64
# max_base64_bytes = 8388608  # encoded payload ceiling
# max_text_bytes = 65536      # inlined text/dir listing per item
```

Defaults are constants; the table only overrides them. Caps go to
`openspec/project.md` when the change lands.

## 7. Tests (offline)

`TestAttachResolve` (txt/dir/PNG/oversize/unsupported/missing), one
converter test per transport asserting the exact JSON shape for a tiny
1×1 PNG fixture, and `TestReadImageDescriptor`. No network, no golden files
— the PNG bytes are a literal in the test.
