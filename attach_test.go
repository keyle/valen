package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sashabaranov/go-openai"
)

// tinyPNG is a literal 1x1 transparent PNG; no golden files, no network.
const tinyPNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// tinyWebP is a literal 1x1 lossless WebP, for the x/image/webp decoder.
const tinyWebPB64 = "UklGRh4AAABXRUJQVlA4TBEAAAAvAAAAAAfQ//73v/+BiOh/AAA="

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(tinyPNGB64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func attachTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	old := getFileRoot()
	setFileRoot(root)
	t.Cleanup(func() { setFileRoot(old) })
	return root
}

func writeBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAttachResolve(t *testing.T) {
	root := attachTestRoot(t)
	writeTestFile(t, root, "notes.txt", "hello\nworld\n")
	writeTestFile(t, root, "sub/a.go", "package a\n")
	writeTestFile(t, root, "sub/b.go", "package b\n")
	writeBytes(t, filepath.Join(root, "shot.png"), tinyPNG(t))
	writeBytes(t, filepath.Join(root, "doc.pdf"), []byte("%PDF-1.7 not really"))

	cfg := testConfig("https://unused")
	paths := []string{"notes.txt", "sub", "shot.png", "doc.pdf", "missing.txt", "notes.txt"}
	refs, notices := resolveAttachments(context.Background(), paths, cfg)
	if len(refs) != 3 {
		t.Fatalf("refs=%d notices=%v", len(refs), notices)
	}
	byName := map[string]AttachmentRef{}
	for _, r := range refs {
		byName[r.Name] = r
	}
	txt := byName["notes.txt"]
	if txt.Kind != attachKindText || txt.Text != "hello\nworld\n" || txt.Lines != 2 {
		t.Fatalf("text ref=%+v", txt)
	}
	dir := byName["sub"]
	if dir.Kind != attachKindDir || !strings.Contains(dir.Text, "a.go") || !strings.Contains(dir.Text, "b.go") {
		t.Fatalf("dir ref=%+v", dir)
	}
	img := byName["shot.png"]
	if img.Kind != attachKindImage || img.MIME != "image/png" || img.W != 1 || img.H != 1 || img.Data == "" {
		t.Fatalf("image ref=%+v", img)
	}
	if len(notices) != 2 || !strings.Contains(strings.Join(notices, " "), "PDF") || !strings.Contains(strings.Join(notices, " "), "not found") {
		t.Fatalf("notices=%v", notices)
	}
	if got := promptWithAttachments("hi", refs); !strings.HasPrefix(got, "hi\n\nAttached file: notes.txt\n\nhello\nworld\n\nAttached directory: ") ||
		!strings.HasSuffix(got, "/sub\n\na.go\nb.go") {
		t.Fatalf("prompt=%q", got)
	}
}

func TestAttachResolveOversize(t *testing.T) {
	root := attachTestRoot(t)
	writeBytes(t, filepath.Join(root, "big.txt"), []byte("0123456789abcdef"))
	cfg := testConfig("https://unused")
	cfg.Attachments.MaxBytes = 8
	refs, notices := resolveAttachments(context.Background(), []string{"big.txt"}, cfg)
	if len(refs) != 0 || len(notices) != 1 || !strings.Contains(notices[0], "over the") {
		t.Fatalf("refs=%v notices=%v", refs, notices)
	}
}

func TestAttachResolveTruncatesText(t *testing.T) {
	root := attachTestRoot(t)
	writeTestFile(t, root, "long.txt", strings.Repeat("x", 100))
	cfg := testConfig("https://unused")
	cfg.Attachments.MaxTextBytes = 10
	refs, notices := resolveAttachments(context.Background(), []string{"long.txt"}, cfg)
	if len(refs) != 1 || len(notices) != 0 {
		t.Fatalf("refs=%v notices=%v", refs, notices)
	}
	if refs[0].Text != strings.Repeat("x", 10) || !refs[0].Trunc {
		t.Fatalf("ref=%+v", refs[0])
	}
	if got := attachTextBlock(refs); !strings.HasPrefix(got, "\n\nAttached file: long.txt (truncated)\n\n") {
		t.Fatalf("truncated block=%q", got)
	}
}

// noisyPNG encodes a deterministic w×h noise image: it does not compress, so
// the base64 cap can be exercised without a golden file.
func noisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	seed := uint32(1)
	for i := 0; i < w*h; i++ {
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		img.Pix[i*4], img.Pix[i*4+1], img.Pix[i*4+2], img.Pix[i*4+3] = byte(seed>>24), byte(seed>>16), byte(seed>>8), 255
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAttachResizeDimensions(t *testing.T) {
	root := attachTestRoot(t)
	writeBytes(t, filepath.Join(root, "wide.png"), noisyPNG(t, 3000, 1000))
	cfg := testConfig("https://unused")
	refs, notices := resolveAttachments(context.Background(), []string{"wide.png"}, cfg)
	if len(refs) != 1 || len(notices) != 0 {
		t.Fatalf("refs=%v notices=%v", refs, notices)
	}
	r := refs[0]
	if r.W != 2000 || r.H != 667 {
		t.Fatalf("resized ref=%+v", r)
	}
	raw, err := base64.StdEncoding.DecodeString(r.Data)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := imageDimensions(raw); w != 2000 || h != 667 {
		t.Fatalf("encoded image=%dx%d", w, h)
	}
}

func TestAttachResizeDisabledRejects(t *testing.T) {
	root := attachTestRoot(t)
	writeBytes(t, filepath.Join(root, "wide.png"), noisyPNG(t, 2100, 10))
	cfg := testConfig("https://unused")
	no := false
	cfg.Attachments.AutoResize = &no
	refs, notices := resolveAttachments(context.Background(), []string{"wide.png"}, cfg)
	if len(refs) != 0 || len(notices) != 1 || !strings.Contains(notices[0], "over the 2000x2000 image cap") {
		t.Fatalf("refs=%v notices=%v", refs, notices)
	}
}

func TestAttachResizeUnderBase64Cap(t *testing.T) {
	root := attachTestRoot(t)
	writeBytes(t, filepath.Join(root, "noise.png"), noisyPNG(t, 64, 64))
	cfg := testConfig("https://unused")
	cfg.Attachments.MaxBase64Bytes = 3000
	refs, notices := resolveAttachments(context.Background(), []string{"noise.png"}, cfg)
	if len(refs) != 1 || len(notices) != 0 {
		t.Fatalf("refs=%v notices=%v", refs, notices)
	}
	r := refs[0]
	if len(r.Data) > 3000 || r.W >= 64 || r.H >= 64 {
		t.Fatalf("image did not shrink under the cap: %d b64, %dx%d", len(r.Data), r.W, r.H)
	}
}

func TestWebPDecodes(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(tinyWebPB64)
	if err != nil {
		t.Fatal(err)
	}
	if mime := imageMime(raw); mime != "image/webp" {
		t.Fatalf("mime=%q", mime)
	}
	if w, h := imageDimensions(raw); w != 1 || h != 1 {
		t.Fatalf("dims=%dx%d", w, h)
	}
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("webp decode: %v", err)
	}
}

func TestLoadConfigAttachmentsTable(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "model = \"m\"\n[attachments]\nmax_bytes = 5\nmax_base64_bytes = 7\nmax_text_bytes = 9\nmax_width = 11\nmax_height = 13\nauto_resize = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Attachments.MaxBytes != 5 || cfg.Attachments.MaxBase64Bytes != 7 || cfg.Attachments.MaxTextBytes != 9 ||
		cfg.Attachments.MaxWidth != 11 || cfg.Attachments.MaxHeight != 13 || cfg.Attachments.autoResize() {
		t.Fatalf("attachments=%+v", cfg.Attachments)
	}
	cfg2, err := loadConfig(writeConfig(t, "model = \"m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg2.Attachments
	if got.MaxBytes != defaultAttachMaxBytes || got.MaxBase64Bytes != defaultAttachMaxBase64Bytes || got.MaxTextBytes != defaultAttachMaxTextBytes ||
		got.MaxWidth != defaultAttachMaxWidth || got.MaxHeight != defaultAttachMaxHeight || !got.autoResize() {
		t.Fatalf("defaults=%+v", got)
	}
}

func testImageRef(t *testing.T) AttachmentRef {
	t.Helper()
	raw := tinyPNG(t)
	return AttachmentRef{
		Name: "shot.png", Kind: attachKindImage, MIME: "image/png", Path: "/tmp/shot.png",
		W: 1, H: 1, Bytes: int64(len(raw)), Data: base64.StdEncoding.EncodeToString(raw),
	}
}

func TestAttachmentWireChat(t *testing.T) {
	img := testImageRef(t)
	msgs := []openai.ChatCompletionMessage{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "look [img-1]"},
	}
	wire := chatMessagesWithImages(chatSendMessages(msgs), []AttachmentRef{img})
	if wire[1].Content != "" || len(wire[1].MultiContent) != 3 {
		t.Fatalf("wire user msg=%+v", wire[1])
	}
	if got := wire[1].MultiContent[1].Text; got != "Attached file: /tmp/shot.png" {
		t.Fatalf("caption=%q", got)
	}
	if msgs[1].Content != "look [img-1]" || len(msgs[1].MultiContent) != 0 {
		t.Fatalf("stored history mutated: %+v", msgs[1])
	}
	raw, err := json.Marshal(wire[1])
	if err != nil {
		t.Fatal(err)
	}
	wantURL := `"type":"image_url","image_url":{"url":"data:image/png;base64,` + img.Data + `","detail":"auto"}`
	if !strings.Contains(string(raw), wantURL) || !strings.Contains(string(raw), `"type":"text","text":"look [img-1]"`) ||
		!strings.Contains(string(raw), `"text":"Attached file: /tmp/shot.png"`) {
		t.Fatalf("chat body=%s", raw)
	}
}

func TestAttachmentWireMessages(t *testing.T) {
	img := testImageRef(t)
	msgs := []messagesMessage{{Role: "user", Content: "look [img-1]"}}
	messagesWithImages(msgs, []AttachmentRef{img})
	raw, err := json.Marshal(msgs[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"type":"image"`,
		`"source":{"data":"` + img.Data + `","media_type":"image/png","type":"base64"}`,
		`"text":"look [img-1]"`,
		`"text":"Attached file: /tmp/shot.png"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("messages body=%s missing %s", raw, want)
		}
	}
	if !strings.Contains(string(raw), `"cache_control"`) {
		t.Fatalf("image block should carry the prompt-cache breakpoint: %s", raw)
	}
	// A tool-result user message must not be mistaken for the prompt.
	msgs = []messagesMessage{
		{Role: "user", Content: "look"},
		{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "c1"}}},
	}
	messagesWithImages(msgs, []AttachmentRef{img})
	if raw, _ := json.Marshal(msgs[0].Content); !strings.Contains(string(raw), "image/png") {
		t.Fatalf("image attached to the wrong message: %s", raw)
	}
	if raw, _ := json.Marshal(msgs[1].Content); strings.Contains(string(raw), "image/png") {
		t.Fatalf("image leaked into the tool result: %s", raw)
	}
}

func TestAttachmentWireResponses(t *testing.T) {
	img := testImageRef(t)
	input := []any{
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "x"},
		map[string]any{"role": "user", "content": "look [img-1]"},
	}
	responsesWithImages(input, []AttachmentRef{img})
	raw, err := json.Marshal(input[1])
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"input_image","image_url":"data:image/png;base64,` + img.Data + `"}`
	if !strings.Contains(string(raw), want) || !strings.Contains(string(raw), `"type":"input_text","text":"look [img-1]"`) ||
		!strings.Contains(string(raw), `"type":"input_text","text":"Attached file: /tmp/shot.png"`) {
		t.Fatalf("responses body=%s", raw)
	}
}

func TestReadImageDescriptor(t *testing.T) {
	root := attachTestRoot(t)
	writeBytes(t, filepath.Join(root, "shot.png"), tinyPNG(t))
	out, err := ReadTool().Fn(context.Background(), `{"path":"shot.png"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[image shot.png 1x1,") || !strings.Contains(out, "@shot.png") {
		t.Fatalf("descriptor=%q", out)
	}
}

// An image riding the turn must not be described as unattached: the model
// reads the path the caption named, and "ask the user to attach it" made it
// answer as if no image had arrived.
func TestReadImageDescriptorAttached(t *testing.T) {
	root := attachTestRoot(t)
	path := filepath.Join(root, "shot.png")
	writeBytes(t, path, tinyPNG(t))
	ctx := withTurnAttachments(context.Background(), []AttachmentRef{{
		Name: "shot.png", Kind: attachKindImage, MIME: "image/png", Path: path,
	}})
	out, err := ReadTool().Fn(ctx, `{"path":"shot.png"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "attached to this message") || strings.Contains(out, "ask the user to attach") {
		t.Fatalf("descriptor=%q", out)
	}
}

// Outside the workdir the @ picker cannot reach the file, so the hint names a
// paste instead of a mention.
func TestImageDescriptorOutsideRoot(t *testing.T) {
	attachTestRoot(t)
	out := imageDescriptor(context.Background(), "shot.png", filepath.Join(t.TempDir(), "shot.png"), tinyPNG(t))
	if !strings.Contains(out, "paste the image or its path") || strings.Contains(out, "@shot.png") {
		t.Fatalf("descriptor=%q", out)
	}
}

func TestAtPaths(t *testing.T) {
	root := attachTestRoot(t)
	writeTestFile(t, root, "notes.md", "x")
	writeTestFile(t, root, "dir/a.go", "package a")
	got := atPaths("read @notes.md and @dir, ignore @missing.go and me@example.com")
	if len(got) != 2 {
		t.Fatalf("paths=%v", got)
	}
	if got[0] != filepath.Join(root, "notes.md") || got[1] != filepath.Join(root, "dir") {
		t.Fatalf("paths=%v", got)
	}
}

func TestPastePathBecomeTokens(t *testing.T) {
	m, root := pickerTestModel(t)
	writeBytes(t, filepath.Join(root, "My Shot.png"), tinyPNG(t))
	writeTestFile(t, root, "notes.txt", "x")
	shot := filepath.Join(root, "My Shot.png")
	notes := filepath.Join(root, "notes.txt")
	pasted := `"` + shot + `"` + "\nfile://" + notes

	cmd, handled := m.handlePaste(tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(pasted)})
	if !handled || cmd != nil {
		t.Fatalf("handled=%v cmd=%v", handled, cmd)
	}
	value := m.input.Value()
	if !strings.Contains(value, "[My Shot.png]") || !strings.Contains(value, "[notes.txt]") {
		t.Fatalf("input=%q", value)
	}
	if len(m.pendingAttachments) != 2 {
		t.Fatalf("pending=%+v", m.pendingAttachments)
	}
	if m.pendingAttachments[0].Path != shot || m.pendingAttachments[1].Path != notes {
		t.Fatalf("pending=%+v", m.pendingAttachments)
	}
}

func TestPasteOutsidePathKeepsFullPath(t *testing.T) {
	m, root := pickerTestModel(t)
	outside := filepath.Join(t.TempDir(), "repro_test.go")
	writeBytes(t, outside, []byte("package main\n"))
	writeTestFile(t, root, "notes.md", "x")

	pasted := "file://" + outside + "\n" + filepath.Join(root, "notes.md")
	if _, handled := m.handlePaste(tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(pasted)}); !handled {
		t.Fatal("path paste should become chips")
	}
	want := "[" + outside + "] [notes.md] "
	if got := m.input.Value(); got != want {
		t.Fatalf("input=%q want %q", got, want)
	}
	if len(m.pendingAttachments) != 2 || m.pendingAttachments[0].Path != outside {
		t.Fatalf("pending=%+v", m.pendingAttachments)
	}
}

func TestPasteEscapedSpaces(t *testing.T) {
	m, root := pickerTestModel(t)
	writeTestFile(t, root, "My Shot.png", string(tinyPNG(t)))
	pasted := strings.ReplaceAll(filepath.Join(root, "My Shot.png"), " ", `\ `)
	if _, handled := m.handlePaste(tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(pasted)}); !handled {
		t.Fatal("escaped-space paste should become a chip")
	}
	if len(m.pendingAttachments) != 1 || !strings.Contains(m.input.Value(), "[My Shot.png]") {
		t.Fatalf("value=%q pending=%+v", m.input.Value(), m.pendingAttachments)
	}
}

func TestPasteMixedTextUnchanged(t *testing.T) {
	m, root := pickerTestModel(t)
	writeBytes(t, filepath.Join(root, "shot.png"), tinyPNG(t))
	pasted := "look at this\n" + filepath.Join(root, "shot.png")
	if _, handled := m.handlePaste(tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(pasted)}); handled {
		t.Fatal("mixed prose and paths must stay literal")
	}
	if len(m.pendingAttachments) != 0 || m.input.Value() != "" {
		t.Fatalf("pending=%+v value=%q", m.pendingAttachments, m.input.Value())
	}
}

func TestTokenReconcileDropsDeleted(t *testing.T) {
	m, root := pickerTestModel(t)
	writeTestFile(t, root, "notes.txt", "x")
	writeTestFile(t, root, "a.txt", "x")
	m.addPendingAttachment("[notes.txt]", filepath.Join(root, "notes.txt"))
	m.addPendingAttachment("[a.txt]", filepath.Join(root, "a.txt"))
	paths := m.reconcileAttachments("please read [notes.txt]")
	if len(paths) != 1 || paths[0] != filepath.Join(root, "notes.txt") {
		t.Fatalf("paths=%v", paths)
	}
	if len(m.pendingAttachments) != 0 || m.imgSeq != 0 {
		t.Fatalf("composer not reset: %+v seq=%d", m.pendingAttachments, m.imgSeq)
	}
}

func TestTUIAttachChip(t *testing.T) {
	m, root := pickerTestModel(t)
	writeBytes(t, filepath.Join(root, "shot.png"), tinyPNG(t))
	m.addPendingAttachment("[shot.png]", filepath.Join(root, "shot.png"))
	m.input.SetValue("see [shot.png]")
	chips := m.attachChips()
	if !strings.Contains(chips, "attach:") || !strings.Contains(chips, "[shot.png]") || !strings.Contains(chips, "1x1 1 KiB") {
		t.Fatalf("chips=%q", chips)
	}
	m.input.SetValue("see")
	if got := m.attachChips(); got != "" {
		t.Fatalf("chip should vanish with its token, got %q", got)
	}
	m.clearInput()
	if len(m.pendingAttachments) != 0 {
		t.Fatalf("clearInput kept chips: %+v", m.pendingAttachments)
	}
}

func TestTUIPickerRegistersAttachment(t *testing.T) {
	m, root := pickerTestModel(t)
	writeTestFile(t, root, "notes.md", "x")
	loadPickerIndex(t, m, root)
	typePickerKeys(t, m, "@not")
	mm, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(*tuiModel)
	if !strings.Contains(m.input.Value(), "@notes.md") {
		t.Fatalf("input=%q", m.input.Value())
	}
	if len(m.pendingAttachments) != 1 || m.pendingAttachments[0].Token != "@notes.md" {
		t.Fatalf("pending=%+v", m.pendingAttachments)
	}
}

func stubExecCommand(t *testing.T, fn func(name string, args ...string) (string, error)) {
	t.Helper()
	old := execCommand
	execCommand = fn
	t.Cleanup(func() { execCommand = old })
}

func TestClipboardImageProbe(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	png := tinyPNG(t)
	stubExecCommand(t, func(string, ...string) (string, error) { return string(png), nil })
	cfg := testConfig("https://unused")
	path, text, notice := attachClipboardImage(cfg)
	if notice != "" || path == "" || text != nil {
		t.Fatalf("path=%q text=%q notice=%q", path, text, notice)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(png) {
		t.Fatalf("cache file: %v", err)
	}
	if !strings.HasPrefix(path, attachCacheRoot()) || !strings.HasSuffix(path, ".png") {
		t.Fatalf("path=%q", path)
	}
}

func TestClipboardTextFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stubExecCommand(t, func(string, ...string) (string, error) { return "/tmp/notes.txt", nil })
	path, text, notice := attachClipboardImage(testConfig("https://unused"))
	if notice != "" || path != "" || string(text) != "/tmp/notes.txt" {
		t.Fatalf("path=%q text=%q notice=%q", path, text, notice)
	}
}

func TestClipboardEmptyKeepsNotice(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stubExecCommand(t, func(string, ...string) (string, error) { return "", fmt.Errorf("exit 1") })
	if _, _, notice := attachClipboardImage(testConfig("https://unused")); notice == "" {
		t.Fatal("a miss must explain itself")
	}
}

func TestClipboardOversizeRejected(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	png := tinyPNG(t)
	stubExecCommand(t, func(string, ...string) (string, error) { return string(png), nil })
	cfg := testConfig("https://unused")
	cfg.Attachments.MaxBytes = 8
	path, _, notice := attachClipboardImage(cfg)
	if path != "" || !strings.Contains(notice, "cap") {
		t.Fatalf("path=%q notice=%q", path, notice)
	}
	if entries, _ := os.ReadDir(attachCacheRoot()); len(entries) != 0 {
		t.Fatalf("oversize image was cached: %v", entries)
	}
}

func TestAttachCachePrune(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := attachCacheRoot()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old.png")
	fresh := filepath.Join(dir, "fresh.png")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	pruneAttachCache(time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old cache file survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh cache file removed: %v", err)
	}
}

func TestAttachTokensReachRequestBody(t *testing.T) {
	root := attachTestRoot(t)
	writeBytes(t, filepath.Join(root, "shot.png"), tinyPNG(t))
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write(completion(nil, "ok"))
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	m := newTestModel(t, cfg)
	path := filepath.Join(root, "shot.png")
	token := m.addPendingAttachment(m.fileToken(path), path)
	prompt := "look at " + token
	m.input.SetValue(prompt)
	cmd := m.submit(prompt)
	if cmd == nil {
		t.Fatalf("submit returned nil (notice=%q)", m.notice)
	}
	_ = cmd()
	if !strings.Contains(body, `"text":"look at [shot.png]"`) {
		t.Fatalf("token text lost: %s", body)
	}
	if !strings.Contains(body, `"type":"image_url"`) || !strings.Contains(body, "data:image/png;base64,") {
		t.Fatalf("image part missing: %s", body)
	}
	if len(m.pendingAttachments) != 0 {
		t.Fatalf("composer not reset: %+v", m.pendingAttachments)
	}
}

func TestHeadlessFileFlag(t *testing.T) {
	root := attachTestRoot(t)
	writeTestFile(t, root, "notes.txt", "hello attach")
	writeBytes(t, filepath.Join(root, "shot.png"), tinyPNG(t))
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		w.Write(completion(nil, "ok"))
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	var stdout, stderr strings.Builder
	err := chatWithAttachments(context.Background(), cfg, newClient(cfg, "key"), "key", nil, NewRegistry(), "hi", []string{"notes.txt", "shot.png"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) == 0 {
		t.Fatal("no request captured")
	}
	body := bodies[len(bodies)-1]
	if !strings.Contains(body, `Attached file: notes.txt\n\nhello attach`) || !strings.Contains(body, `hello attach`) {
		t.Fatalf("text attachment missing: %s", body)
	}
	if !strings.Contains(body, `"type":"image_url"`) || !strings.Contains(body, "data:image/png;base64,") {
		t.Fatalf("image attachment missing: %s", body)
	}
	if strings.Contains(stderr.String(), "valen:") {
		t.Fatalf("unexpected notices: %s", stderr.String())
	}
}
