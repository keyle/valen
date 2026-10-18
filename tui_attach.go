package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// tui_attach.go: the composer side of attachments. Pending entries live next
// to the chatbox and are never part of history; at submit their tokens are
// reconciled against the typed text, the paths go through the shared resolver
// and the resulting refs ride the turn (see attach.go).

// pendingAttachment is one composer chip: the token text sitting in the
// chatbox and the file it stands for.
type pendingAttachment struct {
	Token string // "[shot.png]", "[img-1]" or "@path"
	Path  string
	Label string // "1280x720 84 KiB", "12 KiB", "dir"
}

// clipboardPasteMsg carries one clipboard read back to the UI thread.
type clipboardPasteMsg struct {
	path   string
	text   string
	notice string
}

// noticeMsg shows a background notice (e.g. an attachment rejection).
type noticeMsg struct{ text string }

// cleanPastedPath normalizes one pasted drag-and-drop line: surrounding
// quotes and a file:// prefix stripped, backslash escapes undone.
func cleanPastedPath(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "file://"))
	if s == "localhost" || strings.HasPrefix(s, "localhost/") {
		s = strings.TrimPrefix(s, "localhost")
	}
	return unescapeFileToken(s)
}

// pastePathTokens turns a path-only paste into composer tokens. It reports
// false when any non-empty line is not an existing file or folder, so mixed
// prose and paths stay literal text.
func (m *tuiModel) pastePathTokens(text string) (string, bool) {
	var paths []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(text, "\n") {
		p := cleanPastedPath(line)
		if p == "" {
			continue
		}
		abs, err := resolveFilePath(p)
		if err != nil {
			return "", false
		}
		fi, err := os.Stat(abs)
		if err != nil || (!fi.Mode().IsRegular() && !fi.IsDir()) {
			return "", false
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		paths = append(paths, abs)
	}
	if len(paths) == 0 {
		return "", false
	}
	tokens := make([]string, 0, len(paths))
	for _, abs := range paths {
		tokens = append(tokens, m.addPendingAttachment(m.fileToken(abs), abs))
	}
	return strings.Join(tokens, " ") + " ", true
}

// fileToken is the unique "[basename]" token for a file inside the workdir.
// A file from outside keeps its full path, so the model can tell it is not
// part of the project and can reach it with the file tools. Duplicates get a
// "-2" suffix before the extension.
func (m *tuiModel) fileToken(abs string) string {
	name := filepath.Base(abs)
	if !inRootOrWorktree(getFileRoot(), abs) {
		name = abs
	}
	token := "[" + name + "]"
	if !m.hasPendingToken(token) {
		return token
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for n := 2; ; n++ {
		token = fmt.Sprintf("[%s-%d%s]", base, n, ext)
		if !m.hasPendingToken(token) {
			return token
		}
	}
}

func (m *tuiModel) hasPendingToken(token string) bool {
	for _, p := range m.pendingAttachments {
		if p.Token == token {
			return true
		}
	}
	return false
}

// addPendingAttachment registers a chip and returns its token.
func (m *tuiModel) addPendingAttachment(token, abs string) string {
	m.pendingAttachments = append(m.pendingAttachments, pendingAttachment{
		Token: token,
		Path:  abs,
		Label: attachmentLabel(abs),
	})
	m.syncInputHeight()
	return token
}

// addMentionAttachment registers the @ picker's chosen file. The mention text
// stays in the chatbox; the file rides as an attachment.
func (m *tuiModel) addMentionAttachment(abs string) {
	if _, err := os.Stat(abs); err != nil {
		return
	}
	token := "@" + escapeFileToken(relToRoot(abs))
	if m.hasPendingToken(token) {
		return
	}
	m.addPendingAttachment(token, abs)
}

// nextImageToken numbers clipboard images per composer.
func (m *tuiModel) nextImageToken() string {
	m.imgSeq++
	return fmt.Sprintf("[img-%d]", m.imgSeq)
}

// attachmentLabel is the chip suffix: image dimensions, else the size.
func attachmentLabel(abs string) string {
	fi, err := os.Stat(abs)
	if err != nil {
		return ""
	}
	if fi.IsDir() {
		return "dir"
	}
	if dims, ok := imageDimsLabel(abs); ok {
		return dims + " " + attachSize(fi.Size())
	}
	return attachSize(fi.Size())
}

// imageDimsLabel sniffs a file head for image dimensions (nothing for text).
func imageDimsLabel(abs string) (string, bool) {
	f, err := os.Open(abs)
	if err != nil {
		return "", false
	}
	defer f.Close()
	head, _ := io.ReadAll(io.LimitReader(f, 256<<10))
	if imageMime(head) == "" {
		return "", false
	}
	w, h := imageDimensions(head)
	if w <= 0 || h <= 0 {
		return "image", true
	}
	return fmt.Sprintf("%dx%d", w, h), true
}

// pendingForText are the chips still present in the chatbox; a token erased
// with backspace drops its chip before submit.
func (m *tuiModel) pendingForText() []pendingAttachment {
	if len(m.pendingAttachments) == 0 {
		return nil
	}
	text := m.input.Value()
	out := make([]pendingAttachment, 0, len(m.pendingAttachments))
	for _, p := range m.pendingAttachments {
		if strings.Contains(text, p.Token) {
			out = append(out, p)
		}
	}
	return out
}

// reconcileAttachments drops chips whose token left the text and returns the
// remaining paths. The composer is reset for the next message.
func (m *tuiModel) reconcileAttachments(text string) []string {
	var paths []string
	for _, p := range m.pendingAttachments {
		if strings.Contains(text, p.Token) {
			paths = append(paths, p.Path)
		}
	}
	m.clearAttachments()
	return paths
}

// clearAttachments resets the chips and the clipboard-image numbering.
func (m *tuiModel) clearAttachments() {
	m.pendingAttachments = nil
	m.imgSeq = 0
}

// attachChips is the dim line above the chatbox, "" when nothing is pending.
func (m *tuiModel) attachChips() string {
	items := m.pendingForText()
	if len(items) == 0 {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, p := range items {
		part := p.Token
		if p.Label != "" {
			part += " " + p.Label
		}
		parts = append(parts, part)
	}
	line := "attach: " + strings.Join(parts, " · ")
	if m.width > 0 {
		line = truncateRunes(line, m.columnWidth()-4)
	}
	return metaStyle.Padding(0, 2).Render(line)
}

// pasteClipboard reads the clipboard off the UI thread.
func (m *tuiModel) pasteClipboard() tea.Cmd {
	cfg := m.cfg
	return func() tea.Msg {
		path, text, notice := attachClipboardImage(cfg)
		return clipboardPasteMsg{path: path, text: string(text), notice: notice}
	}
}

// handlePaste routes a bracketed paste: all-path pastes become chips, an
// empty paste probes the clipboard (some terminals send nothing for images),
// anything else falls through to the textarea as literal text.
func (m *tuiModel) handlePaste(msg tea.KeyMsg) (tea.Cmd, bool) {
	text := string(msg.Runes)
	if strings.TrimSpace(text) == "" {
		return m.pasteClipboard(), true
	}
	tokens, ok := m.pastePathTokens(text)
	if !ok {
		return nil, false
	}
	m.input.InsertString(tokens)
	m.dismissTransientNotice()
	return m.syncFilePicker(), true
}

// insertClipboardText inserts clipboard text, still routing a path-only
// paste through the chip tokens.
func (m *tuiModel) insertClipboardText(text string) {
	if tokens, ok := m.pastePathTokens(text); ok {
		m.input.InsertString(tokens)
	} else {
		m.input.InsertString(text)
	}
	m.dismissTransientNotice()
}
