package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/sashabaranov/go-openai"
)

// Monokai dark palette.
var (
	mkBg      = lipgloss.Color("#272822")
	mkFg      = lipgloss.Color("#F8F8F2")
	mkComment = lipgloss.Color("#75715E")
	mkPink    = lipgloss.Color("#F92672")
	mkGreen   = lipgloss.Color("#A6E22E")
	mkYellow  = lipgloss.Color("#E6DB74")
	mkOrange  = lipgloss.Color("#FD971F")
	mkPurple  = lipgloss.Color("#AE81FF")
	mkCyan    = lipgloss.Color("#66D9EF")
	mkBorder  = lipgloss.Color("#49483E")
)

var (
	headerTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(mkPink)
	headerDimStyle   = lipgloss.NewStyle().Foreground(mkComment)
	userHeaderStyle  = lipgloss.NewStyle().Bold(true).Foreground(mkOrange)
	userBodyStyle    = lipgloss.NewStyle().Foreground(mkFg).
				BorderStyle(lipgloss.ThickBorder()).BorderLeft(true).BorderForeground(mkOrange).
				PaddingLeft(1)
	thinkHeaderStyle = lipgloss.NewStyle().Italic(true).Foreground(mkComment)
	thinkBodyStyle   = lipgloss.NewStyle().Italic(true).Foreground(mkComment).
				BorderStyle(lipgloss.NormalBorder()).BorderLeft(true).BorderForeground(mkBorder).
				PaddingLeft(1)
	toolLineStyle = lipgloss.NewStyle().Foreground(mkCyan).PaddingLeft(2)
	toolErrStyle  = lipgloss.NewStyle().Foreground(mkPink).PaddingLeft(2).Bold(true)
	toolDimStyle  = lipgloss.NewStyle().Foreground(mkComment).PaddingLeft(2)
	asstHeadStyle = lipgloss.NewStyle().Bold(true).Foreground(mkGreen)
	asstBodyStyle = lipgloss.NewStyle().Foreground(mkFg).PaddingLeft(1)
	errorTitleSty = lipgloss.NewStyle().Bold(true).Foreground(mkPink).
			BorderStyle(lipgloss.ThickBorder()).BorderLeft(true).BorderForeground(mkPink).
			PaddingLeft(1)
	metaStyle       = lipgloss.NewStyle().Foreground(mkComment)
	statusIdleStyle = lipgloss.NewStyle().Foreground(mkGreen)
	statusBusyStyle = lipgloss.NewStyle().Foreground(mkOrange)
	noticeStyle     = lipgloss.NewStyle().Foreground(mkYellow)
	errNoticeStyle  = lipgloss.NewStyle().Foreground(mkPink)
	footerStyle     = lipgloss.NewStyle().Foreground(mkComment)
	selectedStyle   = lipgloss.NewStyle().Foreground(mkBg).Background(mkGreen).Bold(true).Padding(0, 1)
	emptyStyle      = lipgloss.NewStyle().Foreground(mkComment).Padding(1, 2)
)

// monokaiGlamour is a glamour JSON style approximating Monokai Pro.
const monokaiGlamour = `{
  "document": {"block_prefix": "\n", "block_suffix": "\n", "color": "#F8F8F2", "margin": 2},
  "block_quote": {"indent": 1, "indent_token": "│ "},
  "paragraph": {},
  "list": {"level_indent": 2},
  "heading": {"block_suffix": "\n", "color": "#A6E22E", "bold": true},
  "h1": {"prefix": " ", "suffix": " ", "color": "#272822", "background_color": "#A6E22E", "bold": true},
  "h2": {"prefix": "## ", "color": "#F92672"},
  "h3": {"prefix": "### ", "color": "#66D9EF"},
  "h4": {"prefix": "#### ", "color": "#AE81FF"},
  "h5": {"prefix": "##### ", "color": "#E6DB74"},
  "h6": {"prefix": "###### ", "color": "#75715E", "bold": false},
  "text": {},
  "strikethrough": {"crossed_out": true},
  "emph": {"italic": true},
  "strong": {"bold": true},
  "hr": {"color": "#49483E", "format": "\n--------\n"},
  "item": {"block_prefix": "• "},
  "enumeration": {"block_prefix": ". "},
  "task": {"ticked": "[✓] ", "unticked": "[ ] "},
  "link": {"color": "#66D9EF", "underline": true},
  "link_text": {"color": "#66D9EF", "bold": true},
  "image": {"color": "#AE81FF", "underline": true},
  "image_text": {"color": "#75715E", "format": "Image: {{.text}} →"},
  "code": {"prefix": " ", "suffix": " ", "color": "#E6DB74", "background_color": "#3E3D32"},
  "code_block": {
    "color": "#F8F8F2", "margin": 2,
    "chroma": {
      "text": {"color": "#F8F8F2"},
      "error": {"color": "#F8F8F2", "background_color": "#F92672"},
      "comment": {"color": "#75715E"},
      "comment_preproc": {"color": "#FD971F"},
      "keyword": {"color": "#F92672"},
      "keyword_reserved": {"color": "#F92672"},
      "keyword_namespace": {"color": "#F92672"},
      "keyword_type": {"color": "#66D9EF"},
      "operator": {"color": "#F92672"},
      "punctuation": {"color": "#F8F8F2"},
      "name": {"color": "#F8F8F2"},
      "name_builtin": {"color": "#66D9EF"},
      "name_tag": {"color": "#F92672"},
      "name_attribute": {"color": "#A6E22E"},
      "name_class": {"color": "#A6E22E", "underline": true, "bold": true},
      "name_function": {"color": "#A6E22E"},
      "literal_number": {"color": "#AE81FF"},
      "literal_string": {"color": "#E6DB74"},
      "literal_string_escape": {"color": "#E6DB74"},
      "generic_deleted": {"color": "#F92672"},
      "generic_emph": {"italic": true},
      "generic_inserted": {"color": "#A6E22E"},
      "generic_strong": {"bold": true},
      "generic_subheading": {"color": "#75715E"},
      "background": {"background_color": "#272822"}
    }
  },
  "table": {},
  "definition_list": {},
  "definition_term": {},
  "definition_description": {"block_prefix": "\n🠶 "},
  "html_block": {},
  "html_span": {}
}`

type viewMode int

const (
	viewChat viewMode = iota
	viewHistory
	viewTools
	viewSessions
	viewErrors
	viewHelp
	viewDetail
	viewModels
	viewEffort
	viewAsk
)

// questionMsg delivers a model question batch to the UI thread; the background
// turn goroutine blocks on reply until the user confirms or dismisses.
type questionMsg struct {
	qs    []AskQuestion
	reply chan askReply
}

type askReply struct {
	answers []string
	err     error
}

// askState tracks one AskUserQuestion overlay interaction.
type askState struct {
	qs      []AskQuestion
	idx     int
	cursor  int
	phase   string // "pick", "own", "review"
	own     string
	picks   []map[int]bool // per-question selected option indexes (multi-select)
	ownText []string       // per-question free text when "type your own" chosen
	reply   chan askReply
}

// toolEventMsg streams tool progress from the background turn into the UI.
type toolEventMsg struct {
	kind   string
	text   string
	args   string
	output string
	id     string // tool call ID: matches rows by call, not by name
}

// pendingTool is one in-flight or finished tool call in the live transcript.
type pendingTool struct {
	name   string
	args   string
	output string
	done   bool
	id     string
}

// turnDoneMsg carries the finished turn back to the UI thread.
type turnDoneMsg struct {
	prompt   string
	thinking string
	answer   string
	tools    []ToolRecord
	usage    openai.Usage
	err      error
	history  []openai.ChatCompletionMessage
}

type historyItem struct {
	isFile  bool
	turnIdx int
	path    string
	label   string
	when    time.Time
	tokens  int
}

type toolItem struct {
	turnIdx int
	record  ToolRecord
}

type sessionItem struct {
	path   string
	title  string
	model  string
	effort string
	when   time.Time
	turns  int
}

// modelChoice is one row in the /model picker.
type modelChoice struct {
	id        string
	current   bool
	transport string
}

// modelsLoadedMsg carries the async model list back to the UI thread.
type modelsLoadedMsg struct {
	ids    []string
	err    error
	filter string
}

// usageMsg carries the OpenCode Go plan quota snapshot into the UI thread.
type usageMsg struct {
	usage GoUsage
	err   error
}

// usageTickMsg triggers a periodic quota refresh.
type usageTickMsg struct{}

// compactDoneMsg carries a finished manual compaction back to the UI thread.
type compactDoneMsg struct {
	summary   string
	err       error
	history   []openai.ChatCompletionMessage
	before    int
	after     int
	pruned    int
	compacted bool
	model     string
}

type tuiModel struct {
	cfg        Config
	cfgPath    string
	client     *openai.Client
	registry   *Registry
	session    *Session
	history    []openai.ChatCompletionMessage
	apiKey     string
	httpClient *http.Client
	goUsage    *GoUsage

	viewport viewport.Model
	input    textarea.Model
	spinner  spinner.Model

	prog   *tea.Program
	ctx    context.Context
	cancel context.CancelFunc

	busy          bool
	pendingPrompt string
	pendingSince  time.Time
	toolStatus    string
	// Live transcript state while a turn streams: tool calls in order plus
	// assistant text/thinking deltas so far. Cleared on turnDoneMsg.
	pendingTools    []pendingTool
	pendingText     string
	pendingThinking string
	lastStreamFlush time.Time
	// chatCache memoizes the rendered committed turns so streaming deltas
	// only re-render the live pending block, not the whole history. Committed
	// turns are immutable, so the prefix extends append-only; any key change
	// (turn count reset, width, thinking toggle, model switch) rebuilds.
	// Memory is one extra copy of the rendered transcript, proportional to
	// history already held in session.Turns; /clear drops it.
	chatCacheTurns int
	chatCacheW     int
	chatCacheThink bool
	chatCacheModel string
	chatCache      string
	// usageInflight dedupes quota refreshes: the minute tick and the
	// post-turn refresh share it, so slow networks never stack fetches.
	usageInflight bool
	notice        string
	noticeErr     bool

	showThink   bool
	showMeta    bool
	mode        viewMode
	prevMode    viewMode
	cursor      int
	detailTitle string
	detailBody  string

	modelChoices []modelChoice
	modelFilter  string
	effortLevels []string
	ask          *askState

	width  int
	height int
	ready  bool

	glam      *glamour.TermRenderer
	glamWidth int
	done      chan struct{}
}

func newTUIModel(cfg Config, client *openai.Client, registry *Registry, session *Session) *tuiModel {
	ta := textarea.New()
	ta.Placeholder = "Type a message…  (shift+enter newline · /help)"
	ta.ShowLineNumbers = false
	ta.SetWidth(78)
	ta.SetHeight(inputHeight(cfg))
	ta.CharLimit = 0
	// No partial background: the default CursorLine background only shades
	// the active line, leaving the rest of the box unshaded. Clear it so the
	// whole input shares one flat background.
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	// Steady block cursor when [input] blink=false: static mode renders the
	// block without starting the blink timer (Init skips textarea.Blink too).
	if !inputBlink(cfg) {
		ta.Cursor.SetMode(cursor.CursorStatic)
	}
	ta.Focus()

	vp := viewport.New(80, 20)

	sp := spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(lipgloss.NewStyle().Foreground(mkOrange)))

	m := &tuiModel{
		cfg: cfg, client: client, registry: registry, session: session,
		history:   seedHistory(cfg),
		viewport:  vp,
		input:     ta,
		spinner:   sp,
		showThink: true,
		mode:      viewChat,
	}
	m.refreshViewport()
	return m
}

func seedHistory(cfg Config) []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: effectiveSystem(cfg)}}
}

func (m *tuiModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	if inputBlink(m.cfg) {
		cmds = append(cmds, textarea.Blink)
	}
	cmds = append(cmds, m.spinner.Tick)
	if c := m.fetchUsageCmd(); c != nil {
		cmds = append(cmds, c, usageTick())
	}
	return tea.Batch(cmds...)
}

// askPrompt is the TUI's AskFunc: it forwards the questions to the UI thread
// and blocks until the user submits or dismisses.
func (m *tuiModel) askPrompt(ctx context.Context, qs []AskQuestion) ([]string, error) {
	reply := make(chan askReply, 1)
	if m.prog != nil {
		m.prog.Send(questionMsg{qs: qs, reply: reply})
	} else {
		return nil, errors.New("AskUserQuestion: no terminal UI")
	}
	select {
	case r := <-reply:
		return r.answers, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// dismissAsk unblocks the waiting tool call with err and restores the chat view.
func (m *tuiModel) dismissAsk(err error) {
	if m.ask == nil {
		return
	}
	try := m.ask.reply
	m.ask = nil
	if m.mode == viewAsk {
		m.mode = viewChat
	}
	select {
	case try <- askReply{err: err}:
	default:
	}
	m.input.Focus()
	m.refreshViewport()
}

// askAnswers folds the overlay state into one answer string per question.
func (m *tuiModel) askAnswers() []string {
	if m.ask == nil {
		return nil
	}
	out := make([]string, len(m.ask.qs))
	for i, q := range m.ask.qs {
		if m.ask.ownText[i] != "" {
			out[i] = m.ask.ownText[i]
			continue
		}
		var labels []string
		for j := range q.Options {
			if m.ask.picks[i][j] {
				labels = append(labels, q.Options[j].Label)
			}
		}
		out[i] = strings.Join(labels, ", ")
	}
	return out
}

func (m *tuiModel) handleAskKey(s string) (tea.Model, tea.Cmd) {
	if m.ask == nil {
		return m, nil
	}
	a := m.ask
	switch a.phase {
	case "pick":
		overflow := len(a.qs[a.idx].Options) // the "type your own" row
		rows := len(a.qs[a.idx].Options) + 1
		switch s {
		case "up", "k":
			if a.cursor > 0 {
				a.cursor--
			}
		case "down", "j":
			if a.cursor < rows-1 {
				a.cursor++
			}
		case " ":
			if a.qs[a.idx].MultiSelect && a.cursor < overflow {
				a.picks[a.idx][a.cursor] = !a.picks[a.idx][a.cursor]
			}
		case "enter":
			if a.cursor == overflow {
				a.phase = "own"
				a.own = ""
			} else if a.qs[a.idx].MultiSelect {
				anySelected := false
				for _, on := range a.picks[a.idx] {
					if on {
						anySelected = true
					}
				}
				if !anySelected {
					a.picks[a.idx][a.cursor] = true
				}
				a.advanceAsk()
			} else {
				a.picks[a.idx] = map[int]bool{a.cursor: true}
				a.advanceAsk()
			}
		case "esc":
			m.dismissAsk(errors.New("user dismissed the questions"))
		}
	case "own":
		switch s {
		case "enter":
			if strings.TrimSpace(a.own) == "" {
				a.phase = "pick"
			} else {
				a.ownText[a.idx] = strings.TrimSpace(a.own)
				a.advanceAsk()
			}
		case "backspace":
			if len(a.own) > 0 {
				a.own = a.own[:len(a.own)-1]
			}
		case "esc":
			a.phase = "pick"
		default:
			if len(s) == 1 {
				a.own += s
			}
		}
	case "review":
		switch s {
		case "enter", "y":
			a.reply <- askReply{answers: m.askAnswers()}
			m.ask = nil
			m.mode = viewChat
			m.input.Focus()
		case "esc", "n":
			m.dismissAsk(errors.New("user dismissed the questions"))
		}
	}
	m.refreshViewport()
	return m, nil
}

// advanceAsk moves to the next question, or to the confirmation stage.
func (a *askState) advanceAsk() {
	a.idx++
	a.cursor = 0
	a.phase = "pick"
	if a.idx >= len(a.qs) {
		a.idx = len(a.qs) - 1
		a.phase = "review"
	}
}

func (m *tuiModel) renderAsk() string {
	if m.ask == nil {
		return ""
	}
	a := m.ask
	cw := m.contentWidth()
	if a.phase == "review" {
		var b strings.Builder
		b.WriteString(m.wrapPlain("Confirm your answers — enter submits · esc cancels") + "\n\n")
		answers := m.askAnswers()
		for i, q := range a.qs {
			b.WriteString(m.wrapPlain(q.Question) + "\n" + m.wrapPlain("  → "+answers[i]) + "\n\n")
		}
		b.WriteString(m.wrapPlain("enter = submit · esc = cancel"))
		return b.String()
	}
	q := a.qs[a.idx]
	var b strings.Builder
	fmt.Fprintf(&b, "Question %d of %d", a.idx+1, len(a.qs))
	if q.Header != "" {
		fmt.Fprintf(&b, " · %s", q.Header)
	}
	if q.MultiSelect {
		b.WriteString(" · multi-select (space toggles)")
	}
	b.WriteString("\n\n" + m.wrapPlain(q.Question) + "\n\n")
	if a.phase == "own" {
		b.WriteString(m.wrapPlain(fmt.Sprintf("Type your answer: %s▏\n\n  enter keeps it · esc back", a.own)))
		return b.String()
	}
	for j, o := range q.Options {
		row := fmt.Sprintf("%d. %s", j+1, o.Label)
		if o.Description != "" {
			row += " — " + o.Description
		}
		if q.MultiSelect {
			row = "[" + map[bool]string{true: "x", false: " "}[a.picks[a.idx][j]] + "] " + row
		}
		row = breakLongWords(row, cw-2)
		if j == a.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(row) + "\n")
		} else {
			b.WriteString(m.wrapPlain("  "+row) + "\n")
		}
	}
	ownRow := breakLongWords(fmt.Sprintf("%d. Type your own answer", len(q.Options)+1), cw-2)
	if a.cursor == len(q.Options) {
		b.WriteString(selectedStyle.Width(cw).Render(ownRow) + "\n")
	} else {
		b.WriteString(m.wrapPlain("  "+ownRow) + "\n")
	}
	b.WriteString(m.wrapPlain("↑↓/jk move · enter select · esc cancel") + "\n")
	if q.MultiSelect {
		b.WriteString(m.wrapPlain("space toggles"))
	}
	return b.String()
}

// runningToolCount reports how many live tool calls have started but not
// finished yet — the open threads.
func (m *tuiModel) runningToolCount() int {
	n := 0
	for _, t := range m.pendingTools {
		if !t.done {
			n++
		}
	}
	return n
}

// finishPendingTool marks the in-flight tool call done, matched by call ID
// so parallel same-name calls keep their own outputs. Empty IDs fall back to
// the latest name match (older event sources).
func (m *tuiModel) finishPendingTool(id, name, output string) {
	if id != "" {
		for i := range m.pendingTools {
			if m.pendingTools[i].id == id && !m.pendingTools[i].done {
				m.pendingTools[i].done = true
				m.pendingTools[i].output = output
				return
			}
		}
	}
	for i := len(m.pendingTools) - 1; i >= 0; i-- {
		if m.pendingTools[i].name == name && !m.pendingTools[i].done {
			m.pendingTools[i].done = true
			m.pendingTools[i].output = output
			return
		}
	}
	m.pendingTools = append(m.pendingTools, pendingTool{name: name, output: output, done: true})
}

// flushStream re-renders the live transcript, throttled so a flood of deltas
// stays cheap. The committed turnDoneMsg always renders the final state.
func (m *tuiModel) flushStream() {
	if time.Since(m.lastStreamFlush) < 60*time.Millisecond {
		return
	}
	m.lastStreamFlush = time.Now()
	m.refreshViewport()
	if m.mode == viewChat {
		m.viewport.GotoBottom()
	}
}

// fetchUsageCmd reloads the Go plan quota off the UI thread. It is a no-op
// (nil) when no HTTP client/key is configured, e.g. in model-only tests, or
// when a fetch is already in flight (the minute tick and post-turn refresh
// would otherwise overlap on slow networks).
func (m *tuiModel) fetchUsageCmd() tea.Cmd {
	if m.httpClient == nil || m.apiKey == "" || m.usageInflight {
		return nil
	}
	m.usageInflight = true
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	hc, url, key := m.httpClient, m.cfg.BaseURL, m.apiKey
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, 10*time.Second)
		defer cancel()
		u, err := fetchGoUsage(ctx, hc, url, key)
		return usageMsg{usage: u, err: err}
	}
}

func usageTick() tea.Cmd {
	return tea.Tick(time.Minute, func(time.Time) tea.Msg { return usageTickMsg{} })
}

func (m *tuiModel) viewWidth() int {
	if m.viewport.Width > 0 {
		return m.viewport.Width
	}
	if m.width > 0 {
		return m.width - 4
	}
	return 80
}

func (m *tuiModel) markdownRenderer() *glamour.TermRenderer {
	w := m.viewWidth() - 6
	if w < 40 {
		w = 40
	}
	if m.glam != nil && m.glamWidth == w {
		return m.glam
	}
	r, err := glamour.NewTermRenderer(glamour.WithStylesFromJSONBytes([]byte(monokaiGlamour)), glamour.WithWordWrap(w))
	if err != nil {
		return nil
	}
	m.glam = r
	m.glamWidth = w
	return r
}

func (m *tuiModel) renderMarkdown(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	r := m.markdownRenderer()
	if r == nil {
		return s
	}
	out, err := r.Render(s)
	if err != nil {
		return s
	}
	return strings.TrimSpace(out)
}

func collectTools(msgs []openai.ChatCompletionMessage) []ToolRecord {
	byID := map[string]*ToolRecord{}
	var order []string
	for _, msg := range msgs {
		for _, tc := range msg.ToolCalls {
			byID[tc.ID] = &ToolRecord{Name: tc.Function.Name, Args: tc.Function.Arguments}
			order = append(order, tc.ID)
		}
		if msg.Role == openai.ChatMessageRoleTool {
			if r, ok := byID[msg.ToolCallID]; ok {
				r.Output = msg.Content
			}
		}
	}
	out := make([]ToolRecord, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

func truncatePlain(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// truncateRunes is truncatePlain but rune-safe, for wrapping display lines
// that may contain multibyte text.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 0 {
		return "…"
	}
	return string(r[:n]) + "…"
}

// contentWidth is the usable text width for transcript and overlay blocks:
// the viewport width minus chrome, clamped to a readable range.
func (m *tuiModel) contentWidth() int {
	w := m.viewWidth() - 4
	if w > 120 {
		w = 120
	}
	if w < 20 {
		w = 20
	}
	return w
}

// breakLongWords force-breaks single words (URLs, hashes, long tokens) that
// exceed width, so they cannot push a line off-screen. Normal word wrapping
// is left to the lipgloss Width styles applied at render time.
func breakLongWords(s string, width int) string {
	if width < 10 {
		width = 10
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if len([]rune(line)) <= width {
			continue
		}
		words := strings.Split(line, " ")
		var b strings.Builder
		for j, w := range words {
			if j > 0 {
				b.WriteByte(' ')
			}
			r := []rune(w)
			for len(r) > width {
				b.WriteString(string(r[:width]))
				b.WriteByte('\n')
				r = r[width:]
			}
			b.WriteString(string(r))
		}
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}

// wrapPlain wraps preformatted/plain text to the transcript width, for
// scrollback, question and detail views that render without markdown.
func (m *tuiModel) wrapPlain(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	return lipgloss.NewStyle().Width(m.contentWidth()).Render(breakLongWords(s, m.contentWidth()))
}

// show switches overlay modes and keeps focus/viewport consistent.
func (m *tuiModel) show(mode viewMode) {
	if mode != viewChat && mode != viewDetail {
		m.prevMode = mode
	}
	if mode == viewDetail {
		m.prevMode = m.mode
	}
	m.mode = mode
	m.cursor = 0
	if mode == viewChat {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
	m.refreshViewport()
	if mode == viewChat {
		m.viewport.GotoBottom()
	} else {
		m.viewport.GotoTop()
	}
}

func (m *tuiModel) cancelRun() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *tuiModel) submit(text string) tea.Cmd {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "/") {
		return m.handleCommand(trimmed)
	}
	if m.busy {
		m.notice = "Working… press esc to cancel."
		m.noticeErr = false
		return nil
	}
	hist := append(append([]openai.ChatCompletionMessage(nil), m.history...), openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: trimmed})

	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	m.cancel = cancel
	m.busy = true
	m.pendingPrompt = trimmed
	m.pendingSince = time.Now()
	m.toolStatus = "Contacting model…"
	m.pendingTools = nil
	m.pendingText = ""
	m.pendingThinking = ""
	m.lastStreamFlush = time.Time{}
	m.notice = ""
	m.done = make(chan struct{})
	m.refreshViewport()
	m.viewport.GotoBottom()

	cfg := m.cfg
	client := m.client
	apiKey := m.apiKey
	httpClient := m.httpClient
	registry := m.registry
	prog := m.prog
	beforeLen := len(hist)
	return func() tea.Msg {
		h := append([]openai.ChatCompletionMessage(nil), hist...)
		onEvent := func(ev Event) {
			if prog != nil {
				prog.Send(toolEventMsg{kind: ev.Kind, text: ev.Text, args: ev.Args, output: ev.Output, id: ev.ID})
			}
		}
		reply, usage, err := runTurn(ctx, cfg, client, apiKey, httpClient, registry, &h, onEvent)
		var thinking, answer string
		var tools []ToolRecord
		if len(h) > beforeLen {
			tools = collectTools(h[beforeLen:])
		}
		if err == nil {
			thinking, answer = splitThinking(reply.Content)
		}
		return turnDoneMsg{prompt: trimmed, thinking: thinking, answer: answer, tools: tools, usage: usage, err: err, history: h}
	}
}

func (m *tuiModel) handleCommand(text string) tea.Cmd {
	fields := strings.Fields(text)
	cmd := strings.ToLower(fields[0])
	switch cmd {
	case "/clear", "/new":
		m.history = seedHistory(m.cfg)
		if m.session != nil {
			m.session.Turns = nil
			m.session.TotalUsage = Usage{}
		}
		m.notice = "Cleared — fresh conversation."
		m.noticeErr = false
		m.show(viewChat)
		return nil
	case "/think":
		m.showThink = !m.showThink
		if m.showThink {
			m.notice = "Thinking blocks shown."
		} else {
			m.notice = "Thinking blocks hidden."
		}
		m.noticeErr = false
		m.refreshViewport()
		return nil
	case "/save":
		msg, isErr := m.exportTranscript()
		m.notice = msg
		m.noticeErr = isErr
		return nil
	case "/help", "/h", "/?":
		m.show(viewHelp)
		return nil
	case "/history":
		m.show(viewHistory)
		return nil
	case "/sessions", "/resume", "/continue":
		m.show(viewSessions)
		return nil
	case "/tools":
		m.show(viewTools)
		return nil
	case "/errors":
		m.show(viewErrors)
		return nil
	case "/model":
		if len(fields) == 1 {
			return m.openModelPicker("")
		}
		arg := fields[1]
		if knownGoModel(arg) {
			m.applyModelSwitch(matchGoModel(fallbackGoModels, arg)[0])
			return nil
		}
		return m.openModelPicker(arg)
	case "/effort", "/thinking":
		if len(fields) == 1 {
			m.openEffortPicker()
			return nil
		}
		m.applyThinkingSwitch(fields[1])
		return nil
	case "/compact", "/summarize":
		return m.compactCmd()
	case "/quit", "/exit", "/q":
		return tea.Quit
	default:
		m.notice = fmt.Sprintf("Unknown command %s — try /help.", fields[0])
		m.noticeErr = true
		return nil
	}
}

// compactCmd runs a manual compaction off the UI thread: prune older tool
// output locally, then summarize the head with the cheap [compaction] model.
// Send-side only — the session JSONL keeps every turn.
func (m *tuiModel) compactCmd() tea.Cmd {
	if m.busy {
		m.notice = "Working… press esc to cancel."
		m.noticeErr = false
		return nil
	}
	if len(m.history) < 3 {
		m.notice = "Nothing to compact — history already small."
		m.noticeErr = false
		return nil
	}
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	m.cancel = cancel
	m.busy = true
	m.toolStatus = "Compacting…"
	m.notice = ""
	m.refreshViewport()
	m.viewport.GotoBottom()

	cfg := m.cfg
	client := m.client
	apiKey := m.apiKey
	httpClient := m.httpClient
	h := append([]openai.ChatCompletionMessage(nil), m.history...)
	before := estimateTokens(h)
	return func() tea.Msg {
		pruned := pruneOldToolOutputs(h)
		summary, ok, err := compactHistory(ctx, cfg, client, apiKey, httpClient, &h)
		return compactDoneMsg{
			summary: summary, err: err, history: h,
			before: before, after: estimateTokens(h),
			pruned: pruned, compacted: ok, model: compactionModel(cfg),
		}
	}
}

// openModelPicker fetches the live OpenCode Go catalog off the UI thread.
// With a filter, an exact or unique-substring match switches directly;
// otherwise the filtered picker opens.
func (m *tuiModel) openModelPicker(filter string) tea.Cmd {
	m.notice = "Loading models…"
	m.noticeErr = false
	m.modelFilter = filter
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ids, err := fetchGoModels(ctx, client)
		return modelsLoadedMsg{ids: ids, err: err, filter: filter}
	}
}

// openEffortPicker lists the thinking levels available on the current model,
// plus the provider default.
func (m *tuiModel) openEffortPicker() {
	m.effortLevels = append([]string{""}, AvailableThinkingLevels(m.cfg.Model)...)
	m.show(viewEffort)
	for i, l := range m.effortLevels {
		if l == m.cfg.Thinking {
			m.cursor = i
		}
	}
	m.refreshViewport()
}

// applyModelSwitch moves the session to id, resets an incompatible thinking
// level, and persists both to config.toml so the choice sticks. Unknown
// models probe the provider in the background: a bogus effort 400s with the
// valid list, which is cached on disk for ctrl+t.
func (m *tuiModel) applyModelSwitch(id string) {
	prevThinking := m.cfg.Thinking
	m.cfg.Model = id
	resetNote := ""
	if !validThinkingLevel(id, m.cfg.Thinking) {
		m.cfg.Thinking = ""
		if prevThinking != "" {
			resetNote = fmt.Sprintf(" Thinking reset to default (%s is not available on %s).", prevThinking, id)
		}
	}
	msg := fmt.Sprintf("Switched to %s.%s", id, resetNote)
	if m.cfgPath != "" {
		if err := persistModelSelection(m.cfgPath, m.cfg.Model, m.cfg.Thinking); err != nil {
			m.notice = msg + " (config save failed: " + err.Error() + ")"
			m.noticeErr = true
		} else {
			m.notice = msg + " Saved to config."
			m.noticeErr = false
		}
	} else {
		m.notice = msg + " (session only)"
		m.noticeErr = false
	}
	m.show(viewChat)
	if _, ok := cachedThinkingLevels(id); !ok {
		if _, ok := lookupKnownLevels(normalizeModelID(id)); !ok && m.httpClient != nil && m.apiKey != "" {
			cfg, apiKey, hc := m.cfg, m.apiKey, m.httpClient
			base := m.ctx
			if base == nil {
				base = context.Background()
			}
			go func() {
				ctx, cancel := context.WithTimeout(base, 15*time.Second)
				defer cancel()
				_, _ = probeThinkingLevels(ctx, cfg, apiKey, hc)
			}()
		}
	}
}

// setThinkingLevel validates, persists and announces level, preserving the
// current view mode. It reports whether the level changed.
func (m *tuiModel) setThinkingLevel(level string) bool {
	level = strings.ToLower(strings.TrimSpace(level))
	if !validThinkingLevel(m.cfg.Model, level) {
		m.notice = fmt.Sprintf("Thinking %q is not available on %s (try: %s).", level, m.cfg.Model, strings.Join(AvailableThinkingLevels(m.cfg.Model), ", "))
		m.noticeErr = true
		return false
	}
	m.cfg.Thinking = level
	display := level
	if display == "" {
		display = "provider default"
	}
	msg := fmt.Sprintf("Thinking set to %s on %s.", display, m.cfg.Model)
	if m.cfgPath != "" {
		if err := persistModelSelection(m.cfgPath, m.cfg.Model, m.cfg.Thinking); err != nil {
			m.notice = msg + " (config save failed: " + err.Error() + ")"
			m.noticeErr = true
		} else {
			m.notice = msg + " Saved to config."
			m.noticeErr = false
		}
	} else {
		m.notice = msg + " (session only)"
		m.noticeErr = false
	}
	return true
}

// applyThinkingSwitch sets the reasoning level for the current model,
// or clears it back to the provider default. "default", "auto" and "off"
// all clear.
func (m *tuiModel) applyThinkingSwitch(level string) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "default" || level == "auto" || level == "off" || level == "-" {
		level = ""
	}
	if !m.setThinkingLevel(level) {
		return
	}
	m.show(viewChat)
}

// cycleThinking moves the thinking level forward low to high through
// AvailableThinkingLevels, wrapping back to the lowest. ctrl+t is the
// mnemonic (T-hinking); ctrl+] / ctrl+[ never arrive reliably in the
// terminal (ctrl+[ is esc), so a single wrapping key beats up/down.
func (m *tuiModel) cycleThinking() {
	levels := AvailableThinkingLevels(m.cfg.Model)
	if len(levels) == 0 {
		m.notice = "No adjustable thinking on " + m.cfg.Model + " (provider default only)."
		m.noticeErr = false
		m.refreshViewport()
		return
	}
	idx := -1
	for i, l := range levels {
		if l == m.cfg.Thinking {
			idx = i
			break
		}
	}
	if len(levels) == 1 && idx == 0 {
		m.notice = "Thinking only offers " + effortLabel(levels[0]) + " on " + m.cfg.Model + "."
		m.noticeErr = false
		m.refreshViewport()
		return
	}
	next := 0
	if idx >= 0 {
		next = (idx + 1) % len(levels)
	}
	if m.setThinkingLevel(levels[next]) {
		m.refreshViewport()
	}
}

func (m *tuiModel) exportTranscript() (string, bool) {
	if m.session == nil {
		return "No session to save.", true
	}
	path := fmt.Sprintf("valen-%s.md", m.session.ID)
	var b strings.Builder
	for _, t := range m.session.Turns {
		fmt.Fprintf(&b, "## You · %s\n\n%s\n\n", t.At.Format("15:04"), t.Prompt)
		if t.Thinking != "" {
			fmt.Fprintf(&b, "<details><summary>thinking</summary>\n\n%s\n\n</details>\n\n", t.Thinking)
		}
		for _, tr := range t.Tools {
			fmt.Fprintf(&b, "> tool `%s` `%s`\n>\n> %s\n\n", tr.Name, truncatePlain(tr.Args, 120), truncatePlain(tr.Output, 2000))
		}
		if t.Reply != "" {
			fmt.Fprintf(&b, "%s\n\n", t.Reply)
		}
		if t.Error != nil {
			fmt.Fprintf(&b, "> error: %s\n\n", t.Error.Title)
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "Save failed: " + err.Error(), true
	}
	return "Saved " + path, false
}

// Update drives the Bubble Tea loop. Single in-process runtime, no server.
func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		// Chrome around the transcript: header, status, footer, the input
		// border plus a line of slack (6) beside the configured box lines.
		vpH := msg.Height - 6 - inputHeight(m.cfg)
		if vpH < 5 {
			vpH = 5
		}
		vpW := msg.Width - 4
		if vpW < 40 {
			vpW = 40
		}
		m.viewport.Width = vpW
		m.viewport.Height = vpH
		m.input.SetWidth(vpW - 2)
		m.glam = nil
		m.refreshViewport()
		return m, nil

	case spinner.TickMsg:
		if m.busy {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
			// Keep the live transcript repainting at the ~10fps tick while
			// a turn runs: open tool threads animate in the scrollback and
			// the status spinner never goes stale between events. Committed
			// turns are cached, so this is just the small pending block.
			m.refreshViewport()
			if m.mode == viewChat {
				m.viewport.GotoBottom()
			}
		}
		return m, tea.Batch(cmds...)

	case toolEventMsg:
		switch msg.kind {
		case "tool_start":
			m.pendingTools = append(m.pendingTools, pendingTool{name: msg.text, args: msg.args, id: msg.id})
			if n := m.runningToolCount(); n > 1 {
				m.toolStatus = fmt.Sprintf("⏺ %s… · %d running", msg.text, n)
			} else {
				m.toolStatus = "⏺ " + msg.text + "…"
			}
		case "tool_result":
			m.finishPendingTool(msg.id, msg.text, msg.output)
			if n := m.runningToolCount(); n > 0 {
				m.toolStatus = fmt.Sprintf("✓ %s · %d still running", msg.text, n)
			} else {
				m.toolStatus = "✓ " + msg.text
			}
		case "text_delta":
			m.pendingText = appendStreamLine(m.pendingText, msg.text)
			m.flushStream()
			return m, nil
		case "thinking_delta":
			m.pendingThinking = appendStreamText(m.pendingThinking, msg.text)
			m.flushStream()
			return m, nil
		default:
			m.toolStatus = msg.text
		}
		// Tool events share the delta throttle: a parallel batch emits 2N
		// events at once and the committed turnDoneMsg always renders final
		// state, so collapsing a burst into one render loses nothing.
		m.flushStream()
		return m, nil

	case usageMsg:
		m.usageInflight = false
		if msg.err == nil {
			u := msg.usage
			m.goUsage = &u
		}
		return m, nil

	case usageTickMsg:
		return m, tea.Batch(m.fetchUsageCmd(), usageTick())

	case modelsLoadedMsg:
		if msg.filter != m.modelFilter {
			return m, nil // stale fetch; a newer picker request is in flight
		}
		ids := msg.ids
		if msg.err != nil || len(ids) == 0 {
			ids = append([]string(nil), fallbackGoModels...)
			m.notice = "Live model list unreachable — showing known models."
			m.noticeErr = false
		} else {
			m.notice = ""
		}
		if strings.TrimSpace(msg.filter) != "" {
			matched := matchGoModel(ids, msg.filter)
			switch len(matched) {
			case 0:
				m.notice = fmt.Sprintf("No models match %q.", msg.filter)
				m.noticeErr = true
				return m, nil
			case 1:
				m.applyModelSwitch(matched[0])
				return m, nil
			default:
				ids = matched
			}
		}
		sort.Strings(ids)
		m.modelChoices = m.modelChoices[:0]
		for _, id := range ids {
			m.modelChoices = append(m.modelChoices, modelChoice{id: id, current: id == m.cfg.Model, transport: modelTransport(id)})
		}
		m.show(viewModels)
		for i, c := range m.modelChoices {
			if c.current {
				m.cursor = i
			}
		}
		m.refreshViewport()
		return m, nil

	case questionMsg:
		picks := make([]map[int]bool, len(msg.qs))
		for i := range msg.qs {
			picks[i] = map[int]bool{}
		}
		m.ask = &askState{qs: msg.qs, reply: msg.reply, picks: picks, ownText: make([]string, len(msg.qs)), phase: "pick"}
		m.mode = viewAsk
		m.input.Blur()
		m.refreshViewport()
		return m, nil

	case turnDoneMsg:
		turn := Turn{
			At: msg.promptAt(), Prompt: msg.prompt, Thinking: msg.thinking, Reply: msg.answer, Tools: msg.tools,
			Usage: Usage{Prompt: msg.usage.PromptTokens, Completion: msg.usage.CompletionTokens, Total: msg.usage.TotalTokens},
			Model: m.cfg.Model, Effort: m.cfg.Thinking,
		}
		if msg.err != nil {
			fe := friendlyError(msg.err)
			turn.Error = &fe
			if isCancel(msg.err) {
				m.notice = "Cancelled — resend when ready."
				m.noticeErr = false
			} else {
				m.notice = fe.Title
				m.noticeErr = true
			}
		} else {
			m.notice = ""
		}
		m.history = msg.history
		m.busy = false
		m.pendingPrompt = ""
		m.toolStatus = ""
		m.pendingTools = nil
		m.pendingText = ""
		m.pendingThinking = ""
		m.cancel = nil
		if m.session != nil {
			if err := m.session.append(turn); err != nil {
				m.notice = "Session save failed: " + err.Error()
				m.noticeErr = true
			} else {
				m.session.ensureMeta(m.cfg.Model, m.cfg.Thinking)
			}
		}
		if m.done != nil {
			close(m.done)
			m.done = nil
		}
		m.input.Focus()
		m.refreshViewport()
		m.viewport.GotoBottom()
		return m, m.fetchUsageCmd()

	case compactDoneMsg:
		m.busy = false
		m.toolStatus = ""
		m.cancel = nil
		if msg.err != nil {
			if isCancel(msg.err) {
				m.notice = "Compaction cancelled."
				m.noticeErr = false
			} else {
				m.notice = "Compaction failed: " + friendlyError(msg.err).Title
				m.noticeErr = true
			}
		} else if msg.compacted {
			m.history = msg.history
			m.notice = fmt.Sprintf("Compacted %d→%d tokens (summary by %s). Disk log untouched.", msg.before, msg.after, msg.model)
			m.noticeErr = false
		} else if msg.pruned > 0 {
			m.history = msg.history
			m.notice = fmt.Sprintf("Pruned older tool output %d→%d tokens; nothing left to summarize.", msg.before, msg.after)
			m.noticeErr = false
		} else {
			m.notice = "Nothing to compact — history already small."
			m.noticeErr = false
		}
		m.input.Focus()
		m.refreshViewport()
		m.viewport.GotoBottom()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Forward anything else (e.g. textarea blink) to focused widgets.
	if m.mode == viewChat {
		var icmd tea.Cmd
		m.input, icmd = m.input.Update(msg)
		if icmd != nil {
			cmds = append(cmds, icmd)
		}
	} else {
		var vcmd tea.Cmd
		m.viewport, vcmd = m.viewport.Update(msg)
		if vcmd != nil {
			cmds = append(cmds, vcmd)
		}
	}
	return m, tea.Batch(cmds...)
}

func (msg turnDoneMsg) promptAt() (t time.Time) {
	return time.Now()
}

func isCancel(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "cancel") || strings.Contains(s, "context canceled")
}

// insertInputNewline adds a line break without submitting, shared by the
// modifier-newline keys (shift+enter / alt+enter / ctrl+j).
func (m *tuiModel) insertInputNewline() {
	m.input.InsertRune('\n')
}

func (m *tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := msg.String()

	// Global: cancel / quit.
	switch s {
	case "ctrl+c":
		if m.ask != nil {
			m.dismissAsk(errors.New("user dismissed the questions"))
			return m, nil
		}
		if m.busy {
			m.cancelRun()
			m.notice = "Cancelling…"
			m.noticeErr = false
			return m, nil
		}
		return m, tea.Quit
	case "ctrl+d":
		if m.ask != nil {
			m.dismissAsk(errors.New("user dismissed the questions"))
			return m, nil
		}
		if m.busy {
			m.cancelRun()
			return m, nil
		}
		return m, tea.Quit
	case "esc":
		if m.ask != nil {
			return m.handleAskKey(s)
		}
		if m.busy {
			m.cancelRun()
			return m, nil
		}
		if m.mode != viewChat {
			m.show(viewChat)
			return m, nil
		}
		if m.input.Value() != "" {
			m.input.SetValue("")
			return m, nil
		}
		return m, nil
	}

	// Ctrl shortcuts work everywhere; /help lists them.
	if m.ask != nil {
		return m.handleAskKey(s)
	}
	switch s {
	case "ctrl+g":
		m.show(viewHelp)
		return m, nil
	case "ctrl+r":
		m.show(viewHistory)
		return m, nil
	case "ctrl+l":
		m.show(viewSessions)
		return m, nil
	case "ctrl+u":
		m.show(viewTools)
		return m, nil
	case "ctrl+x":
		m.show(viewErrors)
		return m, nil
	case "ctrl+y":
		m.showThink = !m.showThink
		m.refreshViewport()
		return m, nil
	case "ctrl+o":
		m.showMeta = !m.showMeta
		return m, nil
	case "ctrl+p":
		return m, m.openModelPicker("")
	case "ctrl+s":
		text, isErr := m.exportTranscript()
		m.notice = text
		m.noticeErr = isErr
		return m, nil
	case "ctrl+q":
		return m, tea.Quit
	case "ctrl+t":
		// ctrl+t shadows textarea transpose (rarely used); it cycles the
		// reasoning level low→high and wraps to low.
		m.cycleThinking()
		return m, nil
	}

	if m.ask != nil {
		return m.handleAskKey(s)
	}
	if m.mode != viewChat {
		return m.handleOverlayKey(s)
	}

	switch s {
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		if m.busy {
			m.notice = "Working… press esc to cancel."
			m.noticeErr = false
			return m, nil
		}
		m.input.SetValue("")
		cmd := m.submit(text)
		if cmd == nil {
			return m, nil
		}
		// Keep the spinner ticking while the turn runs.
		return m, tea.Batch(cmd, m.spinner.Tick)
	case "shift+enter", "alt+enter", "ctrl+j":
		// shift+enter is indistinguishable from enter on legacy terminals
		// (both arrive as "enter"), so alt+enter and ctrl+j are the working
		// modifier-newlines today; shift+enter stays for Kitty-capable ones.
		m.insertInputNewline()
		return m, nil
	case "pgup", "pgdown", "ctrl+d":
		var vcmd tea.Cmd
		m.viewport, vcmd = m.viewport.Update(msg)
		return m, vcmd
	case "up", "down":
		// Let the textarea move first; viewport scrolls with pgup/pgdn.
		var icmd tea.Cmd
		m.input, icmd = m.input.Update(msg)
		return m, icmd
	default:
		var icmd tea.Cmd
		m.input, icmd = m.input.Update(msg)
		return m, icmd
	}
}

func (m *tuiModel) handleOverlayKey(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "q":
		m.show(viewChat)
		return m, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.refreshViewport()
		}
		return m, nil
	case "down", "j":
		m.cursor++
		m.refreshViewport()
		return m, nil
	case "enter", " ":
		m.openSelected()
		return m, nil
	case "pgup", "pgdown", "ctrl+d":
		// Paging scrolls the viewport; cursor moves with j/k.
		if s == "pgup" || s == "ctrl+u" {
			m.viewport.PageUp()
		} else {
			m.viewport.PageDown()
		}
		return m, nil
	default:
		// j/k handled above; ignore typing in overlays.
		return m, nil
	}
}

func (m *tuiModel) openSelected() {
	switch m.mode {
	case viewModels:
		if m.cursor < 0 || m.cursor >= len(m.modelChoices) {
			return
		}
		m.applyModelSwitch(m.modelChoices[m.cursor].id)
	case viewEffort:
		if m.cursor < 0 || m.cursor >= len(m.effortLevels) {
			return
		}
		m.applyThinkingSwitch(m.effortLevels[m.cursor])
	case viewHistory:
		items := m.historyItems()
		if m.cursor < 0 || m.cursor >= len(items) {
			return
		}
		it := items[m.cursor]
		if it.isFile {
			m.detailTitle = "Session " + filepath.Base(it.path)
			m.detailBody = renderSessionFileText(m, it.path)
		} else if it.turnIdx >= 0 && it.turnIdx < len(m.session.Turns) {
			t := m.session.Turns[it.turnIdx]
			m.detailTitle = fmt.Sprintf("Turn %d · %s", it.turnIdx+1, t.At.Format("15:04:05"))
			m.detailBody = m.renderTurnFull(&t)
		}
		m.show(viewDetail)
	case viewTools:
		items := m.toolItems()
		if m.cursor < 0 || m.cursor >= len(items) {
			return
		}
		r := items[m.cursor].record
		m.detailTitle = "Tool " + r.Name
		m.detailBody = fmt.Sprintf("args:\n%s\n\noutput:\n%s", r.Args, r.Output)
		m.show(viewDetail)
	case viewSessions:
		items := m.sessionItems()
		if m.cursor < 0 || m.cursor >= len(items) {
			return
		}
		m.resumeSessionPath(items[m.cursor].path)
	case viewErrors:
		idx := m.errorTurnIdx()
		if m.cursor < 0 || m.cursor >= len(idx) {
			return
		}
		t := m.session.Turns[idx[m.cursor]]
		m.detailTitle = "Error · " + t.At.Format("15:04:05")
		if t.Error != nil {
			m.detailBody = t.Error.Title + "\n\n" + t.Error.Details
		}
		m.show(viewDetail)
	}
}

// View renders header / viewport / status / input / footer.
func (m *tuiModel) View() string {
	if m.width == 0 && !m.ready {
		// Pre-window fallback used by tests.
		return m.renderViewportContent()
	}
	header := m.headerView()
	status := m.statusView()
	input := m.inputView()
	footer := m.footerView()
	return lipgloss.JoinVertical(lipgloss.Left, header, m.viewport.View(), status, input, footer)
}

func (m *tuiModel) headerView() string {
	title := headerTitleStyle.Render("✳ valen")
	model := m.cfg.Model
	if m.cfg.Thinking != "" {
		model += " · " + m.cfg.Thinking
	}
	meta := headerDimStyle.Render(fmt.Sprintf("  ·  %s  ·  %s  ·  %d tools", model, shortSession(m.session), len(m.registry.Names())))
	mode := ""
	switch m.mode {
	case viewHistory:
		mode = headerDimStyle.Render("  ·  history")
	case viewSessions:
		mode = headerDimStyle.Render("  ·  sessions")
	case viewTools:
		mode = headerDimStyle.Render("  ·  tools")
	case viewErrors:
		mode = headerDimStyle.Render("  ·  errors")
	case viewHelp:
		mode = headerDimStyle.Render("  ·  help")
	case viewModels:
		mode = headerDimStyle.Render("  ·  models")
	case viewEffort:
		mode = headerDimStyle.Render("  ·  thinking")
	case viewDetail:
		mode = headerDimStyle.Render("  ·  " + m.detailTitle)
	case viewAsk:
		mode = headerDimStyle.Render("  ·  question")
	}
	line := title + meta + mode
	if m.width > 0 {
		return lipgloss.NewStyle().Width(m.width).Render(line)
	}
	return line
}

func shortSession(s *Session) string {
	if s == nil {
		return "no-session"
	}
	id := s.ID
	if len(id) > 14 {
		return id[:14] + "…"
	}
	return id
}

func (m *tuiModel) statusView() string {
	var left string
	if m.busy {
		elapsed := time.Since(m.pendingSince).Round(time.Second)
		tool := m.toolStatus
		if tool == "" {
			tool = "thinking…"
		}
		left = statusBusyStyle.Render(fmt.Sprintf("%s working  %s  %s", m.spinner.View(), tool, elapsed))
	} else {
		left = statusIdleStyle.Render("● ready")
	}
	var mid string
	if m.notice != "" {
		if m.noticeErr {
			mid = "  " + errNoticeStyle.Render(m.notice)
		} else {
			mid = "  " + noticeStyle.Render(m.notice)
		}
	} else if m.session != nil {
		mid = "  " + metaStyle.Render(fmt.Sprintf("%d turns · ↑%d ↓%d", len(m.session.Turns), m.session.TotalUsage.Prompt, m.session.TotalUsage.Completion))
	}
	right := ""
	if m.showMeta {
		right = metaStyle.Render(fmt.Sprintf("  ·  %s %s", m.cfg.Model, m.cfg.BaseURL))
	}
	return lipgloss.NewStyle().Padding(0, 2).Render(left + mid + right)
}

func (m *tuiModel) inputView() string {
	border := mkBorder
	if m.mode == viewChat && !m.busy {
		border = mkGreen
	} else if m.busy {
		border = mkOrange
	}
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(0, 1)
	if m.width > 0 {
		style = style.Width(m.width - 2)
	}
	return style.Render(m.input.View())
}

func (m *tuiModel) footerView() string {
	hint := "/help for keys and commands"
	if m.goUsage == nil {
		return footerStyle.Padding(0, 2).Render(hint)
	}
	text := m.goUsage.footerText(time.Now())
	avail := 0
	if m.width > 0 {
		avail = m.width - 4
		if lipgloss.Width(hint)+lipgloss.Width(text)+5 > avail {
			text = fmt.Sprintf("5h %d%%", m.goUsage.Rolling.Percent)
		}
		if lipgloss.Width(hint)+lipgloss.Width(text)+5 > avail {
			return footerStyle.Padding(0, 2).Render(hint)
		}
	}
	uStyle := footerStyle
	switch m.goUsage.usageSev() {
	case "warn":
		uStyle = noticeStyle
	case "bad":
		uStyle = errNoticeStyle
	}
	line := hint
	if avail > 0 {
		line += strings.Repeat(" ", avail-lipgloss.Width(hint)-lipgloss.Width(text)) + uStyle.Render(text)
	} else {
		line += "  " + uStyle.Render(text)
	}
	return footerStyle.Padding(0, 2).Render(line)
}

func (m *tuiModel) refreshViewport() {
	m.viewport.SetContent(m.renderViewportContent())
}

func (m *tuiModel) renderViewportContent() string {
	switch m.mode {
	case viewHistory:
		return m.renderHistoryList()
	case viewSessions:
		return m.renderSessionsList()
	case viewTools:
		return m.renderToolsList()
	case viewErrors:
		return m.renderErrorsList()
	case viewHelp:
		return m.renderHelp()
	case viewModels:
		return m.renderModelsList()
	case viewEffort:
		return m.renderEffortList()
	case viewDetail:
		return m.renderDetail()
	case viewAsk:
		return m.renderAsk()
	default:
		return m.renderChat()
	}
}

func (m *tuiModel) renderChat() string {
	w := m.viewWidth()
	if m.session == nil || len(m.session.Turns) == 0 {
		m.chatCache, m.chatCacheTurns = "", 0
		if m.pendingPrompt == "" {
			return emptyStyle.Render("No turns yet.")
		}
	}
	committed := m.committedTurns(w)
	if m.pendingPrompt == "" {
		return committed
	}
	if committed == "" {
		return m.renderPending(w)
	}
	// Generous vertical padding between turns reads like a conversation.
	return committed + "\n\n\n" + m.renderPending(w)
}

// committedTurns renders the immutable committed turns, extending the cached
// prefix append-only. Delta flushes during streaming reuse the prefix, so
// per-delta cost is the live pending block, not history.
func (m *tuiModel) committedTurns(w int) string {
	if m.session == nil {
		return ""
	}
	n := len(m.session.Turns)
	if m.chatCacheW != w || m.chatCacheThink != m.showThink || m.chatCacheModel != m.cfg.Model || m.chatCacheTurns > n {
		m.chatCache, m.chatCacheTurns = "", 0
	}
	for m.chatCacheTurns < n {
		m.chatCache += m.renderTurn(&m.session.Turns[m.chatCacheTurns], w) + "\n\n\n"
		m.chatCacheTurns++
	}
	m.chatCacheW, m.chatCacheThink, m.chatCacheModel = w, m.showThink, m.cfg.Model
	return strings.TrimSuffix(m.chatCache, "\n\n\n")
}

func (m *tuiModel) renderTurn(t *Turn, w int) string {
	var parts []string
	// bodyW is the style width; textW leaves room for the left border plus
	// padding so force-broken words still fit inside the chrome.
	bodyW := min(w-4, 120)
	textW := bodyW - 2

	head := userHeaderStyle.Render("You") + "  " + metaStyle.Render(t.At.Format("15:04"))
	body := userBodyStyle.Width(bodyW).Render(breakLongWords(t.Prompt, textW))
	parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, head, "", body))

	if t.Thinking != "" {
		if m.showThink {
			thHead := thinkHeaderStyle.Render("✦ thinking")
			thBody := thinkBodyStyle.Width(bodyW).Render(breakLongWords(t.Thinking, textW))
			parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, thHead, thBody))
		} else {
			parts = append(parts, thinkHeaderStyle.Render(fmt.Sprintf("✦ thinking (%d chars) — ctrl+y to expand", len(t.Thinking))))
		}
	}

	for _, tr := range t.Tools {
		parts = append(parts, m.renderToolLine(tr))
	}

	if t.Reply != "" {
		aHead := asstHeadStyle.Render("✳ " + m.cfg.Model)
		aBody := asstBodyStyle.Width(bodyW).Render(breakLongWords(m.renderMarkdown(t.Reply), bodyW-1))
		parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, aHead, aBody))
	}

	if t.Error != nil {
		parts = append(parts, errorTitleSty.Width(min(w-4, 120)).Render(breakLongWords("✗ "+t.Error.Title, min(w-6, 118)))+"\n"+metaStyle.Render("  ctrl+x for details"))
	}

	if t.Usage.Total > 0 {
		parts = append(parts, metaStyle.Render(fmt.Sprintf("  ↑ %d · ↓ %d", t.Usage.Prompt, t.Usage.Completion)))
	}
	return strings.Join(parts, "\n\n")
}

// renderTurnFull always expands thinking and tool output for recall views.
// Content is wrapped to the transcript width so detail views never run
// off-screen.
func (m *tuiModel) renderTurnFull(t *Turn) string {
	var b strings.Builder
	b.WriteString(m.wrapPlain("You · "+t.At.Format("15:04:05")+"\n"+t.Prompt) + "\n\n")
	if t.Thinking != "" {
		b.WriteString(m.wrapPlain("✦ thinking\n"+t.Thinking) + "\n\n")
	}
	for _, tr := range t.Tools {
		b.WriteString(m.wrapPlain(fmt.Sprintf("⏺ %s\nargs: %s\n%s", tr.Name, tr.Args, tr.Output)) + "\n\n")
	}
	if t.Error != nil {
		b.WriteString(m.wrapPlain("✗ "+t.Error.Title+"\n"+t.Error.Details) + "\n\n")
	}
	if t.Reply != "" {
		b.WriteString(m.wrapPlain(t.Reply) + "\n")
	}
	return b.String()
}

func (m *tuiModel) renderToolLine(tr ToolRecord) string {
	cw := m.contentWidth()
	maxText := cw - 4
	if maxText < 20 {
		maxText = 20
	}
	args := truncateRunes(strings.TrimSpace(tr.Args), maxText)
	label := fmt.Sprintf("⏺ %s  %s", tr.Name, args)
	// Tool styles carry PaddingLeft(2), so break two short of cw.
	if tr.Err != "" || strings.HasPrefix(strings.TrimSpace(tr.Output), "error:") {
		return toolErrStyle.Width(cw).Render(breakLongWords("✗ "+label, cw-2))
	}
	out := strings.TrimSpace(tr.Output)
	if out != "" {
		first := strings.SplitN(out, "\n", 2)[0]
		first = truncateRunes(first, maxText)
		return toolLineStyle.Width(cw).Render(breakLongWords(label, cw-2)) + "\n" + toolDimStyle.Width(cw).Render(breakLongWords("→ "+first, cw-2))
	}
	return toolLineStyle.Width(cw).Render(breakLongWords(label, cw-2))
}

// renderPendingTool renders one live tool row. In-flight tools (no result
// yet) carry the animated spinner frame so concurrent threads are visible
// in the scrollback; finished tools render the usual line plus preview.
func (m *tuiModel) renderPendingTool(t pendingTool) string {
	if t.done {
		return m.renderToolLine(ToolRecord{Name: t.name, Args: t.args, Output: t.output})
	}
	cw := m.contentWidth()
	maxText := cw - 4
	if maxText < 20 {
		maxText = 20
	}
	args := truncateRunes(strings.TrimSpace(t.args), maxText)
	label := fmt.Sprintf("%s  %s  %s · running", m.spinner.View(), t.name, args)
	// Tool styles carry PaddingLeft(2), so break two short of cw.
	return toolLineStyle.Width(cw).Render(breakLongWords(label, cw-2))
}

func (m *tuiModel) renderPending(w int) string {
	bodyW := min(w-4, 120)
	textW := bodyW - 2
	head := userHeaderStyle.Render("You") + "  " + metaStyle.Render("now")
	body := userBodyStyle.Width(bodyW).Render(breakLongWords(m.pendingPrompt, textW))
	parts := []string{head, "", body, ""}

	if m.pendingThinking != "" && m.showThink {
		parts = append(parts, thinkHeaderStyle.Render("✦ thinking"))
		parts = append(parts, thinkBodyStyle.Width(bodyW).Render(breakLongWords(m.pendingThinking, textW)))
		parts = append(parts, "")
	}
	for _, t := range m.pendingTools {
		parts = append(parts, m.renderPendingTool(t))
	}
	if len(m.pendingTools) > 0 {
		parts = append(parts, "")
	}
	if m.pendingText != "" {
		parts = append(parts, asstHeadStyle.Render("✳ "+m.cfg.Model))
		parts = append(parts, asstBodyStyle.Width(bodyW).Render(breakLongWords(streamTail(m.pendingText), bodyW-1)))
		parts = append(parts, "")
	}

	tool := m.toolStatus
	if tool == "" {
		tool = "thinking…"
	}
	status := statusBusyStyle.Render(tool) + "  " + metaStyle.Render("esc cancels")
	parts = append(parts, status)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// appendStreamText accumulates live deltas, capped so a very long stream
// stays cheap. The committed history message always keeps the full text.
func appendStreamText(s, delta string) string {
	const maxChars = 32768
	if len(s) >= maxChars || len(s)+len(delta) > maxChars {
		return s
	}
	return s + delta
}

// appendStreamLine accumulates live text deltas so each progress update lands
// on its own line instead of appending to the end of the same line. Chunks
// that already carry line breaks keep them; bare appends are separated with
// a newline. The committed history message still keeps the exact full text;
// this only affects the live transcript render.
func appendStreamLine(s, delta string) string {
	const maxChars = 32768
	if strings.TrimSpace(delta) == "" {
		return s
	}
	if len(s) >= maxChars {
		return s
	}
	if s == "" {
		if len(delta) > maxChars {
			return delta[:maxChars]
		}
		return delta
	}
	sep := "\n"
	if strings.HasSuffix(s, "\n") || strings.HasPrefix(delta, "\n") {
		sep = ""
	}
	if len(s)+len(sep)+len(delta) > maxChars {
		return s
	}
	return s + sep + delta
}

// streamTail keeps live transcript renders cheap: only the tail of a long
// streamed reply is shown while it arrives; the full text lands on commit.
// It cuts on a line boundary so the live view stays a list of whole lines.
func streamTail(s string) string {
	const maxRunes = 1500
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	tail := r[len(r)-maxRunes:]
	if i := indexRune(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return "…\n" + string(tail)
}

func indexRune(r []rune, c rune) int {
	for i, v := range r {
		if v == c {
			return i
		}
	}
	return -1
}

func (m *tuiModel) historyItems() []historyItem {
	var items []historyItem
	if m.session != nil {
		for i := range m.session.Turns {
			t := m.session.Turns[i]
			items = append(items, historyItem{turnIdx: i, label: truncatePlain(t.Prompt, 64), when: t.At, tokens: t.Usage.Total})
		}
		paths, _ := listSessions()
		for _, p := range paths {
			if p == m.session.Path {
				continue
			}
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			items = append(items, historyItem{isFile: true, path: p, label: "(saved) " + filepath.Base(p), when: info.ModTime()})
		}
	}
	return items
}

func (m *tuiModel) renderHistoryList() string {
	items := m.historyItems()
	if len(items) == 0 {
		return emptyStyle.Render("No turns yet — your conversation will appear here.")
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(items) {
		m.cursor = len(items) - 1
	}
	var b strings.Builder
	b.WriteString(metaStyle.Render("History — enter opens · esc back") + "\n\n")
	cw := m.contentWidth()
	for i, it := range items {
		line := fmt.Sprintf("%2d  %s  %s", i+1, it.when.Format("15:04:05"), it.label)
		if it.tokens > 0 {
			line += fmt.Sprintf("  (%d tok)", it.tokens)
		}
		line = truncateRunes(line, cw)
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}

func (m *tuiModel) toolItems() []toolItem {
	var items []toolItem
	if m.session == nil {
		return items
	}
	for i := range m.session.Turns {
		for _, tr := range m.session.Turns[i].Tools {
			items = append(items, toolItem{turnIdx: i, record: tr})
		}
	}
	return items
}

func (m *tuiModel) renderToolsList() string {
	items := m.toolItems()
	if len(items) == 0 {
		return emptyStyle.Render("No tool calls yet.")
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(items) {
		m.cursor = len(items) - 1
	}
	var b strings.Builder
	b.WriteString(metaStyle.Render("Tools — enter opens full output · esc back") + "\n\n")
	cw := m.contentWidth()
	for i, it := range items {
		status := "ok"
		if strings.HasPrefix(strings.TrimSpace(it.record.Output), "error:") {
			status = "error"
		}
		line := fmt.Sprintf("%2d  %s  %s  [%s]", i+1, it.record.Name, truncateRunes(it.record.Args, 48), status)
		line = truncateRunes(line, cw)
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}

func (m *tuiModel) sessionItems() []sessionItem {
	paths, _ := listSessions()
	var items []sessionItem
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		it := sessionItem{path: p, when: info.ModTime()}
		if meta, err := loadSessionMeta(metaPath(p)); err == nil {
			it.title, it.model, it.effort = meta.Title, meta.Model, meta.Effort
		}
		if turns, err := loadSession(p); err == nil && len(turns) > 0 {
			it.turns = len(turns)
			if it.title == "" {
				it.title = sessionTitle(turns[0].Prompt)
			}
			// Backfill titles on first listing so old JSONLs show names.
			if it.title != "" {
				if meta, err := loadSessionMeta(metaPath(p)); err != nil || meta.Title == "" {
					model, effort := it.model, it.effort
					if model == "" {
						model = turns[0].Model
					}
					if effort == "" {
						effort = turns[0].Effort
					}
					_ = saveSessionMeta(metaPath(p), SessionMeta{Title: it.title, Model: model, Effort: effort})
					it.model, it.effort = model, effort
				}
			}
		}
		if it.title == "" {
			it.title = strings.TrimSuffix(filepath.Base(p), ".jsonl")
		}
		items = append(items, it)
	}
	return items
}

// resumeSessionPath loads path into the current TUI state: turns become the
// transcript and history is rebuilt so the resumed conversation continues.
func (m *tuiModel) resumeSessionPath(path string) {
	turns, err := loadSession(path)
	if err != nil {
		m.notice = "Could not load session: " + err.Error()
		m.noticeErr = true
		return
	}
	m.session.Path = path
	m.session.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	m.session.Turns = turns
	m.session.TotalUsage = Usage{}
	for _, t := range turns {
		m.session.TotalUsage.Prompt += t.Usage.Prompt
		m.session.TotalUsage.Completion += t.Usage.Completion
		m.session.TotalUsage.Total += t.Usage.Total
	}
	m.history = seedHistory(m.cfg)
	for _, t := range turns {
		m.history = append(m.history,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: t.Prompt},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: t.Reply})
	}
	if meta, err := loadSessionMeta(metaPath(path)); err == nil {
		m.session.Title, m.session.Model, m.session.Effort = meta.Title, meta.Model, meta.Effort
	}
	m.notice = "Resumed " + m.session.ID + " (" + itoa(len(turns)) + " turns)."
	m.noticeErr = false
	m.show(viewChat)
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func (m *tuiModel) renderSessionsList() string {
	items := m.sessionItems()
	if len(items) == 0 {
		return emptyStyle.Render("No saved sessions yet.")
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(items) {
		m.cursor = len(items) - 1
	}
	var b strings.Builder
	b.WriteString(metaStyle.Render("Sessions — enter resumes · esc back") + "\n\n")
	cw := m.contentWidth()
	for i, it := range items {
		tag := it.model
		if it.effort != "" {
			tag += " · " + it.effort
		}
		line := fmt.Sprintf("%2d  %s  %s", i+1, it.when.Format("01-02 15:04"), it.title)
		if tag != "" {
			line += "  [" + tag + "]"
		}
		if it.turns > 0 {
			line += fmt.Sprintf("  (%d)", it.turns)
		}
		cur := ""
		if m.session != nil && it.path == m.session.Path {
			cur = "  ● current"
		}
		line = truncateRunes(line+cur, cw)
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}

func (m *tuiModel) errorTurnIdx() []int {
	var idx []int
	if m.session == nil {
		return idx
	}
	for i := range m.session.Turns {
		if m.session.Turns[i].Error != nil {
			idx = append(idx, i)
		}
	}
	return idx
}

func (m *tuiModel) renderErrorsList() string {
	idx := m.errorTurnIdx()
	if len(idx) == 0 {
		return emptyStyle.Render("No errors — friendly messages will land here, never kill the loop.")
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(idx) {
		m.cursor = len(idx) - 1
	}
	var b strings.Builder
	b.WriteString(metaStyle.Render("Errors — enter opens details · esc back") + "\n\n")
	cw := m.contentWidth()
	for i, ti := range idx {
		t := m.session.Turns[ti]
		line := truncateRunes(fmt.Sprintf("%2d  %s  %s", i+1, t.At.Format("15:04:05"), t.Error.Title), cw)
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}

func (m *tuiModel) renderModelsList() string {
	if len(m.modelChoices) == 0 {
		return emptyStyle.Render("No models to show.")
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.modelChoices) {
		m.cursor = len(m.modelChoices) - 1
	}
	var b strings.Builder
	title := "Models — enter switches · esc back"
	if strings.TrimSpace(m.modelFilter) != "" {
		title = fmt.Sprintf("Models matching %q — enter switches · esc back", m.modelFilter)
	}
	b.WriteString(metaStyle.Render(title) + "\n\n")
	cw := m.contentWidth()
	for i, c := range m.modelChoices {
		line := c.id
		if c.current {
			line += "  ● current"
		}
		if c.transport != "chat" {
			line += fmt.Sprintf("  [/%s]", c.transport)
		}
		line = truncateRunes(line, cw-4)
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(fmt.Sprintf("%2d  %s", i+1, line)) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %2d  %s\n", i+1, line))
		}
	}
	b.WriteString("\n" + metaStyle.Render("Transports are marked: /chat, /responses and /messages all converse."))
	return b.String()
}

func effortLabel(level string) string {
	if level == "" {
		return "default (provider)"
	}
	return level
}

func (m *tuiModel) renderEffortList() string {
	if len(m.effortLevels) == 0 {
		return emptyStyle.Render("No thinking levels for this model.")
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.effortLevels) {
		m.cursor = len(m.effortLevels) - 1
	}
	var b strings.Builder
	b.WriteString(metaStyle.Render(fmt.Sprintf("Thinking for %s — enter sets · esc back", m.cfg.Model)) + "\n\n")
	cw := m.contentWidth()
	for i, l := range m.effortLevels {
		line := truncateRunes(effortLabel(l), cw-4)
		if l == m.cfg.Thinking {
			line += "  ● current"
		}
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(fmt.Sprintf("%2d  %s", i+1, line)) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %2d  %s\n", i+1, line))
		}
	}
	return b.String()
}

func (m *tuiModel) renderHelp() string {
	return emptyStyle.Width(m.contentWidth()).Render(`Conversation first: your message shows instantly, the reply streams into the same thread.

Keys
  enter send · shift+enter/alt+enter/ctrl+j newline · esc cancel/back · pgup/pgdn scroll
  ctrl+g help · ctrl+r history · ctrl+l sessions · ctrl+u tools · ctrl+x errors · ctrl+y thinking display
  ctrl+o details · ctrl+p model · ctrl+t thinking level (cycles low→high, wraps) · ctrl+s save · ctrl+q quit · ctrl+c cancel/quit
  in lists: ↑↓ move · enter open · esc back

Commands
  /help /history /sessions (/resume) /tools /errors /think /save /clear /quit
  /compact (/summarize) — prune + summarize older history with the cheap model; disk log untouched
  /model [filter] — list OpenCode Go models and switch (ctrl+p does the same)
  /effort [level] — thinking level for the current model (/thinking is an alias)

When the model calls AskUserQuestion: ↑↓/jk move · enter select · space toggles multi-select · type your own when needed · enter confirms.

Go plan usage (5h rolling / 7d / 30d) shows in the footer, refreshed each
minute and after every turn: dim normally, yellow past 80%, pink at the cap.

Models persist to config.toml, so the last switch sticks across runs.
Thinking levels depend on the model (see /effort picker): muse-spark takes
minimal/low/medium/high/xhigh, gpt/grok take low…max, glm/deepseek take
low/high/max or high/max, kimi-k3 is max-only, toggle models take none/high.
Unset thinking means the provider default. ctrl+t cycles the list low→high
and wraps to low; unknown models learn valid levels from the provider 400
and cache them on disk.

Thinking is shown by default (ctrl+y hides). Tools render inline; ctrl+u shows full output.

Chatbox chrome lives in config.toml [input]: blink=false parks the block
cursor steady (no blink), height=N sets the box lines (default 3, blinking on).`) + "\n"
}

func (m *tuiModel) renderDetail() string {
	return m.wrapPlain("‹ "+m.detailTitle+" · esc back ›") + "\n\n" + m.wrapPlain(m.detailBody) + "\n"
}

func renderSessionFileText(m *tuiModel, path string) string {
	turns, err := loadSession(path)
	if err != nil {
		return "Could not load: " + err.Error()
	}
	var b strings.Builder
	for i := range turns {
		b.WriteString(m.renderTurnFull(&turns[i]))
		b.WriteString("\n---\n\n")
	}
	return b.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func runTUI(ctx context.Context, cfg Config, cfgPath string, client *openai.Client, registry *Registry, apiKey string) error {
	session, err := newSession(cfg)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	m := newTUIModel(cfg, client, registry, session)
	m.ctx = ctx
	m.cfgPath = cfgPath
	m.apiKey = apiKey
	m.httpClient = newHTTPClient(cfg)
	askPrompter = m.askPrompt
	p := tea.NewProgram(m, tea.WithAltScreen())
	m.prog = p
	_, err = p.Run()
	return err
}
