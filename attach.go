package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/sashabaranov/go-openai"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// attach.go: turn-scoped attachments — files, folders and screenshots handed
// to the model beside the prompt. History stays string-Content only; text and
// folders are inlined into the prompt in opencode's shape, images are scaled
// to the image caps and ride the wire copy of the last user message (per
// transport) for every round of the turn.

const (
	attachKindText  = "text"
	attachKindDir   = "dir"
	attachKindImage = "image"

	// Defaults for the [attachments] table; the table only overrides them.
	// Byte and dimension caps match opencode's media.image settings.
	defaultAttachMaxBytes       = 20 << 20
	defaultAttachMaxBase64Bytes = 5 << 20
	defaultAttachMaxTextBytes   = 64 << 10
	defaultAttachMaxWidth       = 2000
	defaultAttachMaxHeight      = 2000

	maxAttachDirEntries = 100
	attachTextLines     = 2000
	// maxAttachPixels bounds a source decode: a small PNG can name a huge
	// bitmap, and the resizer has to hold it in memory.
	maxAttachPixels = 64_000_000
)

// AttachmentConfig mirrors the [attachments] table: per-item caps for the
// submit-time resolver. AutoResize is a pointer because the default is true;
// nil means enabled. An image over a cap is shrunk when it is, rejected when
// it is not.
type AttachmentConfig struct {
	MaxBytes       int64 `toml:"max_bytes"`
	MaxBase64Bytes int   `toml:"max_base64_bytes"`
	MaxTextBytes   int   `toml:"max_text_bytes"`
	MaxWidth       int   `toml:"max_width"`
	MaxHeight      int   `toml:"max_height"`
	AutoResize     *bool `toml:"auto_resize"`
}

// autoResize reports whether over-cap images are shrunk instead of rejected.
func (c AttachmentConfig) autoResize() bool {
	return c.AutoResize == nil || *c.AutoResize
}

// normalizeAttachmentConfig fills unset caps with their defaults.
func normalizeAttachmentConfig(c AttachmentConfig) AttachmentConfig {
	if c.MaxBytes <= 0 {
		c.MaxBytes = defaultAttachMaxBytes
	}
	if c.MaxBase64Bytes <= 0 {
		c.MaxBase64Bytes = defaultAttachMaxBase64Bytes
	}
	if c.MaxTextBytes <= 0 {
		c.MaxTextBytes = defaultAttachMaxTextBytes
	}
	if c.MaxWidth <= 0 {
		c.MaxWidth = defaultAttachMaxWidth
	}
	if c.MaxHeight <= 0 {
		c.MaxHeight = defaultAttachMaxHeight
	}
	return c
}

// AttachmentRef is one resolved attachment, turn-scoped only (never stored in
// history). Text/folders carry the inlined body; images carry the base64
// payload the transports wrap per protocol.
type AttachmentRef struct {
	Name  string
	Kind  string
	MIME  string
	Path  string
	W, H  int
	Bytes int64
	// Text is the inlined body for text/folders; Data is the base64 payload
	// for images.
	Text  string
	Data  string
	Lines int
	Trunc bool
}

// imageMime sniffs the four accepted image formats by magic bytes.
func imageMime(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(head, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "image/gif"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// imageExt is the cache-file extension for an accepted image MIME.
func imageExt(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".img"
}

// imageDimensions decodes the pixel size for the accepted formats.
func imageDimensions(raw []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// isBinaryData reports whether raw is not text: a NUL byte or invalid UTF-8
// in the head. A multi-byte rune cut at the head cap is not binary.
func isBinaryData(raw []byte) bool {
	head := raw
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return true
	}
	for i := 0; i < 3 && len(head) > 0 && !utf8.Valid(head); i++ {
		head = head[:len(head)-1]
	}
	return !utf8.Valid(head)
}

// riffForm reports a RIFF container of the named form (e.g. WAVE, AVI ).
func riffForm(head []byte, form string) bool {
	return len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == form
}

// unsupportedAttachmentHint names known non-text formats by magic bytes ("" for
// text or unknown binary). A PDF header is ASCII, so this runs before the
// binary sniff.
func unsupportedAttachmentHint(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return "PDF is not supported; export the page as PNG first"
	case bytes.HasPrefix(head, []byte("ID3")), bytes.HasPrefix(head, []byte("fLaC")),
		bytes.HasPrefix(head, []byte("OggS")), riffForm(head, "WAVE"), riffForm(head, "AVI "),
		len(head) >= 8 && string(head[4:8]) == "ftyp":
		return "audio/video is not supported"
	}
	return ""
}

const unsupportedAttachmentMessage = "only text, folders and PNG/JPEG/GIF/WebP images can be attached"

// imageDescriptor is what read returns for an image: tool results are
// text-only, so the model gets the dimensions instead of bytes. An image
// already riding this turn's user message points at its media part; anything
// else names the way to attach it.
func imageDescriptor(ctx context.Context, name, abs string, raw []byte) string {
	w, h := imageDimensions(raw)
	dims := "unknown size"
	if w > 0 && h > 0 {
		dims = fmt.Sprintf("%dx%d", w, h)
	}
	for _, r := range turnAttachments(ctx) {
		if r.Kind == attachKindImage && r.Path == abs {
			return fmt.Sprintf("[image %s %s, %s] attached to this message — inspect the image part, no need to read the file", name, dims, attachSize(int64(len(raw))))
		}
	}
	if inRootOrWorktree(getFileRoot(), abs) {
		return fmt.Sprintf("[image %s %s, %s] ask the user to attach it with @%s", name, dims, attachSize(int64(len(raw))), name)
	}
	return fmt.Sprintf("[image %s %s, %s] tool results carry no bytes; ask the user to paste the image or its path", name, dims, attachSize(int64(len(raw))))
}

// attachSize renders a byte count for chips and notices (12 KiB, 1.4 MiB).
func attachSize(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", (n+1023)/1024)
}

// resolveAttachments turns paths into attachment refs at submit time. Each
// rejected item becomes a user-facing notice and is skipped; the turn still
// sends. checkFileAccess runs first, so an outside file asks once per folder
// like read does.
func resolveAttachments(ctx context.Context, paths []string, cfg Config) ([]AttachmentRef, []string) {
	refs := make([]AttachmentRef, 0, len(paths))
	var notices []string
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		abs, err := resolveFilePath(p)
		if err != nil {
			notices = append(notices, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		ref, notice := resolveAttachment(ctx, abs, cfg)
		if notice != "" {
			notices = append(notices, notice)
			continue
		}
		refs = append(refs, ref)
	}
	return refs, notices
}

// resolveAttachment stats one path and classifies it: folder listing, image
// (magic bytes, base64 payload) or inlined text. The returned notice is
// user-facing and non-empty when the item is rejected.
func resolveAttachment(ctx context.Context, abs string, cfg Config) (AttachmentRef, string) {
	name := filepath.Base(abs)
	fail := func(msg string) (AttachmentRef, string) { return AttachmentRef{}, name + ": " + msg }
	// The clipboard cache is ours: the user pasted those bytes, so it never
	// prompts for folder access.
	if !withinRoot(attachCacheRoot(), abs) {
		if err := checkFileAccess(ctx, "read", abs); err != nil {
			return fail("not readable from here")
		}
	}
	if blocksSecret(abs) {
		return fail("holds secrets and cannot be attached")
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return fail("not found")
	}
	if fi.IsDir() {
		entries, err := os.ReadDir(abs)
		if err != nil {
			return fail("cannot be listed")
		}
		var lines []string
		for i, e := range entries {
			if i >= maxAttachDirEntries {
				break
			}
			n := e.Name()
			if e.IsDir() {
				n += "/"
			}
			lines = append(lines, n)
		}
		return AttachmentRef{
			Name: name, Kind: attachKindDir, Path: abs, Lines: len(entries),
			Text: strings.Join(lines, "\n"), Trunc: len(entries) > maxAttachDirEntries,
		}, ""
	}
	if !fi.Mode().IsRegular() {
		return fail("not a regular file")
	}
	ac := normalizeAttachmentConfig(cfg.Attachments)
	if fi.Size() > ac.MaxBytes {
		return fail(fmt.Sprintf("is %s, over the %s attachment cap", attachSize(fi.Size()), attachSize(ac.MaxBytes)))
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return fail("cannot be read")
	}
	if mime := imageMime(raw); mime != "" {
		data, outMime, w, h, notice := normalizeImage(raw, mime, ac)
		if notice != "" {
			return fail(notice)
		}
		return AttachmentRef{
			Name: name, Kind: attachKindImage, MIME: outMime, Path: abs, W: w, H: h, Bytes: fi.Size(),
			Data: data,
		}, ""
	}
	if hint := unsupportedAttachmentHint(raw); hint != "" || isBinaryData(raw) {
		if hint == "" {
			hint = unsupportedAttachmentMessage
		}
		return AttachmentRef{}, name + ": " + hint
	}
	body := raw
	trunc := false
	if len(body) > ac.MaxTextBytes {
		body, trunc = body[:ac.MaxTextBytes], true
		for len(body) > 0 && !utf8.Valid(body) {
			body = body[:len(body)-1]
		}
	}
	text := string(body)
	lines := strings.Count(text, "\n")
	if len(text) > 0 && !strings.HasSuffix(text, "\n") {
		lines++
	}
	kept := strings.SplitAfter(text, "\n")
	if len(kept) > attachTextLines {
		kept, trunc = kept[:attachTextLines], true
		lines = attachTextLines
	}
	return AttachmentRef{
		Name: name, Kind: attachKindText, Path: abs, Bytes: fi.Size(),
		Text: strings.Join(kept, ""), Lines: lines, Trunc: trunc,
	}, ""
}

// normalizeImage prepares an image for the wire. Bytes that already fit pass
// through untouched (mime and animation preserved); otherwise the image is
// scaled to the dimension caps, preserving aspect ratio, then encoded as PNG
// and JPEG at progressively smaller scales until max_base64_bytes is met — so
// the output mime can change. The returned notice is non-empty when the image
// is rejected.
func normalizeImage(raw []byte, mime string, ac AttachmentConfig) (data, outMime string, w, h int, notice string) {
	w, h = imageDimensions(raw)
	if w > 0 && h > 0 && w <= ac.MaxWidth && h <= ac.MaxHeight {
		if data = base64.StdEncoding.EncodeToString(raw); len(data) <= ac.MaxBase64Bytes {
			return data, mime, w, h, ""
		}
	}
	if !ac.autoResize() {
		if w > ac.MaxWidth || h > ac.MaxHeight {
			return "", "", 0, 0, fmt.Sprintf("is %dx%d, over the %dx%d image cap — resize it first", w, h, ac.MaxWidth, ac.MaxHeight)
		}
		if n := base64.StdEncoding.EncodedLen(len(raw)); n > ac.MaxBase64Bytes {
			return "", "", 0, 0, fmt.Sprintf("is %s as base64, over the %s cap — resize it first", attachSize(int64(n)), attachSize(int64(ac.MaxBase64Bytes)))
		}
		return "", "", 0, 0, "cannot be decoded"
	}
	if w <= 0 || h <= 0 {
		return "", "", 0, 0, "cannot be decoded"
	}
	if int64(w)*int64(h) > maxAttachPixels {
		return "", "", 0, 0, fmt.Sprintf("is %dx%d, too large to decode", w, h)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", "", 0, 0, "cannot be decoded"
	}
	encoders := []struct {
		mime string
		fn   func(io.Writer, image.Image) error
	}{
		{"image/png", png.Encode},
		{"image/jpeg", func(out io.Writer, im image.Image) error {
			return jpeg.Encode(out, im, &jpeg.Options{Quality: 85})
		}},
	}
	scale := math.Min(1, math.Min(float64(ac.MaxWidth)/float64(w), float64(ac.MaxHeight)/float64(h)))
	for attempt := 0; attempt < 8; attempt++ {
		nw := max(1, int(math.Round(float64(w)*scale)))
		nh := max(1, int(math.Round(float64(h)*scale)))
		frame := img
		if nw != w || nh != h {
			dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
			xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), xdraw.Src, nil)
			frame = dst
		}
		for _, enc := range encoders {
			var buf bytes.Buffer
			if enc.fn(&buf, frame) != nil {
				continue
			}
			if base64.StdEncoding.EncodedLen(buf.Len()) <= ac.MaxBase64Bytes {
				return base64.StdEncoding.EncodeToString(buf.Bytes()), enc.mime, nw, nh, ""
			}
		}
		scale *= 0.8
	}
	return "", "", 0, 0, "cannot be resized under the image caps"
}

// attachTextBlock inlines the text/folder bodies into the prompt in opencode's
// shape — "Attached file: <name>" (or "Attached directory: <path>"), a blank
// line, then the contents. One renderer for every transport keeps the bytes
// identical (stable cache prefix).
func attachTextBlock(refs []AttachmentRef) string {
	var b strings.Builder
	for _, r := range refs {
		var label string
		switch r.Kind {
		case attachKindText:
			label = "Attached file: " + r.Name
		case attachKindDir:
			label = "Attached directory: " + r.Path
		default:
			continue
		}
		if r.Trunc {
			label += " (truncated)"
		}
		fmt.Fprintf(&b, "\n\n%s\n\n%s", label, strings.TrimRight(r.Text, "\n"))
	}
	return b.String()
}

// promptWithAttachments appends the inlined text/folder bodies to the user
// prompt; images are not inlined (they ride the wire copy instead).
func promptWithAttachments(prompt string, refs []AttachmentRef) string {
	block := attachTextBlock(refs)
	if block == "" {
		return prompt
	}
	return strings.TrimRight(prompt, "\n") + block
}

// atPaths returns the @mentions in prompt that name an existing file or
// folder, for headless -p prompts (the TUI resolves mentions via the picker).
// The mention text itself stays in the prompt.
func atPaths(prompt string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, f := range strings.Fields(prompt) {
		if len(f) < 2 || f[0] != '@' {
			continue
		}
		p := unescapeFileToken(strings.TrimRight(f[1:], ".,;:!?)]"))
		if p == "" {
			continue
		}
		abs, err := resolveFilePath(p)
		if err != nil || seen[abs] {
			continue
		}
		fi, err := os.Stat(abs)
		if err != nil || (!fi.Mode().IsRegular() && !fi.IsDir()) {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}

// imageAttachments filters the image refs out of a turn's attachments.
func imageAttachments(refs []AttachmentRef) []AttachmentRef {
	var out []AttachmentRef
	for _, r := range refs {
		if r.Kind == attachKindImage && r.Data != "" {
			out = append(out, r)
		}
	}
	return out
}

// imageDataURL is the data: URL every transport shares for one image.
func imageDataURL(r AttachmentRef) string {
	return "data:" + r.MIME + ";base64," + r.Data
}

// turnAttachmentsKey carries the turn's attachments to the transports without
// touching runTurn's signature (runTurn stays UI-agnostic).
type turnAttachmentsKey struct{}

func withTurnAttachments(ctx context.Context, refs []AttachmentRef) context.Context {
	if len(refs) == 0 {
		return ctx
	}
	return context.WithValue(ctx, turnAttachmentsKey{}, refs)
}

func turnAttachments(ctx context.Context) []AttachmentRef {
	refs, _ := ctx.Value(turnAttachmentsKey{}).([]AttachmentRef)
	return refs
}

// imageCaption names a local image beside its media part, matching opencode's
// "Attached file:" line. Refs without a path carry no caption.
func imageCaption(r AttachmentRef) string {
	return "Attached file: " + r.Path
}

// chatMessagesWithImages swaps the turn's user message for a MultiContent copy
// carrying the image parts. The stored history entry stays a plain string.
func chatMessagesWithImages(msgs []openai.ChatCompletionMessage, imgs []AttachmentRef) []openai.ChatCompletionMessage {
	if len(imgs) == 0 {
		return msgs
	}
	i := -1
	for j := len(msgs) - 1; j >= 0; j-- {
		if msgs[j].Role == openai.ChatMessageRoleUser {
			i = j
			break
		}
	}
	if i < 0 {
		return msgs
	}
	parts := make([]openai.ChatMessagePart, 0, 1+2*len(imgs))
	parts = append(parts, openai.ChatMessagePart{Type: openai.ChatMessagePartTypeText, Text: msgs[i].Content})
	for _, img := range imgs {
		if img.Path != "" {
			parts = append(parts, openai.ChatMessagePart{Type: openai.ChatMessagePartTypeText, Text: imageCaption(img)})
		}
		parts = append(parts, openai.ChatMessagePart{
			Type:     openai.ChatMessagePartTypeImageURL,
			ImageURL: &openai.ChatMessageImageURL{URL: imageDataURL(img), Detail: openai.ImageURLDetailAuto},
		})
	}
	msgs[i].Content = ""
	msgs[i].MultiContent = parts
	return msgs
}

// messagesImageBlocks is the Anthropic block list for one image set.
func messagesImageBlocks(imgs []AttachmentRef) []any {
	blocks := make([]any, 0, 2*len(imgs))
	for _, img := range imgs {
		if img.Path != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": imageCaption(img)})
		}
		blocks = append(blocks, map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "base64", "media_type": img.MIME, "data": img.Data},
		})
	}
	return blocks
}

// messagesWithImages attaches the image blocks to the turn's user message:
// the last string-content (or already-stamped text) user turn. Tool-result
// user messages are skipped. The prompt-cache breakpoint moves to the last
// image, so the bytes ride the cached prefix across rounds.
func messagesWithImages(msgs []messagesMessage, imgs []AttachmentRef) {
	if len(imgs) == 0 {
		return
	}
	blocks := messagesImageBlocks(imgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		var next []any
		switch c := msgs[i].Content.(type) {
		case string:
			next = []any{map[string]any{"type": "text", "text": c}}
		case []any:
			if len(c) == 0 || blockIsToolResult(c[0]) {
				continue
			}
			for _, b := range c {
				if m, ok := b.(map[string]any); ok {
					delete(m, "cache_control")
				}
			}
			next = c
		default:
			continue
		}
		next = append(next, blocks...)
		next[len(next)-1].(map[string]any)["cache_control"] = ephemeralCache()
		msgs[i].Content = next
		return
	}
}

func blockIsToolResult(b any) bool {
	m, ok := b.(map[string]any)
	return ok && m["type"] == "tool_result"
}

// responsesWithImages turns the turn's user item into input_text + input_image
// content items, leaving the stored history untouched.
func responsesWithImages(input []any, imgs []AttachmentRef) {
	if len(imgs) == 0 {
		return
	}
	for i := len(input) - 1; i >= 0; i-- {
		m, ok := input[i].(map[string]any)
		if !ok || m["role"] != "user" {
			continue
		}
		text, ok := m["content"].(string)
		if !ok {
			continue
		}
		items := make([]responsesContent, 0, 1+2*len(imgs))
		items = append(items, responsesContent{Type: "input_text", Text: text})
		for _, img := range imgs {
			if img.Path != "" {
				items = append(items, responsesContent{Type: "input_text", Text: imageCaption(img)})
			}
			items = append(items, responsesContent{Type: "input_image", ImageURL: imageDataURL(img)})
		}
		m["content"] = items
		return
	}
}
