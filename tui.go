package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/sashabaranov/go-openai"
)

// Monokai dark palette.
var (
	mkBg        = lipgloss.Color("#272822")
	mkFg        = lipgloss.Color("#F8F8F2")
	mkComment   = lipgloss.Color("#75715E")
	mkPink      = lipgloss.Color("#F92672")
	mkGreen     = lipgloss.Color("#A6E22E")
	mkGreenDark = lipgloss.Color("#4F6F0F")
	mkYellow    = lipgloss.Color("#E6DB74")
	mkOrange    = lipgloss.Color("#FD971F")
	mkPurple    = lipgloss.Color("#AE81FF")
	mkCyan      = lipgloss.Color("#66D9EF")
	mkBorder    = lipgloss.Color("#49483E")
)

var (
	headerTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(mkPink)
	headerDimStyle   = lipgloss.NewStyle().Foreground(mkComment)
	userHeaderStyle  = lipgloss.NewStyle().Bold(true).Foreground(mkOrange)
	userBodyStyle    = lipgloss.NewStyle().Foreground(mkFg).
				BorderStyle(lipgloss.ThickBorder()).BorderLeft(true).BorderForeground(mkOrange).
				PaddingLeft(1)
	// Plan-mode prompts wear the chatbox's cyan: header gains "(PLAN)" so a
	// scrollback of mixed turns reads which mode each message was sent in.
	userHeaderPlanStyle = lipgloss.NewStyle().Bold(true).Foreground(mkCyan)
	userBodyPlanStyle   = lipgloss.NewStyle().Foreground(mkFg).
				BorderStyle(lipgloss.ThickBorder()).BorderLeft(true).BorderForeground(mkCyan).
				PaddingLeft(1)
	thinkHeaderStyle = lipgloss.NewStyle().Italic(true).Foreground(mkComment)
	thinkBodyStyle   = lipgloss.NewStyle().Italic(true).Foreground(mkComment).
				BorderStyle(lipgloss.NormalBorder()).BorderLeft(true).BorderForeground(mkBorder).
				PaddingLeft(1)
	toolLineStyle = lipgloss.NewStyle().Foreground(mkCyan).PaddingLeft(2)
	toolRunStyle  = lipgloss.NewStyle().Foreground(mkYellow).PaddingLeft(2)
	toolErrStyle  = lipgloss.NewStyle().Foreground(mkPink).PaddingLeft(2).Bold(true)
	toolDimStyle  = lipgloss.NewStyle().Foreground(mkComment).PaddingLeft(2)
	asstHeadStyle = lipgloss.NewStyle().Bold(true).Foreground(mkGreen)
	asstBodyStyle = lipgloss.NewStyle().Foreground(mkFg).PaddingLeft(2)
	errorTitleSty = lipgloss.NewStyle().Bold(true).Foreground(mkPink).
			BorderStyle(lipgloss.ThickBorder()).BorderLeft(true).BorderForeground(mkPink).
			PaddingLeft(1)
	metaStyle       = lipgloss.NewStyle().Foreground(mkComment)
	statusIdleStyle = lipgloss.NewStyle().Foreground(mkGreen)
	statusBusyStyle = lipgloss.NewStyle().Foreground(mkOrange)
	noticeStyle     = lipgloss.NewStyle().Foreground(mkYellow)
	errNoticeStyle  = lipgloss.NewStyle().Foreground(mkPink)
	footerStyle     = lipgloss.NewStyle().Foreground(mkComment)
	// Question overlay selection: light text on a darker green — the old
	// dark-on-bright-green block glared at small sizes.
	selectedStyle = lipgloss.NewStyle().Foreground(mkFg).Background(mkGreenDark).Bold(true).Padding(0, 1)
	// Confirmation hint: bold green like the ready status, so the waiting
	// state is obvious at a glance.
	confirmHintStyle = lipgloss.NewStyle().Foreground(mkGreen).Bold(true)
	// Favourite marker in the model picker: warm yellow, like a lit star.
	favStyle = lipgloss.NewStyle().Foreground(mkYellow).Bold(true)
	// Model name in picker rows: bright and bold so it reads first while the
	// provider prefix stays dim.
	modelNameStyle = lipgloss.NewStyle().Foreground(mkFg).Bold(true)
	emptyStyle     = lipgloss.NewStyle().Foreground(mkComment).Padding(1, 2)
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
	// rowTop/rowBottom are the selected option's line span in the last
	// render, so key handling can scroll it into view on short terminals.
	rowTop, rowBottom int
}

// toolEventMsg streams tool progress from the background turn into the UI.
type toolEventMsg struct {
	kind   string
	text   string
	args   string
	output string
	id     string // tool call ID: matches rows by call, not by name
	durMs  int64  // wall execution time on tool_result
	idx    int    // reasoning part index on thinking_delta (see Event.Index)
}

// pendingTool is one in-flight or finished tool call in the live transcript.
type pendingTool struct {
	name   string
	args   string
	output string
	done   bool
	id     string
	durMs  int64
}

// chatTurnRender caches one committed turn's rendered forms for the scrollback
// budget: the full render and a lazily-built compact one (reasoning/reply
// bodies collapsed) used when the transcript is over max_lines. Line counts
// ride along so assembling the budget stays O(turns), not O(rendered bytes).
type chatTurnRender struct {
	full         string
	fullN        int
	compact      string
	compactN     int
	compactReady bool
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
	model    string
	effort   string
	plan     bool
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

// orCreditsMsg carries the OpenRouter credit snapshot into the UI thread.
type orCreditsMsg struct {
	credits ORCredits
	err     error
}

// usageTickMsg triggers a periodic quota refresh.
type usageTickMsg struct{}

// escConfirmWindow is how long a first esc stays armed while working.
const escConfirmWindow = 2 * time.Second

// escExpireMsg disarms a pending esc-to-cancel and restores the working hint.
type escExpireMsg struct{ at time.Time }

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
	statePath  string
	client     *openai.Client
	registry   *Registry
	session    *Session
	history    []openai.ChatCompletionMessage
	apiKey     string
	httpClient *http.Client
	goUsage    *GoUsage
	orCredits  *ORCredits

	viewport viewport.Model
	input    textarea.Model
	spinner  spinner.Model

	prog   *tea.Program
	ctx    context.Context
	cancel context.CancelFunc

	busy          bool
	pendingPrompt string
	pendingPlan   bool
	pendingSince  time.Time
	lastTurnDur   time.Duration // working time of the last finished turn, shown beside "ready"
	// retrying marks that the chatbox holds the recalled last prompt: the
	// next send rewinds that turn first. Cleared whenever the box empties.
	retrying   bool
	askSince   time.Time // set while a question overlay pauses the timer
	toolStatus string
	// roundCur/roundMax track loop progress for the status row (parsed from
	// round_start events); reset on submit and turnDone.
	roundCur, roundMax int
	// Live transcript state while a turn streams: tool calls in order plus
	// assistant text/thinking deltas so far. Cleared on turnDoneMsg.
	pendingTools    []pendingTool
	pendingText     string
	pendingThinking string
	pendingThinkIdx int // reasoning part index of the pendingThinking tail
	// roundText/roundThinking snapshot the pending streams at round start so
	// a retry can discard the failed attempt's partial output.
	roundText, roundThinking string
	lastStreamFlush          time.Time
	// chatCache memoizes the rendered committed turns so streaming deltas
	// only re-render the live pending block, not the whole history. Committed
	// turns are immutable, so the prefix extends append-only; any key change
	// (turn count reset, width, thinking toggle, model switch) rebuilds.
	// Each turn keeps a full and a lazily-rendered compact form; the budgeted
	// assembly in committedTurns picks per turn so older reasoning collapses
	// instead of feeding an ever-growing scrollback to the viewport.
	chatCacheTurns int
	chatCacheW     int
	chatCacheThink bool
	chatCacheModel string
	chatCache      []chatTurnRender
	// usageInflight dedupes quota refreshes: the minute tick and the
	// post-turn refresh share it, so slow networks never stack fetches.
	usageInflight bool
	// orCreditsInflight does the same for the OpenRouter balance fetch.
	orCreditsInflight bool
	notice            string
	noticeErr         bool
	// escArmedAt is when the first esc of a two-press cancel landed while
	// busy; zero when disarmed. A second esc within escConfirmWindow cancels.
	escArmedAt time.Time

	showThink bool
	showMeta  bool
	// follow pins the chat transcript to the newest line. Wheel/pgup
	// scrolling up clears it so streaming ticks stop yanking the view
	// back down; scrolling back to the bottom (or a new submit/commit)
	// sets it again.
	follow      bool
	mode        viewMode
	prevMode    viewMode
	cursor      int
	detailTitle string
	detailBody  string

	modelCatalog    []modelChoice
	modelChoices    []modelChoice
	modelFilter     string
	modelLoadFilter string
	modelSearch     textinput.Model
	favourites      map[string]bool
	effortLevels    []string
	ask             *askState

	// mouseHold buffers ambiguous SGR mouse-report fragments (split wheel
	// reports arrive as KeyRunes, indistinguishable from typing on
	// content alone). Held runes are never inserted: a reassembled
	// report is dropped, anything else is released as typing on
	// timeout or on the next unambiguous key. mouseHoldGen invalidates
	// stale flush timers.
	mouseHold    []tea.KeyMsg
	mouseHoldGen int
	// mouseOff is the alt+m selection toggle. Reporting stays on by default
	// so the wheel always scrolls; the bypass modifier (shift in most
	// terminals; opt in iTerm2/Terminal.app) drag-selects + copies without
	// toggling. With mouseOff set, terminal reporting is disabled via
	// tea.DisableMouse so a plain drag selects natively (wheel paused)
	// until alt+m re-enables it.
	mouseOff bool

	width  int
	height int
	ready  bool

	glam      *glamour.TermRenderer
	glamWidth int
	done      chan struct{}
}

// inputPlaceholder is the ghosted chatbox hint; plan mode tells the user how
// to leave instead of the generic prompt.
func inputPlaceholder(plan bool) string {
	if plan {
		return "Plan mode…  TAB to switch (/help)"
	}
	return "Type a message…  (/help)"
}

func newTUIModel(cfg Config, client *openai.Client, registry *Registry, session *Session) *tuiModel {
	ta := textarea.New()
	ta.Placeholder = inputPlaceholder(cfg.Plan)
	ta.ShowLineNumbers = false
	// No white thick bar: the textarea default Prompt is
	// lipgloss.ThickBorder().Left + " " (a white ▌ on every line).
	ta.Prompt = ""
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

	search := textinput.New()
	search.Placeholder = "Filter available models..."
	search.Prompt = ""
	search.CharLimit = 0
	search.Width = 52
	search.PlaceholderStyle = metaStyle
	search.TextStyle = lipgloss.NewStyle().Foreground(mkFg)
	search.Cursor.SetMode(cursor.CursorStatic)

	vp := viewport.New(80, 20)
	vp.MouseWheelEnabled = true

	sp := spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(lipgloss.NewStyle().Foreground(mkOrange)))

	m := &tuiModel{
		cfg: cfg, client: client, registry: registry, session: session,
		history:     seedHistory(cfg),
		viewport:    vp,
		input:       ta,
		spinner:     sp,
		modelSearch: search,
		showThink:   true,
		follow:      true,
		mode:        viewChat,
	}
	m.refreshViewport()
	return m
}

// Kitty keyboard protocol: push/pop "disambiguate escape codes" on the
// current screen's stack.
const (
	kittyKeysPush = "\x1b[>1u"
	kittyKeysPop  = "\x1b[<u"
)

func (m *tuiModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	cmds = append(cmds, func() tea.Msg {
		fmt.Fprint(os.Stdout, kittyKeysPush)
		return nil
	})
	if inputBlink(m.cfg) {
		cmds = append(cmds, textarea.Blink)
	}
	cmds = append(cmds, m.spinner.Tick)
	if c := m.fetchQuotaCmd(); c != nil {
		cmds = append(cmds, c)
	}
	cmds = append(cmds, usageTick())
	return tea.Batch(cmds...)
}

func (m *tuiModel) clearInput() {
	m.input.SetValue("")
	m.retrying = false
}

// dismissTransientNotice restores the turns/token status row after an
// informational (yellow) notice: any chatbox edit clears it so the useful
// metadata returns. Errors stay sticky until resolved.
func (m *tuiModel) dismissTransientNotice() {
	if m.notice != "" && !m.noticeErr {
		m.notice = ""
	}
}

// recallLastPrompt loads the previous prompt into the empty chatbox so it
// can be edited and sent again. It reports whether there was one.
func (m *tuiModel) recallLastPrompt() bool {
	if m.session == nil || len(m.session.Turns) == 0 {
		return false
	}
	m.input.SetValue(m.session.Turns[len(m.session.Turns)-1].Prompt)
	m.input.CursorEnd()
	m.retrying = true
	m.notice = "Editing previous prompt — enter resends from there, esc clears."
	m.noticeErr = false
	return true
}

// rewindLastTurn drops the last turn from memory and scrollback so a
// retried prompt continues from before it. The disk log stays append-only:
// the retry's checkpoint supersedes the dropped turn on resume. It does
// nothing unless the turn's prompt is still the last user message.
func (m *tuiModel) rewindLastTurn() {
	if m.session == nil || len(m.session.Turns) == 0 {
		return
	}
	last := m.session.Turns[len(m.session.Turns)-1]
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].Role != openai.ChatMessageRoleUser {
			continue
		}
		if m.history[i].Content != last.Prompt {
			return
		}
		m.history = m.history[:i:i]
		m.session.Turns = m.session.Turns[:len(m.session.Turns)-1]
		m.session.TotalUsage.Prompt -= last.Usage.Prompt
		m.session.TotalUsage.Completion -= last.Usage.Completion
		m.session.TotalUsage.Total -= last.Usage.Total
		m.chatCache, m.chatCacheTurns = nil, 0
		return
	}
}

// dingCmd rings the terminal bell (BEL) when [tui] sound is on. It writes
// straight to the tty: bubbletea v1 has no raw-output command, and a
// one-byte write cannot split a renderer frame.
func (m *tuiModel) dingCmd() tea.Cmd {
	if !tuiSound(m.cfg) {
		return nil
	}
	return func() tea.Msg {
		_, _ = os.Stdout.WriteString("\a")
		return nil
	}
}

// turnElapsed is the working time of the current turn; time spent waiting
// on a question overlay does not count.
func (m *tuiModel) turnElapsed() time.Duration {
	if !m.askSince.IsZero() {
		return m.askSince.Sub(m.pendingSince)
	}
	return time.Since(m.pendingSince)
}

// resumeTimer shifts the turn start forward by the time spent asking.
func (m *tuiModel) resumeTimer() {
	if m.askSince.IsZero() {
		return
	}
	if !m.pendingSince.IsZero() {
		m.pendingSince = m.pendingSince.Add(time.Since(m.askSince))
	}
	m.askSince = time.Time{}
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
	m.resumeTimer()
	if m.mode == viewAsk {
		m.show(viewChat) // restores the chat viewport height and chatbox
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
			m.resumeTimer()
			m.show(viewChat)
		case "esc", "n":
			m.dismissAsk(errors.New("user dismissed the questions"))
		}
	}
	m.refreshViewport()
	if m.ask != nil {
		m.scrollAskRowIntoView()
	}
	return m, nil
}

// scrollAskRowIntoView keeps the selected option on screen; a new question
// or phase starts from the top so its heading is never scrolled away.
func (m *tuiModel) scrollAskRowIntoView() {
	a := m.ask
	if a.phase != "pick" {
		m.viewport.GotoTop()
		return
	}
	if a.rowTop < m.viewport.YOffset {
		m.viewport.SetYOffset(a.rowTop)
	} else if bottom := m.viewport.YOffset + m.viewport.Height - 1; a.rowBottom > bottom {
		m.viewport.SetYOffset(a.rowBottom - m.viewport.Height + 1)
	}
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
		b.WriteString(m.wrapAskHint("Confirm your answers — enter submits · esc cancels") + "\n\n")
		answers := m.askAnswers()
		for i, q := range a.qs {
			b.WriteString(m.wrapPlain(q.Question) + "\n" + m.wrapPlain("  → "+answers[i]) + "\n\n")
		}
		b.WriteString(m.wrapAskHint("enter = submit · esc = cancel"))
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
			rendered := selectedStyle.Width(cw).Render(" " + row)
			a.rowTop = strings.Count(b.String(), "\n")
			a.rowBottom = a.rowTop + strings.Count(rendered, "\n")
			b.WriteString(rendered + "\n")
		} else {
			b.WriteString(m.wrapPlain("  "+row) + "\n")
		}
	}
	ownRow := breakLongWords(fmt.Sprintf("%d. Type your own answer", len(q.Options)+1), cw-2)
	if a.cursor == len(q.Options) {
		rendered := selectedStyle.Width(cw).Render(" " + ownRow)
		a.rowTop = strings.Count(b.String(), "\n")
		a.rowBottom = a.rowTop + strings.Count(rendered, "\n")
		b.WriteString(rendered + "\n")
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
func (m *tuiModel) finishPendingTool(id, name, output string, durMs int64) {
	if id != "" {
		for i := range m.pendingTools {
			if m.pendingTools[i].id == id && !m.pendingTools[i].done {
				m.pendingTools[i].done = true
				m.pendingTools[i].output = output
				m.pendingTools[i].durMs = durMs
				return
			}
		}
	}
	for i := len(m.pendingTools) - 1; i >= 0; i-- {
		if m.pendingTools[i].name == name && !m.pendingTools[i].done {
			m.pendingTools[i].done = true
			m.pendingTools[i].output = output
			m.pendingTools[i].durMs = durMs
			return
		}
	}
	m.pendingTools = append(m.pendingTools, pendingTool{name: name, output: output, done: true, durMs: durMs})
}

// flushStream re-renders the live transcript, throttled so a flood of deltas
// stays cheap. The committed turnDoneMsg always renders the final state.
func (m *tuiModel) flushStream() {
	if time.Since(m.lastStreamFlush) < 60*time.Millisecond {
		return
	}
	m.lastStreamFlush = time.Now()
	m.refreshViewport()
	if m.mode == viewChat && m.follow {
		m.viewport.GotoBottom()
	}
}

// fetchUsageCmd reloads the Go plan quota off the UI thread. It is a no-op
// (nil) when no HTTP client/key is configured, e.g. in model-only tests, or
// when a fetch is already in flight (the minute tick and post-turn refresh
// would otherwise overlap on slow networks).
func (m *tuiModel) fetchUsageCmd() tea.Cmd {
	if m.httpClient == nil || m.usageInflight {
		return nil
	}
	key := effectiveOpenCodeKey(m.cfg, m.apiKey)
	if key == "" {
		return nil
	}
	m.apiKey = key
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

// fetchORCreditsCmd reloads the OpenRouter balance off the UI thread. Like
// fetchUsageCmd it is a no-op (nil) without a client/key or while a fetch is
// already in flight. Non-management keys 403 and the footer stays hidden.
func (m *tuiModel) fetchORCreditsCmd() tea.Cmd {
	if m.httpClient == nil || m.orCreditsInflight {
		return nil
	}
	key := effectiveOpenRouterKey(m.cfg)
	if key == "" {
		return nil
	}
	m.orCreditsInflight = true
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	hc, url := m.httpClient, openRouterBaseURL
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, 10*time.Second)
		defer cancel()
		c, err := fetchOpenRouterCredits(ctx, hc, url, key)
		return orCreditsMsg{credits: c, err: err}
	}
}

// fetchQuotaCmd reloads whichever quota the footer shows for the current
// model: the OpenRouter balance on openrouter/… models, the Go plan quota
// otherwise. Callers (init, tick, post-turn, model switch) stay on this so
// the hidden provider never refreshes while selected.
func (m *tuiModel) fetchQuotaCmd() tea.Cmd {
	switch modelProvider(m.cfg.Model) {
	case ProviderOpenRouter:
		return m.fetchORCreditsCmd()
	case ProviderZen:
		return nil // Zen is pay-as-you-go: no plan quota to show
	}
	return m.fetchUsageCmd()
}

func usageTick() tea.Cmd {
	return tea.Tick(time.Minute, func(time.Time) tea.Msg { return usageTickMsg{} })
}

// maxColumnWidth caps the whole UI column (header, scrollback, status, input,
// footer). It is contentWidth's 120 plus the 8 columns of chrome, so
// scrollback text keeps its readable width and the rest lines up with it.
const maxColumnWidth = 128

// columnWidth is the width of the centered UI column: the terminal width,
// capped at maxColumnWidth.
func (m *tuiModel) columnWidth() int {
	if m.width > maxColumnWidth {
		return maxColumnWidth
	}
	return m.width
}

// isPickerMode reports whether the active view is a list picker (or the
// question overlay) that hides the chatbox and footer, leaving the whole
// height to the list so a question stays readable on short terminals.
func (m *tuiModel) isPickerMode() bool {
	return m.mode == viewSessions || m.mode == viewModels || m.mode == viewAsk
}

// maxInputHeight caps how far the chatbox grows with its content.
const maxInputHeight = 10

// inputRows counts the visual rows the chatbox text occupies at the current
// textarea width, soft wraps included (minimum 1).
func (m *tuiModel) inputRows() int {
	w := m.input.Width()
	if w <= 0 {
		return 1
	}
	rows := 0
	for _, ln := range strings.Split(m.input.Value(), "\n") {
		rows += len(wrapInputLine([]rune(ln), w))
	}
	if rows < 1 {
		rows = 1
	}
	return rows
}

// wrapInputLine mirrors the bubbles textarea word wrap so the chatbox can
// size itself to the rows the textarea will actually draw.
func wrapInputLine(runes []rune, width int) [][]rune {
	var (
		lines  = [][]rune{{}}
		word   []rune
		row    int
		spaces int
	)
	strw := func(r []rune) int { return lipgloss.Width(string(r)) }
	for _, r := range runes {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}
		if spaces > 0 {
			if strw(lines[row])+strw(word)+spaces > width {
				row++
				lines = append(lines, []rune{})
			}
			lines[row] = append(lines[row], word...)
			lines[row] = append(lines[row], []rune(strings.Repeat(" ", spaces))...)
			spaces = 0
			word = nil
		} else if strw(word)+lipgloss.Width(string(word[len(word)-1])) > width {
			if len(lines[row]) > 0 {
				row++
				lines = append(lines, []rune{})
			}
			lines[row] = append(lines[row], word...)
			word = nil
		}
	}
	if strw(lines[row])+strw(word)+spaces >= width {
		lines = append(lines, append(append([]rune{}, word...), []rune(strings.Repeat(" ", spaces+1))...))
	}
	return lines
}

// syncInputHeight grows the chatbox with its content, from the configured
// height up to maxInputHeight, and resizes the scrollback to match.
func (m *tuiModel) syncInputHeight() {
	h := m.inputRows()
	if h > maxInputHeight {
		h = maxInputHeight
	}
	if base := inputHeight(m.cfg); h < base {
		h = base
	}
	if h == m.input.Height() {
		return
	}
	m.input.SetHeight(h)
	// The textarea only re-clamps its internal scroll on Update; a nil
	// message keeps the cursor row visible after the resize.
	m.input, _ = m.input.Update(nil)
	if m.height == 0 {
		return
	}
	m.viewport.Height = m.viewportHeight()
	if m.follow && m.mode == viewChat {
		m.viewport.GotoBottom()
	}
}

// viewportHeight is the scrollback height for the current mode and terminal
// size. Chat reserves room for the chatbox, status and footer; pickers drop
// the chatbox and footer, so the viewport grows to keep the status row pinned
// to the bottom edge instead of leaving dead space beneath it.
func (m *tuiModel) viewportHeight() int {
	if m.height == 0 {
		return m.viewport.Height
	}
	h := m.height - 6 - m.input.Height()
	if m.isPickerMode() {
		h = m.height - 2 // header + status
	}
	if h < 5 {
		h = 5
	}
	return h
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
		// Measure display width, not runes: s may carry ANSI styling
		// (rendered markdown), and escape bytes must neither count toward
		// the width nor ever be split.
		if ansi.StringWidth(line) <= width {
			continue
		}
		words := strings.Split(line, " ")
		for j, w := range words {
			if ansi.StringWidth(w) > width {
				words[j] = ansi.Hardwrap(w, width, true)
			}
		}
		lines[i] = strings.Join(words, " ")
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

// wrapAskHint renders the question-overlay waiting hints (confirmation
// submit line) in bold green like the ready status, so it reads as waiting
// on the user rather than idle helper text.
func (m *tuiModel) wrapAskHint(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	return confirmHintStyle.Width(m.contentWidth()).Render(breakLongWords(s, m.contentWidth()))
}

// pinBottom re-renders and snaps the transcript to the newest line,
// re-enabling autoscroll. Use for explicit jumps (submit, commit,
// returning to chat); streaming ticks honor follow instead so a user
// reading history is not yanked down.
func (m *tuiModel) pinBottom() {
	m.follow = true
	m.refreshViewport()
	m.viewport.GotoBottom()
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
	m.viewport.Height = m.viewportHeight()
	if mode == viewChat {
		m.input.Focus()
		m.modelSearch.Blur()
	} else {
		m.input.Blur()
		if mode == viewModels {
			m.modelSearch.Focus()
		} else {
			m.modelSearch.Blur()
		}
	}
	m.refreshViewport()
	if mode == viewChat {
		m.follow = true
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
	if strings.EqualFold(trimmed, ":q") {
		return tea.Quit // vim-style alias for /quit
	}
	if strings.HasPrefix(trimmed, "/") {
		return m.handleCommand(trimmed)
	}
	if m.busy {
		m.notice = "Working… press esc twice to cancel."
		m.noticeErr = false
		return nil
	}
	// Refresh from the live env so an export after boot is picked up, then
	// fail fast with a notice (not a crash) when the current model has no key.
	m.apiKey = effectiveOpenCodeKey(m.cfg, m.apiKey)
	m.cfg.OpenRouterKey = effectiveOpenRouterKey(m.cfg)
	if err := missingKeyError(m.cfg, m.apiKey); err != nil {
		if modelProvider(m.cfg.Model) == ProviderOpenRouter {
			m.notice = fmt.Sprintf("OpenRouter key not found — export %s=... to use %s.", openRouterKeyEnv, m.cfg.Model)
		} else {
			m.notice = fmt.Sprintf("OpenCode API key not found — set %s=... to use %s.", openCodeKeyEnv, m.cfg.Model)
		}
		m.noticeErr = true
		m.refreshViewport()
		return nil
	}
	if m.retrying {
		m.rewindLastTurn()
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
	m.pendingPlan = m.cfg.Plan
	m.session.nameSession(trimmed)
	m.pendingSince = time.Now()
	m.askSince = time.Time{}
	m.toolStatus = "Sending…"
	m.roundCur, m.roundMax = 0, 0
	m.pendingTools = nil
	m.pendingText = ""
	m.pendingThinking = ""
	m.pendingThinkIdx = 0
	m.lastStreamFlush = time.Time{}
	m.notice = ""
	m.done = make(chan struct{})
	m.pinBottom()

	cfg := m.cfg
	client := m.client
	apiKey := m.apiKey
	httpClient := m.httpClient
	registry := m.registry
	prog := m.prog
	return func() tea.Msg {
		h := hist
		var tools []ToolRecord
		var thinking, answer string
		thinkIdx := 0
		var roundAnswer, roundThinking string
		onEvent := func(ev Event) {
			switch ev.Kind {
			case "round_start":
				answer = roundBreak(answer)
				thinking = roundBreak(thinking)
				roundAnswer, roundThinking = answer, thinking
			case "retry":
				// The failed attempt's partial output is replayed by the
				// next attempt; drop it so nothing shows twice.
				answer, thinking = roundAnswer, roundThinking
			case "tool_start":
				tools = append(tools, ToolRecord{ID: ev.ID, Name: ev.Text, Args: ev.Args})
			case "tool_result":
				for i := len(tools) - 1; i >= 0; i-- {
					if tools[i].ID == ev.ID && tools[i].Name == ev.Text {
						tools[i].Output = ev.Output
						tools[i].DurMs = ev.DurMs
						break
					}
				}
			case "text_delta":
				answer = appendStreamText(answer, ev.Text)
			case "thinking_delta":
				thinking, thinkIdx = appendThinkingDelta(thinking, ev.Text, ev.Index, thinkIdx)
			}
			if prog != nil {
				prog.Send(toolEventMsg{kind: ev.Kind, text: ev.Text, args: ev.Args, output: ev.Output, id: ev.ID, durMs: ev.DurMs, idx: ev.Index})
			}
		}
		reply, usage, err := runTurn(ctx, cfg, client, apiKey, httpClient, registry, &h, onEvent)
		if err == nil {
			streamedThinking := thinking
			thinking, answer = splitThinking(reply.Content)
			if reply.ReasoningContent != "" {
				thinking = reply.ReasoningContent
			}
			// The streamed buffer carries the part boundaries the live
			// view rendered (a newline between summary parts); the
			// authoritative reply may concatenate fragments exactly, so
			// prefer the streamed buffer for display when deltas
			// arrived. Empty means the non-streaming fallback (or
			// <think> in content only) with nothing to preserve.
			if streamedThinking != "" {
				thinking = streamedThinking
			}
		}
		return turnDoneMsg{prompt: trimmed, thinking: thinking, answer: answer, tools: tools, usage: usage, err: err, history: h, model: cfg.Model, effort: cfg.Thinking, plan: cfg.Plan}
	}
}

// applyPlanMode toggles plan mode at runtime: flips the flag, reseeds the
// system message so the next turn carries the plan overlay (or drops it),
// and notices which way we went. History after index 0 is untouched.
func (m *tuiModel) applyPlanMode(on bool) {
	m.cfg.Plan = on
	if len(m.history) > 0 && m.history[0].Role == openai.ChatMessageRoleSystem {
		m.history[0].Content = effectiveSystem(m.cfg)
	} else {
		m.history = seedHistory(m.cfg)
	}
	m.input.Placeholder = inputPlaceholder(on)
	if on {
		m.notice = "Plan Mode enabled: read only and planning"
	} else {
		m.notice = "Work mode enabled"
	}
	m.noticeErr = false
	if m.busy {
		// The running turn already captured its config and gates.
		m.notice += " Applies from your next message."
	}
	m.refreshViewport()
}

func (m *tuiModel) handleCommand(text string) tea.Cmd {
	fields := strings.Fields(text)
	cmd := strings.ToLower(fields[0])
	switch cmd {
	case "/clear":
		m.history = seedHistory(m.cfg)
		resetTodos()
		m.lastTurnDur = 0
		if m.session != nil {
			m.session.Turns = nil
			m.session.TotalUsage = Usage{}
		}
		m.notice = "Cleared — fresh conversation."
		m.noticeErr = false
		m.show(viewChat)
		return nil
	case "/new":
		m.startFreshSession()
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
			return m.fetchQuotaCmd()
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
	case "/plan":
		m.applyPlanMode(!m.cfg.Plan)
		return nil
	case "/quit", "/exit", "/q":
		return tea.Quit
	default:
		m.notice = fmt.Sprintf("Unknown command %s — try /help.", fields[0])
		m.noticeErr = true
		return nil
	}
}

// startFreshSession mints a brand-new blank session: a fresh ses-<nano> ID
// with its own JSONL, reseeded history, and all pending/live state dropped
// so memory and token history release. The old session file stays on disk,
// resumable via /sessions. Blocked while busy so an in-flight turn can't
// commit into (or be orphaned by) the fresh session.
func (m *tuiModel) startFreshSession() {
	if m.busy {
		m.notice = "Working… press esc twice to cancel."
		m.noticeErr = false
		return
	}
	m.cfg.SessionID = newSessionID()
	s, err := newSession(m.cfg)
	if err != nil {
		m.notice = "New session failed: " + err.Error()
		m.noticeErr = true
		return
	}
	m.session = s
	m.history = seedHistory(m.cfg)
	resetTodos()
	// SessionID is stamped per-request by headerTransport, so rebuild the
	// clients around the new ID; otherwise turns would keep attributing
	// to the old session server-side.
	m.httpClient = newHTTPClient(m.cfg)
	m.apiKey = effectiveOpenCodeKey(m.cfg, m.apiKey)
	m.cfg.OpenRouterKey = effectiveOpenRouterKey(m.cfg)
	m.client = newClient(m.cfg, m.apiKey)
	m.pendingPrompt = ""
	m.lastTurnDur = 0
	m.pendingTools = nil
	m.pendingText = ""
	m.pendingThinking = ""
	m.pendingThinkIdx = 0
	m.toolStatus = ""
	m.cancel = nil
	m.chatCache, m.chatCacheTurns = nil, 0
	m.clearInput()
	m.notice = "New session " + m.session.ID + " — blank."
	m.noticeErr = false
	m.show(viewChat)
}

// compactCmd runs a manual compaction off the UI thread: prune older tool
// output locally, then summarize the head with the cheap [compaction] model.
// Send-side only — the session JSONL keeps every turn.
func (m *tuiModel) compactCmd() tea.Cmd {
	if m.busy {
		m.notice = "Working… press esc twice to cancel."
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
	m.pinBottom()

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

// openModelPicker fetches the live catalogs off the UI thread: Go plus
// OpenRouter (models arrive stored with their provider prefix). Both are
// listed even without keys — fallbacks cover unreachable catalogs — so a
// missing key surfaces on pick/send, never as a boot crash. With a filter,
// an exact or unique-substring match switches directly; otherwise the
// filtered picker opens.
func (m *tuiModel) openModelPicker(filter string) tea.Cmd {
	m.notice = "Loading models…"
	m.noticeErr = false
	m.modelFilter = filter
	m.modelLoadFilter = filter
	m.modelSearch.SetValue(filter)
	cfg := m.cfg
	goKey := effectiveOpenCodeKey(cfg, m.apiKey)
	orKey := effectiveOpenRouterKey(cfg)
	client := m.client
	if client == nil && goKey != "" {
		client = newClient(cfg, goKey)
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var ids []string
		var err error
		if client != nil {
			ids, err = fetchGoModels(ctx, client)
		} else {
			err = fmt.Errorf("no client")
		}
		if err != nil || len(ids) == 0 {
			ids = append([]string(nil), fallbackGoModels...)
		}
		zenIDs, zenErr := fetchZenModels(ctx, nil, zenBaseURL, goKey)
		if zenErr != nil || len(zenIDs) == 0 {
			zenIDs = prefixedZenModels(fallbackZenModels)
		}
		ids = append(ids, zenIDs...)
		orIDs, orErr := fetchOpenRouterModels(ctx, nil, openRouterBaseURL, orKey)
		if orErr != nil || len(orIDs) == 0 {
			orIDs = prefixedOpenRouterFallback()
		}
		ids = append(ids, orIDs...)
		return modelsLoadedMsg{ids: ids, err: err, filter: filter}
	}
}

// filterModelChoices rebuilds the visible model rows from the full catalog and
// the current search text. Keeping the catalog intact lets clearing a query
// reveal models hidden by earlier filtering.
func (m *tuiModel) filterModelChoices() {
	ids := make([]string, 0, len(m.modelCatalog))
	byID := make(map[string]modelChoice, len(m.modelCatalog))
	for _, choice := range m.modelCatalog {
		ids = append(ids, choice.id)
		byID[choice.id] = choice
	}
	matched := matchGoModel(ids, m.modelFilter)
	m.modelChoices = m.modelChoices[:0]
	for _, id := range matched {
		m.modelChoices = append(m.modelChoices, byID[id])
	}
	// Starred models float to the top, keeping catalog (alphabetical) order
	// within each group so a toggle never shuffles the rest of the list.
	sort.SliceStable(m.modelChoices, func(i, j int) bool {
		fi, fj := m.isFavourite(m.modelChoices[i].id), m.isFavourite(m.modelChoices[j].id)
		return fi && !fj
	})
}

// favouritesPath is the picker's favourites file, "" when there is no state
// path (session-only, like model/thinking).
func (m *tuiModel) favouritesPath() string { return favouritesPathFor(m.statePath) }

func (m *tuiModel) isFavourite(id string) bool {
	return m.favourites[normalizeModelID(id)]
}

// toggleFavourite stars or unstars the highlighted model, persists the set
// and re-sorts the list, keeping the cursor on the same model. Ctrl+F drives
// it; Enter stays selection.
func (m *tuiModel) toggleFavourite() {
	if m.cursor < 0 || m.cursor >= len(m.modelChoices) {
		return
	}
	if m.favourites == nil {
		m.favourites = readFavourites(m.favouritesPath())
	}
	id := m.modelChoices[m.cursor].id
	key := normalizeModelID(id)
	added := !m.favourites[key]
	if added {
		m.favourites[key] = true
	} else {
		delete(m.favourites, key)
	}
	label := displayModelID(id)
	suffix := ""
	if path := m.favouritesPath(); path != "" {
		if err := saveFavourites(path, m.favourites); err != nil {
			m.notice = "Favourites save failed: " + err.Error()
			m.noticeErr = true
			m.refreshViewport()
			return
		}
	} else {
		suffix = " (session only)"
	}
	if added {
		m.notice = "★ " + label + " added to favourites." + suffix
	} else {
		m.notice = label + " removed from favourites." + suffix
	}
	m.noticeErr = false
	m.modelFilter = m.modelSearch.Value()
	m.filterModelChoices()
	for i, c := range m.modelChoices {
		if c.id == id {
			m.cursor = i
			break
		}
	}
	m.refreshViewport()
	m.ensureCursorVisible()
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
// level, and persists both to state.json so the choice sticks. Unknown
// models probe the provider in the background: a bogus effort 400s with the
// valid list, which is cached on disk for ctrl+t. A model whose provider key
// is missing still switches (so keyless boots can browse), but the notice
// says which key is missing instead of pretending the switch is ready.
func (m *tuiModel) applyModelSwitch(id string) {
	prevThinking := m.cfg.Thinking
	m.cfg.Model = id
	if len(m.history) > 0 && m.history[0].Role == openai.ChatMessageRoleSystem {
		m.history[0].Content = effectiveSystem(m.cfg)
	} else {
		m.history = append(seedHistory(m.cfg), m.history...)
	}
	resetNote := ""
	if !validThinkingLevel(id, m.cfg.Thinking) {
		m.cfg.Thinking = ""
		if prevThinking != "" {
			resetNote = fmt.Sprintf(" Thinking reset to default (%s is not available on %s).", prevThinking, id)
		}
	}
	msg := fmt.Sprintf("Switched to %s.%s", id, resetNote)
	if m.statePath != "" {
		if err := saveModelState(m.statePath, m.cfg.Model, m.cfg.Thinking); err != nil {
			m.notice = msg + " (state save failed: " + err.Error() + ")"
			m.noticeErr = true
		} else {
			m.notice = msg + " Saved."
			m.noticeErr = false
		}
	} else {
		m.notice = msg + " (session only)"
		m.noticeErr = false
	}
	if err := missingKeyError(m.cfg, m.apiKey); err != nil {
		if modelProvider(id) == ProviderOpenRouter {
			m.notice = fmt.Sprintf("Switched to %s. OpenRouter key not found — export %s=... to use it.", id, openRouterKeyEnv)
		} else {
			m.notice = fmt.Sprintf("Switched to %s. OpenCode API key not found — set %s=... to use it.", id, openCodeKeyEnv)
		}
		m.noticeErr = true
	}
	m.show(viewChat)
	if _, ok := cachedThinkingLevels(id); !ok {
		if _, ok := lookupKnownLevels(modelBaseID(id)); !ok && m.httpClient != nil && missingKeyError(m.cfg, m.apiKey) == nil {
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
	if m.statePath != "" {
		if err := saveModelState(m.statePath, m.cfg.Model, m.cfg.Thinking); err != nil {
			m.notice = msg + " (state save failed: " + err.Error() + ")"
			m.noticeErr = true
		} else {
			m.notice = msg + " Saved."
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
			fmt.Fprintf(&b, "<details><summary>thinking%s</summary>\n\n%s\n\n</details>\n\n", thinkDurSuffix(t.DurMs), t.Thinking)
		}
		for _, tr := range t.Tools {
			fmt.Fprintf(&b, "> tool `%s` `%s`%s\n>\n> %s\n\n", tr.Name, truncatePlain(tr.Args, 120), toolDurSuffix(tr.DurMs), truncatePlain(tr.Output, 2000))
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
	mdl, cmd := m.update(msg)
	m.syncInputHeight()
	return mdl, cmd
}

func (m *tuiModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// Disambiguated ctrl+m (the letter form terminals send once a
	// disambiguating mode is on) opens the model picker from anywhere an
	// overlay question is not waiting for its keys.
	if ctrlMCSI(msg) {
		if m.ask != nil {
			return m, nil
		}
		return m, m.openModelPicker("")
	}

	// Kitty disambiguate mode reports esc and ctrl/alt+key as CSI-u, which
	// bubbletea drops as unknown: translate back into ordinary key events.
	if km, ok := csiUKey(msg); ok {
		return m.Update(km)
	}

	// CSI-u / modifyOtherKeys enter: shift+enter (and friends) that legacy
	// terminals collapse into plain enter. mod 1 is unmodified enter, so it
	// submits like the enter key; any modifier inserts a newline instead.
	if mod, ok := enterCSIMod(msg); ok {
		if m.ask != nil || m.mode != viewChat {
			return m, nil
		}
		if mod == 1 {
			return m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		}
		m.insertInputNewline()
		return m, nil
	}

	switch msg := msg.(type) {
	case mouseHoldFlushMsg:
		// Ambiguous fragments that never completed a mouse report were
		// typed text: release them. A completed report is already
		// dropped at arrival, so anything still held here is typing.
		if msg.gen == m.mouseHoldGen {
			m.flushMouseHold()
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		// Chrome around the transcript: header, status, footer, the input
		// border plus a line of slack (6) beside the configured box lines.
		// Pickers hide the chatbox and footer, so viewportHeight reclaims
		// that space for the scrollback.
		vpW := m.columnWidth() - 4
		if vpW < 40 {
			vpW = 40
		}
		m.viewport.Width = vpW
		m.viewport.Height = m.viewportHeight()
		m.input.SetWidth(vpW - 2)
		m.glam = nil
		m.refreshViewport()
		if m.follow && m.mode != viewAsk {
			m.viewport.GotoBottom()
		}
		return m, nil

	case spinner.TickMsg:
		if m.busy {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
			// Only the status-line spinner animates on the tick; the scrollback
			// is refreshed by flushStream on real content changes, so a tick no
			// longer re-renders (and re-lexes) the whole pending block.
		}
		return m, tea.Batch(cmds...)

	case toolEventMsg:
		switch msg.kind {
		case "round_start":
			m.toolStatus = "Reasoning…"
			if cur, max, ok := parseRoundProgress(msg.text); ok {
				m.roundCur, m.roundMax = cur, max
			}
			m.pendingText = roundBreak(m.pendingText)
			m.pendingThinking = roundBreak(m.pendingThinking)
			m.roundText, m.roundThinking = m.pendingText, m.pendingThinking
		case "retry":
			m.pendingText, m.pendingThinking = m.roundText, m.roundThinking
			m.toolStatus = msg.text
		case "tool_start":
			m.pendingTools = append(m.pendingTools, pendingTool{name: msg.text, args: msg.args, id: msg.id})
			if n := m.runningToolCount(); n > 1 {
				m.toolStatus = fmt.Sprintf("%s… · %d running", msg.text, n)
			} else {
				m.toolStatus = msg.text + "…"
			}
		case "tool_result":
			m.finishPendingTool(msg.id, msg.text, msg.output, msg.durMs)
			if n := m.runningToolCount(); n > 0 {
				m.toolStatus = fmt.Sprintf("✓ %s · %d still running", msg.text, n)
			} else {
				m.toolStatus = "✓ " + msg.text
			}
		case "text_delta":
			m.pendingText = appendStreamLine(m.pendingText, msg.text)
			m.toolStatus = "writing…"
			m.flushStream()
			return m, nil
		case "thinking_delta":
			m.pendingThinking, m.pendingThinkIdx = appendThinkingDelta(m.pendingThinking, msg.text, msg.idx, m.pendingThinkIdx)
			m.toolStatus = "Reasoning…"
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

	case orCreditsMsg:
		m.orCreditsInflight = false
		if msg.err == nil {
			c := msg.credits
			m.orCredits = &c
		}
		return m, nil

	case escExpireMsg:
		if m.escArmedAt.Equal(msg.at) {
			m.escArmedAt = time.Time{}
			if m.busy {
				m.notice = "Working… press esc twice to cancel."
				m.noticeErr = false
			}
		}
		return m, nil
	case usageTickMsg:
		return m, tea.Batch(m.fetchQuotaCmd(), usageTick())

	case modelsLoadedMsg:
		if msg.filter != m.modelLoadFilter {
			return m, nil // stale fetch; a newer picker request is in flight
		}
		ids := msg.ids
		if msg.err != nil || len(ids) == 0 {
			// Go live list failed: keep any OpenRouter rows already merged
			// (live or fallback) and backfill Go with the curated list, so
			// keyless boots still browse both providers.
			var orKeep, zenKeep []string
			for _, id := range msg.ids {
				switch modelProvider(id) {
				case ProviderOpenRouter:
					orKeep = append(orKeep, id)
				case ProviderZen:
					zenKeep = append(zenKeep, id)
				}
			}
			if len(orKeep) == 0 {
				orKeep = prefixedOpenRouterFallback()
			}
			if len(zenKeep) == 0 {
				zenKeep = prefixedZenModels(fallbackZenModels)
			}
			ids = append(append([]string(nil), fallbackGoModels...), zenKeep...)
			ids = append(ids, orKeep...)
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
				return m, m.fetchQuotaCmd()
			}
		}
		sort.Strings(ids)
		m.modelCatalog = m.modelCatalog[:0]
		for _, id := range ids {
			m.modelCatalog = append(m.modelCatalog, modelChoice{id: id, current: id == m.cfg.Model, transport: modelTransport(id)})
		}
		m.modelSearch.SetValue(msg.filter)
		m.modelFilter = msg.filter
		if path := m.favouritesPath(); path != "" {
			m.favourites = readFavourites(path)
		}
		if m.favourites == nil {
			m.favourites = map[string]bool{}
		}
		m.filterModelChoices()
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
		if m.askSince.IsZero() {
			m.askSince = time.Now()
		}
		m.input.Blur()
		m.viewport.Height = m.viewportHeight() // overlay hides the chatbox
		m.refreshViewport()
		m.viewport.GotoTop()
		return m, m.dingCmd()

	case turnDoneMsg:
		turn := Turn{
			At: msg.promptAt(), Prompt: msg.prompt, Thinking: msg.thinking, Reply: msg.answer, Tools: msg.tools,
			Usage: Usage{Prompt: msg.usage.PromptTokens, Completion: msg.usage.CompletionTokens, Total: msg.usage.TotalTokens},
			Model: msg.model, Effort: msg.effort, Plan: msg.plan, History: msg.history,
		}
		if !m.pendingSince.IsZero() {
			m.resumeTimer()
			turn.DurMs = time.Since(m.pendingSince).Milliseconds()
			if turn.DurMs < 1 {
				turn.DurMs = 1
			}
			m.lastTurnDur = time.Duration(turn.DurMs) * time.Millisecond
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
		m.history = append([]openai.ChatCompletionMessage(nil), msg.history...)
		// Model/plan switches during a request apply to the next request, not
		// the completed turn's attribution or checkpoint.
		if len(m.history) > 0 && m.history[0].Role == openai.ChatMessageRoleSystem {
			m.history[0].Content = effectiveSystem(m.cfg)
		}
		m.busy = false
		m.pendingPrompt = ""
		m.toolStatus = ""
		m.roundCur, m.roundMax = 0, 0
		m.pendingTools = nil
		m.pendingText = ""
		m.pendingThinking = ""
		m.pendingThinkIdx = 0
		m.cancel = nil
		if m.session != nil {
			if err := m.session.append(turn); err != nil {
				m.notice = "Session save failed: " + err.Error()
				m.noticeErr = true
			} else {
				m.session.ensureMeta(msg.model, msg.effort)
			}
		}
		if m.done != nil {
			close(m.done)
			m.done = nil
		}
		m.input.Focus()
		m.pinBottom()
		if msg.err != nil && isCancel(msg.err) {
			// The user stopped it themselves: no bell.
			return m, m.fetchQuotaCmd()
		}
		return m, tea.Batch(m.fetchQuotaCmd(), m.dingCmd())

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
		m.pinBottom()
		return m, nil

	case tea.MouseMsg:
		// With reporting toggled off (alt+m) there is nothing to scroll:
		// the terminal keeps drags for native selection, so stray
		// reports are ignored instead of moving the viewport.
		if m.mouseOff {
			return m, nil
		}
		// Wheel (and clicks) go to the viewport in every mode: in chat
		// the textarea would swallow them and the scrollback would never
		// move. Scrolling away from the bottom pauses autoscroll;
		// scrolling back to the bottom resumes it.
		var vcmd tea.Cmd
		m.viewport, vcmd = m.viewport.Update(msg)
		if m.mode == viewChat {
			m.follow = m.viewport.AtBottom()
		}
		if vcmd != nil {
			cmds = append(cmds, vcmd)
		}
		return m, tea.Batch(cmds...)

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
	} else if m.mode == viewModels {
		var scmd tea.Cmd
		m.modelSearch, scmd = m.modelSearch.Update(msg)
		if scmd != nil {
			cmds = append(cmds, scmd)
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
	m.dismissTransientNotice()
}

// parseEnterCSI matches the byte sequences terminals send for modifier+enter
// once a disambiguating mode is on: Kitty's CSI 13;mod u and xterm
// modifyOtherKeys' CSI 13;mod ~. mod follows the CSI-u convention
// (1 none, 2 shift, 3 alt, 4 shift+alt, 5 ctrl, …); absent means 1.
func parseEnterCSI(seq []byte) (mod int, ok bool) {
	// ESC [ 1 3 [; mod] (u | ~)
	if len(seq) < 4 || seq[0] != 0x1b || seq[1] != '[' || seq[2] != '1' || seq[3] != '3' {
		return 0, false
	}
	rest := seq[4:]
	if len(rest) == 1 && (rest[0] == 'u' || rest[0] == '~') {
		return 1, true
	}
	if len(rest) >= 3 && rest[0] == ';' && (rest[len(rest)-1] == 'u' || rest[len(rest)-1] == '~') {
		n := 0
		for _, c := range rest[1 : len(rest)-1] {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		if n >= 1 && n <= 16 {
			return n, true
		}
	}
	return 0, false
}

// ctrlMCSI reports whether msg carries a disambiguated ctrl+m letter
// sequence (CSI 109;5u or CSI 109;5~). Like enterCSIMod, the message is an
// unexported unknown-CSI []byte-kind value, so kind+content matching is
// precise without naming that type. Only the ctrl form (mod 5) counts: a
// bare CSI 109u would be a plain "m" keystroke, which must keep typing.
// Byte-13 ctrl+m can never arrive here — legacy terminals collapse it into
// plain enter by design — so there is deliberately no handleKey "ctrl+m"
// case (bubbletea aliases KeyCtrlM to KeyEnter and stringifies it "enter").
func ctrlMCSI(msg tea.Msg) bool {
	v := reflect.ValueOf(msg)
	if !v.IsValid() || v.Kind() != reflect.Slice || v.Type().Elem().Kind() != reflect.Uint8 {
		return false
	}
	seq := v.Bytes()
	// ESC [ 1 0 9 ; 5 (u | ~), exact length: longer runs are not ours.
	if len(seq) != 8 {
		return false
	}
	if seq[0] != 0x1b || seq[1] != '[' || seq[2] != '1' || seq[3] != '0' || seq[4] != '9' || seq[5] != ';' || seq[6] != '5' {
		return false
	}
	return seq[7] == 'u' || seq[7] == '~'
}

// csiUKey decodes the Kitty "disambiguate" reports bubbletea v1 leaves as
// unknown CSI: ESC[27u (esc), ESC[<letter>;5u (ctrl+letter) and
// ESC[<char>;3u (alt+char). Anything else (enter, ctrl+m, unmodified
// text) is left to its own handler. Lock bits (caps/num) are ignored.
func csiUKey(msg tea.Msg) (tea.KeyMsg, bool) {
	v := reflect.ValueOf(msg)
	if !v.IsValid() || v.Kind() != reflect.Slice || v.Type().Elem().Kind() != reflect.Uint8 {
		return tea.KeyMsg{}, false
	}
	seq := string(v.Bytes())
	if len(seq) < 4 || !strings.HasPrefix(seq, "\x1b[") || seq[len(seq)-1] != 'u' {
		return tea.KeyMsg{}, false
	}
	cpStr, modStr, _ := strings.Cut(seq[2:len(seq)-1], ";")
	cp, err := strconv.Atoi(cpStr)
	if err != nil {
		return tea.KeyMsg{}, false
	}
	mod := 1
	if modStr != "" {
		if mod, err = strconv.Atoi(modStr); err != nil || mod < 1 {
			return tea.KeyMsg{}, false
		}
	}
	switch bits := (mod - 1) & 0x3f; {
	case cp == 27 && bits == 0:
		return tea.KeyMsg{Type: tea.KeyEsc}, true
	case bits == 4 && cp >= 'a' && cp <= 'z' && cp != 'm':
		return tea.KeyMsg{Type: tea.KeyType(cp - 'a' + 1)}, true
	case bits == 2 && cp > 32 && cp < 127:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{rune(cp)}, Alt: true}, true
	}
	return tea.KeyMsg{}, false
}

// enterCSIMod reports the CSI-u/modifyOtherKeys enter modifier for a message
// carrying one of the parseEnterCSI sequences. Bubbletea reports those as an
// unexported unknown-CSI message — the only []byte-kind message on the loop —
// so kind+content matching is precise without naming that type.
func enterCSIMod(msg tea.Msg) (int, bool) {
	v := reflect.ValueOf(msg)
	if !v.IsValid() || v.Kind() != reflect.Slice || v.Type().Elem().Kind() != reflect.Uint8 {
		return 0, false
	}
	return parseEnterCSI(v.Bytes())
}

// Split SGR mouse reports (ESC[<Cb;Cx;CyM, one per wheel tick) can land
// as KeyRunes when a report straddles two TTY reads; genuine wheel input
// still arrives as tea.MouseMsg and is untouched (see Update). Fragments
// are printable ([, <, digits, ;, M) so no content blacklist can catch
// them without also eating typing — instead, ambiguous fragments are
// held briefly and reassembled: a completed report is dropped, anything
// else is released as typing. Pasted text never participates: pastes
// arrive with Paste set and bypass the hold entirely.
//
// sgrReportComplete matches one reassembled report (button/cell/release);
// sgrReportPrefix matches its strict prefixes, the only shapes worth
// holding. Anything else (digits without "<", stray M, overlong runs)
// can never become a report and types immediately.
var (
	sgrReportCompleteRe = regexp.MustCompile(`^\[?<\d+;\d+;\d+[Mm]$`)
	sgrReportPrefixRe   = regexp.MustCompile(`^\[?$|^\[?<\d{0,6};?\d{0,6};?\d{0,6}$`)
)

// mouseHoldDelay bounds how long ambiguous fragments stay invisible while
// waiting for the rest of a possibly split report.
const mouseHoldDelay = 50 * time.Millisecond

// mouseHoldCap bounds the hold buffer: a real report is under 20 bytes,
// so anything longer is typing, not mouse.
const mouseHoldCap = 24

// mouseHoldFlushMsg releases held fragments as typing when no report
// completed in time. gen drops stale timers after newer holds.
type mouseHoldFlushMsg struct {
	gen int
}

// mouseFragmentText reports whether msg could be part of a split SGR
// report, returning its ESC-stripped text. Only plain single-stream
// runes qualify: pastes, alt combos (except the lone "[" left when ESC
// splits off), and special keys never do.
func mouseFragmentText(msg tea.KeyMsg) (string, bool) {
	if msg.Paste || msg.Type != tea.KeyRunes || len(msg.Runes) == 0 {
		return "", false
	}
	if msg.Alt {
		if len(msg.Runes) == 1 && msg.Runes[0] == '[' {
			return "[", true
		}
		return "", false
	}
	for _, r := range msg.Runes {
		switch r {
		case '[', '<', ';', 'M', 'm', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		default:
			return "", false
		}
	}
	return string(msg.Runes), true
}

func (m *tuiModel) mouseHoldText() string {
	var b strings.Builder
	for _, h := range m.mouseHold {
		if t, ok := mouseFragmentText(h); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}

// flushMouseHold releases held fragments to their sink, replaying each
// exactly as the normal path would: the chat textarea in viewChat, the
// ask free-text box in its own phase. Overlays and pick/review ignore
// typing, so held runes there are dropped like typed ones would be.
func (m *tuiModel) flushMouseHold() {
	held := m.mouseHold
	m.mouseHold = nil
	if len(held) == 0 {
		return
	}
	if m.ask != nil && m.ask.phase == "own" {
		for _, h := range held {
			if s := h.String(); len(s) == 1 {
				m.ask.own += s
			}
		}
		return
	}
	if m.mode == viewChat && m.ask == nil {
		before := m.input.Value()
		for _, h := range held {
			m.input, _ = m.input.Update(h)
		}
		if m.input.Value() != before {
			m.dismissTransientNotice()
		}
	}
}

// filterMouseFragment holds a possible split-report fragment instead of
// typing it. It reports consumed=true when the key was held or dropped
// (caller returns); consumed=false means the hold was flushed (or empty)
// and the key must flow through the normal path.
func (m *tuiModel) filterMouseFragment(msg tea.KeyMsg) (tea.Cmd, bool) {
	frag, ok := mouseFragmentText(msg)
	if !ok {
		m.flushMouseHold()
		return nil, false
	}
	combined := m.mouseHoldText() + frag
	if sgrReportCompleteRe.MatchString(combined) {
		// A split report reassembled: drop it all, it was never typed.
		m.mouseHold = nil
		m.mouseHoldGen++
		return nil, true
	}
	if len(combined) > mouseHoldCap || !sgrReportPrefixRe.MatchString(combined) {
		// Can never become a mouse report: release as typing now.
		m.flushMouseHold()
		return nil, false
	}
	m.mouseHold = append(m.mouseHold, msg)
	m.mouseHoldGen++
	gen := m.mouseHoldGen
	return tea.Tick(mouseHoldDelay, func(time.Time) tea.Msg {
		return mouseHoldFlushMsg{gen: gen}
	}), true
}

func (m *tuiModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Possible split wheel-mouse reports are held for reassembly, never
	// typed: only pasted or typed chars may reach the input box. Genuine
	// wheel input arrives as tea.MouseMsg (see Update).
	if cmd, consumed := m.filterMouseFragment(msg); consumed {
		return m, cmd
	}
	s := msg.String()

	// Global: cancel / quit.
	switch s {
	case "ctrl+c", "ctrl+d":
		if m.ask != nil {
			m.dismissAsk(errors.New("user dismissed the questions"))
			return m, nil
		}
		if m.busy {
			m.cancelRun()
			if s == "ctrl+c" {
				m.notice = "Cancelling…"
				m.noticeErr = false
			}
			return m, nil
		}
		// Idle: first press clears a non-empty chatbox, next one quits.
		if m.input.Value() != "" {
			m.clearInput()
			return m, nil
		}
		return m, tea.Quit
	case "esc":
		if m.ask != nil {
			return m.handleAskKey(s)
		}
		if m.mode == viewModels {
			if strings.TrimSpace(m.modelSearch.Value()) != "" {
				m.modelSearch.SetValue("")
				m.modelFilter = ""
				m.filterModelChoices()
				m.cursor = 0
				m.refreshViewport()
				m.viewport.GotoTop()
				return m, nil
			}
			m.show(viewChat)
			return m, nil
		}
		if m.mode == viewSessions {
			// Back to the current session; never arms the cancel confirm.
			m.show(viewChat)
			return m, nil
		}
		if m.busy {
			now := time.Now()
			if !m.escArmedAt.IsZero() && now.Sub(m.escArmedAt) <= escConfirmWindow {
				m.escArmedAt = time.Time{}
				m.cancelRun()
				m.notice = "Cancelling…"
				m.noticeErr = false
				return m, nil
			}
			m.escArmedAt = now
			m.notice = "Press esc again within 2s to stop."
			m.noticeErr = false
			return m, tea.Tick(escConfirmWindow, func(time.Time) tea.Msg { return escExpireMsg{at: now} })
		}
		if m.mode != viewChat {
			m.show(viewChat)
			return m, nil
		}
		if m.input.Value() != "" {
			m.clearInput()
			return m, nil
		}
		return m, nil
	}

	// Ctrl shortcuts work everywhere; /help lists them.
	if m.ask != nil {
		return m.handleAskKey(s)
	}
	switch s {
	case "alt+m":
		// Selection toggle: reporting on (default) keeps wheel-scroll
		// with bypass-modifier drag-select; off disables terminal
		// reporting so a plain drag selects + copies natively.
		m.mouseOff = !m.mouseOff
		if m.mouseOff {
			m.notice = "Mouse off — drag selects + copies natively (wheel paused) · alt+m re-enables"
			m.noticeErr = false
			return m, tea.DisableMouse
		}
		m.notice = "Mouse on — wheel scrolls · shift+drag (opt+drag in iTerm/Terminal) selects + copies"
		m.noticeErr = false
		return m, tea.EnableMouseCellMotion
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
	case "ctrl+n":
		// Fresh blank session; blocked while busy (see startFreshSession).
		// Shadows textarea's down-history (unused: no history recall here).
		m.startFreshSession()
		return m, nil
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
		return m.handleOverlayKey(msg, s)
	}

	switch s {
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		if m.busy {
			m.notice = "Working… press esc twice to cancel."
			m.noticeErr = false
			return m, nil
		}
		m.input.SetValue("")
		cmd := m.submit(text) // consumes m.retrying
		m.retrying = false
		if cmd == nil {
			return m, nil
		}
		// Keep the spinner ticking while the turn runs.
		return m, tea.Batch(cmd, m.spinner.Tick)
	case "tab":
		// Tab swaps work and plan mode, same as /plan. The textarea has no
		// use for it (no tab character in prompts).
		m.applyPlanMode(!m.cfg.Plan)
		return m, nil
	case "shift+enter", "alt+enter", "ctrl+j":
		// "shift+enter" only arrives from terminals that report it as a
		// distinct key; most send plain enter unless Kitty enhancement is
		// on (pushed in runTUI, decoded above as CSI 13;2u). alt+enter and
		// ctrl+j are the bindings that work everywhere.
		m.insertInputNewline()
		return m, nil
	case "pgup", "pgdown", "ctrl+d":
		var vcmd tea.Cmd
		m.viewport, vcmd = m.viewport.Update(msg)
		m.follow = m.viewport.AtBottom()
		return m, vcmd
	case "up", "down":
		// Let the textarea move first; viewport scrolls with pgup/pgdn.
		// On the first/last visual row there is nowhere to go, so jump to
		// the start/end of the text instead (shell-style).
		if s == "up" && m.input.Value() == "" && !m.busy && m.recallLastPrompt() {
			return m, nil
		}
		li := m.input.LineInfo()
		if s == "up" && m.input.Line() == 0 && li.RowOffset == 0 {
			m.input.SetCursor(0)
			return m, nil
		}
		if s == "down" && m.input.Line() == m.input.LineCount()-1 && li.RowOffset >= li.Height-1 {
			m.input.CursorEnd()
			return m, nil
		}
		var icmd tea.Cmd
		before := m.input.Value()
		m.input, icmd = m.input.Update(msg)
		if m.input.Value() != before {
			m.dismissTransientNotice()
		}
		return m, icmd
	default:
		var icmd tea.Cmd
		before := m.input.Value()
		m.input, icmd = m.input.Update(msg)
		if m.input.Value() != before {
			m.dismissTransientNotice()
		}
		if m.input.Value() == "" {
			m.retrying = false
		}
		return m, icmd
	}
}

// ensureCursorVisible scrolls the overlay viewport just enough to keep the
// cursor row on screen. Regular lists put a two-line title above their rows;
// the model picker adds its search box and uses cursor+4 instead.
func (m *tuiModel) ensureCursorVisible() {
	line := m.cursor + 2
	if m.mode == viewModels {
		line = m.cursor + 4
	}
	if line < m.viewport.YOffset {
		m.viewport.SetYOffset(line)
	} else if bottom := m.viewport.YOffset + m.viewport.Height - 1; line > bottom {
		m.viewport.SetYOffset(line - m.viewport.Height + 1)
	}
}

func (m *tuiModel) handleOverlayKey(msg tea.KeyMsg, s string) (tea.Model, tea.Cmd) {
	if m.mode == viewModels {
		switch s {
		case "up":
			if m.cursor > 0 {
				m.cursor--
				m.refreshViewport()
				m.ensureCursorVisible()
			}
			return m, nil
		case "down":
			if m.cursor < len(m.modelChoices)-1 {
				m.cursor++
				m.refreshViewport()
				m.ensureCursorVisible()
			}
			return m, nil
		case "enter":
			return m, m.openSelected()
		case "ctrl+f":
			// Ctrl+F stars the highlighted model; Enter keeps selecting.
			// A printable key would be swallowed before the search box sees
			// it, so the toggle needs a modifier.
			m.toggleFavourite()
			return m, nil
		case "pgup", "pgdown":
			if s == "pgup" {
				m.viewport.PageUp()
			} else {
				m.viewport.PageDown()
			}
			return m, nil
		}
		old := m.modelSearch.Value()
		var cmd tea.Cmd
		m.modelSearch, cmd = m.modelSearch.Update(msg)
		if next := m.modelSearch.Value(); next != old {
			m.modelFilter = next
			m.filterModelChoices()
			m.cursor = 0
			m.refreshViewport()
			m.viewport.GotoTop()
		}
		return m, cmd
	}
	switch s {
	case "q":
		m.show(viewChat)
		return m, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.refreshViewport()
			m.ensureCursorVisible()
		}
		return m, nil
	case "down", "j":
		m.cursor++
		m.refreshViewport()
		m.ensureCursorVisible()
		return m, nil
	case "enter", " ":
		return m, m.openSelected()
	case "pgup", "pgdown", "ctrl+d":
		// Paging scrolls the viewport; cursor moves with the arrow keys or j/k.
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

func (m *tuiModel) openSelected() tea.Cmd {
	switch m.mode {
	case viewModels:
		if m.cursor < 0 || m.cursor >= len(m.modelChoices) {
			return nil
		}
		m.applyModelSwitch(m.modelChoices[m.cursor].id)
		return m.fetchQuotaCmd()
	case viewEffort:
		if m.cursor < 0 || m.cursor >= len(m.effortLevels) {
			return nil
		}
		m.applyThinkingSwitch(m.effortLevels[m.cursor])
	case viewHistory:
		items := m.historyItems()
		if m.cursor < 0 || m.cursor >= len(items) {
			return nil
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
			return nil
		}
		r := items[m.cursor].record
		m.detailTitle = "Tool " + r.Name
		m.detailBody = fmt.Sprintf("args:\n%s\n\noutput:\n%s", r.Args, r.Output)
		m.show(viewDetail)
	case viewSessions:
		items := m.sessionItems()
		if m.cursor < 0 || m.cursor >= len(items) {
			return nil
		}
		m.resumeSessionPath(items[m.cursor].path)
	case viewErrors:
		idx := m.errorTurnIdx()
		if m.cursor < 0 || m.cursor >= len(idx) {
			return nil
		}
		t := m.session.Turns[idx[m.cursor]]
		m.detailTitle = "Error · " + t.At.Format("15:04:05")
		if t.Error != nil {
			m.detailBody = t.Error.Title + "\n\n" + t.Error.Details
		}
		m.show(viewDetail)
	}
	return nil
}

// View renders header / viewport / status / input / footer.
func (m *tuiModel) View() string {
	if m.width == 0 && !m.ready {
		// Pre-window fallback used by tests.
		return m.renderViewportContent()
	}
	header := m.headerView()
	status := m.statusView()
	parts := []string{header, m.viewport.View(), status}
	// The picker views (sessions and models) are navigation surfaces: the
	// chatbox and quota footer don't apply there, but the ready/working
	// status stays so the session state remains visible.
	if !m.isPickerMode() {
		parts = append(parts, m.inputView(), m.footerView())
	}
	block := lipgloss.JoinVertical(lipgloss.Left, parts...)
	// Never exceed the terminal height: an extra row scrolls the whole
	// screen up and shifts the status/chatbox off the bottom edge.
	if m.height > 0 {
		if lines := strings.Split(block, "\n"); len(lines) > m.height {
			block = strings.Join(lines[len(lines)-m.height:], "\n")
		}
	}
	// Center the capped column on wide terminals.
	if pad := (m.width - m.columnWidth()) / 2; pad > 0 {
		block = lipgloss.NewStyle().PaddingLeft(pad).Render(block)
	}
	return block
}

func (m *tuiModel) headerView() string {
	title := headerTitleStyle.Render(" ✦ valen")
	model := m.cfg.Model
	if m.cfg.Thinking != "" {
		model += " · " + m.cfg.Thinking
	}
	// Suffixes after the session label: plan marker and the overlay name.
	// They are reserved when truncating the label so the header never wraps
	// onto a second row (which pushes the whole screen down one line).
	var tail string
	if m.cfg.Plan {
		tail += "  ·  plan"
	}
	switch m.mode {
	case viewHistory:
		tail += "  ·  history"
	case viewSessions:
		tail += "  ·  sessions"
	case viewTools:
		tail += "  ·  tools"
	case viewErrors:
		tail += "  ·  errors"
	case viewHelp:
		tail += "  ·  help"
	case viewModels:
		tail += "  ·  models"
	case viewEffort:
		tail += "  ·  thinking"
	case viewDetail:
		tail += "  ·  " + m.detailTitle
	case viewAsk:
		tail += "  ·  question"
	}
	label := sessionLabel(m.session)
	var meta string
	if label != "" {
		if m.width > 0 {
			reserved := lipgloss.Width(" ✦ valen") + lipgloss.Width(fmt.Sprintf("  ·  %s  ·  ", model)) + lipgloss.Width("  ·  alt+m select") + lipgloss.Width(tail)
			// truncateRunes appends "…" past n, so leave one cell for it.
			label = truncateRunes(label, m.columnWidth()-reserved-1)
		}
		meta = headerDimStyle.Render(fmt.Sprintf("  ·  %s  ·  %s  ·  alt+m select", model, label))
	} else {
		meta = headerDimStyle.Render(fmt.Sprintf("  ·  %s  ·  alt+m select", model))
	}
	line := title + meta + headerDimStyle.Render(tail)
	if m.width > 0 {
		// One row, always: clip instead of wrapping.
		w := m.columnWidth()
		return lipgloss.NewStyle().Inline(true).MaxWidth(w).Render(line)
	}
	return line
}

func sessionLabel(s *Session) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(s.Title)
}

// fk renders token counts in thousands for the status row: 999 stays raw,
// 275654 becomes 276k.
func fk(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%dk", (n+500)/1000)
}

// parseRoundProgress reads "Round %d/%d" loop-event text for the status row.
func parseRoundProgress(s string) (cur, max int, ok bool) {
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "Round %d/%d", &cur, &max); err != nil || cur <= 0 || max <= 0 {
		return 0, 0, false
	}
	return cur, max, true
}

func (m *tuiModel) statusView() string {
	var left string
	if m.busy {
		elapsed := m.turnElapsed().Round(time.Second)
		tool := m.toolStatus
		if tool == "" {
			tool = "Working…"
		}
		prog := ""
		if m.roundCur > 0 && m.roundMax > 0 {
			prog = fmt.Sprintf(" · round %d/%d", m.roundCur, m.roundMax)
		}
		left = statusBusyStyle.Render(fmt.Sprintf("%s working  %s%s  %s", m.spinner.View(), tool, prog, elapsed))
	} else {
		left = "● ready"
		if m.lastTurnDur > 0 {
			left += " · " + formatThinkDur(m.lastTurnDur)
		}
		left = statusIdleStyle.Render(left)
	}
	var mid string
	if m.notice != "" {
		if m.noticeErr {
			mid = "  " + errNoticeStyle.Render(m.notice)
		} else {
			mid = "  " + noticeStyle.Render(m.notice)
		}
	} else if m.session != nil {
		mid = "  " + metaStyle.Render(fmt.Sprintf("%d turns · %s/%s · ↑%s ↓%s",
			len(m.session.Turns), fk(estimateTokens(m.history)), fk(contextBudget(m.cfg)),
			fk(m.session.TotalUsage.Prompt), fk(m.session.TotalUsage.Completion)))
	}
	right := ""
	if m.showMeta {
		right = metaStyle.Render(fmt.Sprintf("  ·  %s %s", m.cfg.Model, providerBaseURL(m.cfg)))
	}
	if m.busy {
		// Cancel hint lives here, pinned to the right edge directly above
		// the chatbox. The scrollback no longer repeats the status line,
		// so the spinner row above is the single source of live state.
		esc := metaStyle.Render("esc esc to stop")
		if m.width > 0 {
			avail := m.columnWidth() - 4
			used := lipgloss.Width(left) + lipgloss.Width(mid) + lipgloss.Width(right) + lipgloss.Width(esc)
			if used+2 <= avail {
				esc = strings.Repeat(" ", avail-used) + esc
			} else {
				esc = "  " + esc
			}
		} else {
			esc = "  " + esc
		}
		right += esc
	}
	row := lipgloss.NewStyle().Padding(0, 2).Render(left + mid + right)
	if m.width > 0 {
		// One row, always: a long notice (e.g. the plan-mode switch text)
		// must clip rather than wrap and push the layout down a line.
		row = lipgloss.NewStyle().Inline(true).MaxWidth(m.columnWidth()).Render(row)
	}
	return row
}

// inputBorderColor: orange while working, green when ready (cyan in plan
// mode), dim outside the chat view.
func (m *tuiModel) inputBorderColor() lipgloss.Color {
	switch {
	case m.busy:
		return mkOrange
	case m.mode != viewChat:
		return mkBorder
	case m.cfg.Plan:
		return mkCyan
	default:
		return mkGreen
	}
}

func (m *tuiModel) inputView() string {
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(m.inputBorderColor()).Padding(0, 1)
	if m.width > 0 {
		style = style.Width(m.columnWidth() - 2)
	}
	return style.Render(m.input.View())
}

func (m *tuiModel) footerView() string {
	hint := "/help for keys and commands"
	// The Go plan quota only applies to Go models: on openrouter/… models it
	// stays hidden and the footer shows the OpenRouter balance instead (when
	// the credits fetch has landed — plain API keys 403, so the footer stays
	// clean rather than erroring).
	if modelProvider(m.cfg.Model) == ProviderOpenRouter {
		if m.orCredits == nil {
			return footerStyle.Padding(0, 2).Render(hint)
		}
		text := m.orCredits.footerText()
		avail := 0
		if m.width > 0 {
			avail = m.columnWidth() - 4
			if lipgloss.Width(hint)+lipgloss.Width(text)+5 > avail {
				return footerStyle.Padding(0, 2).Render(hint)
			}
		}
		line := hint
		if avail > 0 {
			line += strings.Repeat(" ", avail-lipgloss.Width(hint)-lipgloss.Width(text)) + footerStyle.Render(text)
		} else {
			line += "  " + footerStyle.Render(text)
		}
		return footerStyle.Padding(0, 2).Render(line)
	}
	if m.goUsage == nil || modelProvider(m.cfg.Model) == ProviderZen {
		return footerStyle.Padding(0, 2).Render(hint)
	}
	text := m.goUsage.footerText(time.Now())
	avail := 0
	if m.width > 0 {
		avail = m.columnWidth() - 4
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
		m.chatCache, m.chatCacheTurns = nil, 0
		if m.pendingPrompt == "" {
			return emptyStyle.Render("valen " + version)
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

// committedTurns renders the immutable committed turns from the append-only
// per-turn cache. Delta flushes during streaming reuse the cache, so per-delta
// cost is the budgeted assembly (bounded by max_lines) plus the live pending
// block — not total history depth.
func (m *tuiModel) committedTurns(w int) string {
	if m.session == nil {
		return ""
	}
	n := len(m.session.Turns)
	if m.chatCacheW != w || m.chatCacheThink != m.showThink || m.chatCacheModel != m.cfg.Model || m.chatCacheTurns > n {
		m.chatCache, m.chatCacheTurns = nil, 0
	}
	for m.chatCacheTurns < n {
		full := m.renderTurn(&m.session.Turns[m.chatCacheTurns], w)
		m.chatCache = append(m.chatCache, chatTurnRender{full: full, fullN: lineCount(full)})
		m.chatCacheTurns++
	}
	m.chatCacheW, m.chatCacheThink, m.chatCacheModel = w, m.showThink, m.cfg.Model
	return m.assembleCommitted(tuiMaxLines(m.cfg))
}

// lineCount is the rendered line count of one turn, used to budget the
// scrollback without splitting the styled string back into lines.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// ensureCompact lazily builds (and caches) a turn's compact scrollback form.
func (m *tuiModel) ensureCompact(i int) {
	p := &m.chatCache[i]
	if p.compactReady {
		return
	}
	p.compact = m.renderTurnCompact(&m.session.Turns[i], m.chatCacheW)
	p.compactN = lineCount(p.compact)
	p.compactReady = true
}

// assembleCommitted joins cached turns under max_lines, newest first. The
// newest turn always survives (full when it fits, else compact); older turns
// stay full while they fit, then collapse to their compact form, then drop.
// A leading notice marks dropped turns. max_lines <= 0 means unlimited.
func (m *tuiModel) assembleCommitted(maxLines int) string {
	n := len(m.chatCache)
	if n == 0 {
		return ""
	}
	parts := m.chatCache
	// Unlimited budget: everything renders full.
	fullFrom, start, used := 0, 0, 0
	if maxLines > 0 {
		newest := n - 1
		if parts[newest].fullN <= maxLines {
			fullFrom = newest
			used = parts[newest].fullN
			for fullFrom > 0 && used+parts[fullFrom-1].fullN <= maxLines {
				fullFrom--
				used += parts[fullFrom].fullN
			}
			start = fullFrom
		} else {
			// A single huge turn: collapse it rather than hand the viewport
			// thousands of lines. fullFrom stays n, so every turn is compact.
			m.ensureCompact(newest)
			fullFrom = n
			start = newest
			used = parts[newest].compactN
		}
		// Older turns join compact while the budget allows.
		for start > 0 {
			m.ensureCompact(start - 1)
			l := parts[start-1].compactN
			if used+l > maxLines {
				break
			}
			start--
			used += l
		}
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString(metaStyle.Render(fmt.Sprintf("… %d earlier turn(s) trimmed — ctrl+h for history", start)))
		b.WriteString("\n\n\n")
	}
	for i := start; i < n; i++ {
		if i > start {
			b.WriteString("\n\n\n")
		}
		if i >= fullFrom {
			b.WriteString(parts[i].full)
		} else {
			b.WriteString(parts[i].compact)
		}
	}
	return b.String()
}

// thinkDurSuffix renders the persisted thinking timestamp, e.g. " (1m3s)".
// Empty when unknown so pre-existing sessions render unchanged.
func thinkDurSuffix(durMs int64) string {
	if durMs <= 0 {
		return ""
	}
	return " (" + formatThinkDur(time.Duration(durMs)*time.Millisecond) + ")"
}

// toolDurSuffix renders per-tool execution time, e.g. " (342ms)", " (1.5s)".
// Empty when unknown so pre-existing sessions render unchanged.
func toolDurSuffix(durMs int64) string {
	if durMs <= 0 {
		return ""
	}
	return " (" + formatToolDur(time.Duration(durMs)*time.Millisecond) + ")"
}

// formatToolDur keeps ms precision under a second and one decimal under a
// minute so short tools still show useful times; longer ones match think.
func formatToolDur(d time.Duration) string {
	return formatThinkDur(d)
}

// formatThinkDur keeps ms precision under a second so short reasoning
// bursts and fast tools show useful times; longer ones round to seconds
// so the header reads 12s / 1m3s, matching the live status elapsed.
func formatThinkDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		s := float64(d) / float64(time.Second)
		if s >= 10 {
			return fmt.Sprintf("%.0fs", s)
		}
		return fmt.Sprintf("%.1fs", s)
	}
	return d.Round(time.Second).String()
}

// liveThinkSuffix is the in-flight elapsed for the pending thinking header;
// empty when the turn start is unknown.
func (m *tuiModel) liveThinkSuffix() string {
	if m.pendingSince.IsZero() {
		return ""
	}
	return " (" + formatThinkDur(m.turnElapsed()) + ")"
}

func (m *tuiModel) renderTurn(t *Turn, w int) string {
	return m.renderTurnMode(t, w, false)
}

// renderTurnCompact is the scrollback form used once the transcript is over
// the max_lines budget: reasoning and reply bodies collapse to a bounded head.
// The user prompt and tool lines (the priority content) render unchanged, so
// trimming only ever removes the in-between reasoning, never what the user
// said or what the agent ran.
func (m *tuiModel) renderTurnCompact(t *Turn, w int) string {
	return m.renderTurnMode(t, w, true)
}

// userHead renders the "You" header for a turn, suffixed "(PLAN)" and tinted
// cyan when the prompt was sent in plan mode, so a mixed scrollback reads at
// a glance which mode each message belongs to.
func userHead(plan bool, when string) string {
	label, style := "You", userHeaderStyle
	if plan {
		label, style = "You (PLAN)", userHeaderPlanStyle
	}
	return style.Render(label) + "  " + metaStyle.Render(when)
}

func userBody(plan bool) lipgloss.Style {
	if plan {
		return userBodyPlanStyle
	}
	return userBodyStyle
}

func (m *tuiModel) renderTurnMode(t *Turn, w int, compact bool) string {
	var parts []string
	// bodyW is the style width; textW leaves room for the left border plus
	// padding so force-broken words still fit inside the chrome.
	bodyW := min(w-4, 120)
	textW := bodyW - 2

	head := userHead(t.Plan, t.At.Format("15:04"))
	body := userBody(t.Plan).Width(bodyW).Render(breakLongWords(t.Prompt, textW))
	parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, head, "", body))

	if t.Thinking != "" {
		switch {
		case !m.showThink:
			label := "✦ thinking" + thinkDurSuffix(t.DurMs)
			if t.DurMs > 0 {
				label = fmt.Sprintf("%s · %d chars — ctrl+y to expand", label, len(t.Thinking))
			} else {
				label = fmt.Sprintf("%s (%d chars) — ctrl+y to expand", label, len(t.Thinking))
			}
			parts = append(parts, thinkHeaderStyle.Render(label))
		case compact:
			label := fmt.Sprintf("✦ thinking%s · %d chars trimmed — ctrl+y to expand", thinkDurSuffix(t.DurMs), len(t.Thinking))
			parts = append(parts, thinkHeaderStyle.Render(label))
		default:
			thHead := thinkHeaderStyle.Render("✦ thinking" + thinkDurSuffix(t.DurMs))
			thBody := thinkBodyStyle.Width(bodyW).Render(breakLongWords(t.Thinking, textW))
			parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, thHead, thBody))
		}
	} else if m.showThink && t.DurMs > 0 && len(t.Tools) > 0 {
		// Tool-only turn: no thinking text, but the wall time still lands
		// on a bare thinking header so the elapsed persists in scrollback.
		// The "\n\n" join below keeps a line return after it.
		parts = append(parts, thinkHeaderStyle.Render("✦ thinking"+thinkDurSuffix(t.DurMs)))
	}

	for _, tr := range t.Tools {
		parts = append(parts, m.renderToolLine(tr))
	}

	if t.Reply != "" {
		reply := t.Reply
		if compact {
			reply = compactReply(reply)
		}
		aHead := asstHeadStyle.Render(" ✦ " + m.cfg.Model)
		aBody := asstBodyStyle.Width(bodyW).Render(breakLongWords(m.renderMarkdown(reply), bodyW-1))
		parts = append(parts, lipgloss.JoinVertical(lipgloss.Left, aHead, "", aBody))
	}

	if t.Error != nil {
		parts = append(parts, errorTitleSty.Width(min(w-4, 120)).Render(breakLongWords("✗ "+t.Error.Title, min(w-6, 118)))+"\n"+metaStyle.Render("  ctrl+x, enter for details"))
	}

	return strings.Join(parts, "\n\n")
}

// renderTurnFull always expands thinking and tool output for recall views.
// Content is wrapped to the transcript width so detail views never run
// off-screen.
func (m *tuiModel) renderTurnFull(t *Turn) string {
	var b strings.Builder
	who := "You"
	if t.Plan {
		who = "You (PLAN)"
	}
	b.WriteString(m.wrapPlain(who+" · "+t.At.Format("15:04:05")+"\n"+t.Prompt) + "\n\n")
	if t.Thinking != "" {
		b.WriteString(m.wrapPlain("✦ thinking"+thinkDurSuffix(t.DurMs)+"\n"+t.Thinking) + "\n\n")
	}
	for _, tr := range t.Tools {
		head := fmt.Sprintf("%s  %s%s", tr.Name, toolSummary(tr.Name, tr.Args, 300), toolDurSuffix(tr.DurMs))
		var body strings.Builder
		body.WriteString(head)
		if tr.Name == "edit" {
			if del, add, ok := editDiffLines(tr.Args, 300); ok {
				for _, ln := range del {
					body.WriteString("\n" + ln)
				}
				for _, ln := range add {
					body.WriteString("\n" + ln)
				}
				// The "edited …" confirmation adds nothing after the diff.
				if isEditConfirmation(tr.Output) {
					b.WriteString(m.wrapPlain(body.String()) + "\n\n")
					continue
				}
			}
		} else if tr.Name == "write" {
			if lines, ok := writeDiffLines(tr.Args, 300); ok {
				for _, ln := range lines {
					body.WriteString("\n" + ln)
				}
				if isEditConfirmation(tr.Output) {
					b.WriteString(m.wrapPlain(body.String()) + "\n\n")
					continue
				}
			}
		} else if tr.Name == "todowrite" || tr.Name == "todoread" {
			if lines, ok := todoChecklistLines(tr); ok {
				for _, ln := range lines {
					body.WriteString("\n" + ln)
				}
				b.WriteString(m.wrapPlain(body.String()) + "\n\n")
				continue
			}
		}
		if strings.TrimSpace(tr.Output) != "" {
			body.WriteString("\n" + tr.Output)
		}
		if tr.Err != "" {
			body.WriteString("\nerror: " + tr.Err)
		}
		b.WriteString(m.wrapPlain(body.String()) + "\n\n")
	}
	if t.Error != nil {
		b.WriteString(m.wrapPlain("✗ "+t.Error.Title+"\n"+t.Error.Details) + "\n\n")
	}
	if t.Reply != "" {
		b.WriteString(m.wrapPlain(t.Reply) + "\n")
	}
	return b.String()
}

// maxEditDiffLines caps the edit/write diff preview so one change never
// floods the transcript; the tail counts the remainder. 16 = 8 del + 8 add.
const maxEditDiffLines = 16

// displayPath renders a tool-call path for compact headers, preferring the
// workdir-local form over a full path when the model passed an absolute path
// inside the root (`read /work/foo.go` reads as `read foo.go`). Paths outside
// the root, and already-relative paths, pass through unchanged.
func displayPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) {
		return p
	}
	return relToRoot(p)
}

// toolSummary renders tool args in plain English for scrollback headers
// instead of raw JSON: paths, patterns and commands read as words.
// Unknown tools and unparseable args fall back to truncated raw args.
func toolSummary(name, args string, maxText int) string {
	short := func(s string) string { return truncateRunes(strings.TrimSpace(s), maxText) }
	switch name {
	case "read":
		var a struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Path) == "" {
			return short(args)
		}
		p := displayPath(a.Path)
		if a.Offset <= 1 && a.Limit <= 0 {
			return short(p)
		}
		start := a.Offset
		if start < 1 {
			start = 1
		}
		if a.Limit > 0 {
			return short(fmt.Sprintf("%s:%d-%d", p, start, start+a.Limit-1))
		}
		return short(fmt.Sprintf("%s:%d+", p, start))
	case "edit", "write":
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Path) == "" {
			return short(args)
		}
		return short(displayPath(a.Path))
	case "grep":
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
			Include string `json:"include"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Pattern) == "" {
			return short(args)
		}
		var b strings.Builder
		b.WriteString(short(`"` + strings.TrimSpace(a.Pattern) + `"`))
		if p := displayPath(a.Path); p != "" && p != "." {
			b.WriteString(" in " + short(p))
		}
		if inc := strings.TrimSpace(a.Include); inc != "" {
			b.WriteString(" · " + short(inc))
		}
		return truncateRunes(b.String(), maxText)
	case "glob":
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Pattern) == "" {
			return short(args)
		}
		s := strings.TrimSpace(a.Pattern)
		if p := displayPath(a.Path); p != "" && p != "." {
			s += " in " + p
		}
		return short(s)
	case "shell":
		var a struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Command) == "" {
			return short(args)
		}
		return short(a.Command)
	case "web_search":
		var a struct {
			Query string `json:"query"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Query) == "" {
			return short(args)
		}
		return short(a.Query)
	case "web_fetch":
		var a struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.URL) == "" {
			return short(args)
		}
		return short(a.URL)
	case "task":
		var a struct {
			Description string `json:"description"`
			Prompt      string `json:"prompt"`
		}
		if json.Unmarshal([]byte(args), &a) != nil {
			return short(args)
		}
		if d := strings.TrimSpace(a.Description); d != "" {
			return short(d)
		}
		if p := strings.TrimSpace(a.Prompt); p != "" {
			if i := strings.Index(p, "\n"); i >= 0 {
				p = p[:i]
			}
			return short(p)
		}
		return short(args)
	case "AskUserQuestion":
		var a struct {
			Questions []struct {
				Question string `json:"question"`
				Header   string `json:"header"`
			} `json:"questions"`
		}
		if json.Unmarshal([]byte(args), &a) != nil || len(a.Questions) == 0 {
			return short(args)
		}
		q := strings.TrimSpace(a.Questions[0].Question)
		if q == "" {
			q = strings.TrimSpace(a.Questions[0].Header)
		}
		if q == "" {
			return short(args)
		}
		return short(q)
	default:
		return short(args)
	}
}

// isEditConfirmation reports the stock edit/write success line, which the
// recall view drops once the diff itself is shown.
func isEditConfirmation(out string) bool {
	s := strings.TrimSpace(out)
	return strings.HasPrefix(s, "edited ") || strings.HasPrefix(s, "replaced ") || strings.HasPrefix(s, "wrote ")
}

// editDiffLines renders the oldString/newString pair from an edit call as
// -/+ diff lines, capped at maxEditDiffLines with a +N more tail.
func editDiffLines(args string, maxText int) (del, add []string, ok bool) {
	var a struct {
		OldString string `json:"oldString"`
		NewString string `json:"newString"`
	}
	if json.Unmarshal([]byte(args), &a) != nil || a.OldString == "" && a.NewString == "" {
		return nil, nil, false
	}
	w := maxText - 2
	if w < 10 {
		w = 10
	}
	for _, ln := range strings.Split(a.OldString, "\n") {
		del = append(del, "- "+truncateRunes(ln, w))
	}
	for _, ln := range strings.Split(a.NewString, "\n") {
		add = append(add, "+ "+truncateRunes(ln, w))
	}
	total := len(del) + len(add)
	if total <= maxEditDiffLines {
		return del, add, true
	}
	// Keep the head of each side; the tail counts what was cut.
	keepDel := len(del)
	keepAdd := len(add)
	for keepDel+keepAdd > maxEditDiffLines && (keepDel > 1 || keepAdd > 1) {
		if keepAdd >= keepDel && keepAdd > 1 {
			keepAdd--
		} else if keepDel > 1 {
			keepDel--
		} else {
			break
		}
	}
	del = del[:keepDel]
	add = add[:keepAdd]
	tail := fmt.Sprintf("… +%d more", total-keepDel-keepAdd)
	add = append(add, tail)
	return del, add, true
}

// writeDiffLines renders a write call's content as + diff lines, capped.
func writeDiffLines(args string, maxText int) ([]string, bool) {
	var a struct {
		Content string `json:"content"`
	}
	if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Content) == "" {
		return nil, false
	}
	w := maxText - 2
	if w < 10 {
		w = 10
	}
	var out []string
	lines := strings.Split(a.Content, "\n")
	for _, ln := range lines {
		out = append(out, "+ "+truncateRunes(ln, w))
		if len(out) >= maxEditDiffLines {
			if len(lines) > len(out) {
				out = append(out, fmt.Sprintf("… +%d more", len(lines)-len(out)))
			}
			break
		}
	}
	return out, true
}

func (m *tuiModel) renderToolLine(tr ToolRecord) string {
	cw := m.contentWidth()
	maxText := cw - 4
	if maxText < 20 {
		maxText = 20
	}
	// Session-plan calls render as a compact checklist instead of raw JSON
	// so todo progress reads like opencode's in the scrollback, both live
	// and committed. Anything unparseable falls through to the default row.
	if tr.Name == "todowrite" || tr.Name == "todoread" {
		if lines, ok := todoChecklistLines(tr); ok {
			out := toolLineStyle.Width(cw).Render(breakLongWords("TODO", cw-2))
			for _, ln := range lines {
				out += "\n" + toolDimStyle.Width(cw).Render(breakLongWords(ln, cw-2))
			}
			return out
		}
	}
	summary := toolSummary(tr.Name, tr.Args, maxText)
	label := fmt.Sprintf("%s  %s%s", tr.Name, summary, toolDurSuffix(tr.DurMs))
	// Tool styles carry PaddingLeft(2), so break two short of cw.
	if tr.Err != "" || strings.HasPrefix(strings.TrimSpace(tr.Output), "error:") {
		return toolErrStyle.Width(cw).Render(breakLongWords("✗ "+label, cw-2))
	}
	// Edits render the old/new pair as a -/+ diff instead of raw JSON.
	if tr.Name == "edit" {
		if del, add, ok := editDiffLines(tr.Args, maxText); ok {
			out := toolLineStyle.Width(cw).Render(breakLongWords(label, cw-2))
			for _, ln := range highlightDiffRowsCached(diffPath(tr.Args), del, cw) {
				out += "\n" + ln
			}
			for _, ln := range highlightDiffRowsCached(diffPath(tr.Args), add, cw) {
				out += "\n" + ln
			}
			return out
		}
	}
	if tr.Name == "write" {
		if lines, ok := writeDiffLines(tr.Args, maxText); ok {
			out := toolLineStyle.Width(cw).Render(breakLongWords(label, cw-2))
			for _, ln := range highlightDiffRowsCached(diffPath(tr.Args), lines, cw) {
				out += "\n" + ln
			}
			return out
		}
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
// yet) read "now: <action>" in yellow; the scrollback no longer redraws on
// every spinner tick, so an animated glyph there would just freeze mid-frame.
// Finished tools render the usual line plus preview.
func (m *tuiModel) renderPendingTool(t pendingTool) string {
	if t.done {
		return m.renderToolLine(ToolRecord{Name: t.name, Args: t.args, Output: t.output, DurMs: t.durMs})
	}
	cw := m.contentWidth()
	maxText := cw - 4
	if maxText < 20 {
		maxText = 20
	}
	// A running todowrite has no result yet, but its args already carry the
	// planned list — show the checklist under the running header.
	if t.name == "todowrite" || t.name == "todoread" {
		if lines, ok := formatTodoChecklist(t.args); ok {
			out := toolRunStyle.Width(cw).Render(breakLongWords("TODO", cw-2))
			for _, ln := range lines {
				out += "\n" + toolDimStyle.Width(cw).Render(breakLongWords(ln, cw-2))
			}
			return out
		}
	}
	summary := toolSummary(t.name, t.args, maxText)
	// A running edit already carries old/new strings — show the diff live.
	if t.name == "edit" {
		if del, add, ok := editDiffLines(t.args, maxText); ok {
			out := toolRunStyle.Width(cw).Render(breakLongWords(fmt.Sprintf("now: %s  %s", t.name, summary), cw-2))
			for _, ln := range highlightDiffRowsCached(diffPath(t.args), del, cw) {
				out += "\n" + ln
			}
			for _, ln := range highlightDiffRowsCached(diffPath(t.args), add, cw) {
				out += "\n" + ln
			}
			return out
		}
	}
	if t.name == "write" {
		if lines, ok := writeDiffLines(t.args, maxText); ok {
			out := toolRunStyle.Width(cw).Render(breakLongWords(fmt.Sprintf("now: %s  %s", t.name, summary), cw-2))
			for _, ln := range highlightDiffRowsCached(diffPath(t.args), lines, cw) {
				out += "\n" + ln
			}
			return out
		}
	}
	label := fmt.Sprintf("now: %s  %s", t.name, summary)
	// Tool styles carry PaddingLeft(2), so break two short of cw.
	return toolRunStyle.Width(cw).Render(breakLongWords(label, cw-2))
}

func (m *tuiModel) renderPending(w int) string {
	bodyW := min(w-4, 120)
	textW := bodyW - 2
	head := userHead(m.pendingPlan, "now")
	body := userBody(m.pendingPlan).Width(bodyW).Render(breakLongWords(m.pendingPrompt, textW))
	parts := []string{head, "", body, ""}

	// The thinking header renders from turn start (body follows once text
	// streams in) so the live elapsed is always visible while busy, even
	// across long tool-only stretches with no thinking deltas yet. The
	// trailing blank keeps a line return after the thinking state.
	// Models that stream <think> inside content would otherwise show their
	// reasoning as plain (white) answer text; route it to the thinking style.
	live, answer := m.pendingThinking, m.pendingText
	if strings.Contains(answer, "<think>") {
		t, a := splitThinking(answer)
		answer = a
		if t != "" {
			if live != "" {
				live += "\n"
			}
			live += t
		}
	}
	if m.showThink {
		parts = append(parts, thinkHeaderStyle.Render("✦ thinking"+m.liveThinkSuffix()))
		if live != "" {
			parts = append(parts, thinkBodyStyle.Width(bodyW).Render(breakLongWords(live, textW)))
		}
		parts = append(parts, "")
	}
	for _, t := range m.pendingTools {
		parts = append(parts, m.renderPendingTool(t))
	}
	if len(m.pendingTools) > 0 {
		parts = append(parts, "")
	}
	if answer != "" {
		parts = append(parts, asstHeadStyle.Render(" ✦ "+m.cfg.Model))
		parts = append(parts, "")
		parts = append(parts, asstBodyStyle.Width(bodyW).Render(breakLongWords(streamTail(answer), bodyW-1)))
		parts = append(parts, "")
	}

	// No trailing status line here: the spinner row above the chatbox already
	// carries the live tool/reasoning state, and the cancel hint sits at its
	// right edge. Repeating either in the scrollback doubles every update.
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// appendStreamText accumulates live deltas, capped so a very long stream
// stays cheap. The committed history message always keeps the full text.
func appendStreamText(s, delta string) string {
	const maxChars = 32768
	if len(delta) >= maxChars {
		s = strings.Clone(delta[len(delta)-maxChars:])
	} else {
		if len(s)+len(delta) > maxChars {
			s = s[len(s)+len(delta)-maxChars:]
		}
		s += delta
	}
	// A byte cap must not leave a partial UTF-8 rune at the start of the tail.
	for len(s) > 0 && s[0]&0xc0 == 0x80 {
		s = s[1:]
	}
	return s
}

// appendThinkingDelta accumulates one reasoning update into the live view.
// Deltas are arbitrary fragments, so fragments that share a part index
// concatenate exactly and the thought stays whole; only the first delta of
// a new part starts on its own line, mirroring the committed newline join
// of separate summary parts. cur tracks the part index of the tail.
func appendThinkingDelta(s, delta string, idx, cur int) (string, int) {
	if delta == "" {
		return s, cur
	}
	if s != "" && idx != cur && !strings.HasSuffix(s, "\n") && !strings.HasPrefix(delta, "\n") {
		s += "\n"
	}
	return appendStreamText(s, delta), idx
}

// roundBreak ends the text streamed so far with a blank line, so the next
// model round starts its own paragraph instead of gluing onto the previous
// round's last sentence. Empty text and text already ending in a blank
// line are left alone.
func roundBreak(s string) string {
	switch {
	case s == "", strings.HasSuffix(s, "\n\n"):
		return s
	case strings.HasSuffix(s, "\n"):
		return s + "\n"
	}
	return s + "\n\n"
}

// Stream chunks are arbitrary token fragments, not lines. Preserve whitespace
// and concatenate exactly; only the capped rolling tail is discarded.
func appendStreamLine(s, delta string) string {
	return appendStreamText(s, delta)
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

// compactReplyMaxLines caps the markdown head kept in a compacted turn. The
// tail marker tells the user the full reply is still in history (ctrl+h).
const compactReplyMaxLines = 8

// compactReply truncates a reply to its first compactReplyMaxLines lines for
// the budgeted scrollback. Short replies pass through untouched.
func compactReply(s string) string {
	total := strings.Count(s, "\n") + 1
	if total <= compactReplyMaxLines {
		return s
	}
	head := strings.Join(strings.Split(s, "\n")[:compactReplyMaxLines], "\n")
	return fmt.Sprintf("%s\n… %d more lines — ctrl+h for history", head, total-compactReplyMaxLines)
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
			b.WriteString(selectedStyle.Width(cw).Render(" "+line) + "\n")
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
			b.WriteString(selectedStyle.Width(cw).Render(" "+line) + "\n")
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
	if m.busy {
		m.notice = "Working... press esc twice to cancel before resuming."
		m.noticeErr = false
		return
	}
	turns, err := loadSession(path)
	if err != nil {
		m.notice = "Could not load session: " + err.Error()
		m.noticeErr = true
		return
	}
	m.session = &Session{Path: path, ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Turns: turns}
	m.cfg.SessionID = m.session.ID
	m.httpClient = newHTTPClient(m.cfg)
	m.apiKey = effectiveOpenCodeKey(m.cfg, m.apiKey)
	m.cfg.OpenRouterKey = effectiveOpenRouterKey(m.cfg)
	m.client = newClient(m.cfg, m.apiKey)
	for _, t := range turns {
		m.session.TotalUsage.Prompt += t.Usage.Prompt
		m.session.TotalUsage.Completion += t.Usage.Completion
		m.session.TotalUsage.Total += t.Usage.Total
	}
	m.history = seedHistory(m.cfg)
	start := 0
	for i := len(turns) - 1; i >= 0; i-- {
		if len(turns[i].History) > 0 {
			m.history = append([]openai.ChatCompletionMessage(nil), turns[i].History...)
			start = i + 1
			break
		}
	}
	for i := start; i < len(turns); i++ {
		t := turns[i]
		m.history = append(m.history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: t.Prompt})
		for j, tr := range t.Tools {
			id := tr.ID
			if id == "" {
				id = fmt.Sprintf("legacy-%d-%d", i, j)
			}
			m.history = append(m.history,
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: id, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: tr.Name, Arguments: tr.Args}}}},
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: tr.Output})
		}
		if t.Reply != "" {
			m.history = append(m.history, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: t.Reply})
		}
	}
	if len(m.history) > 0 && m.history[0].Role == openai.ChatMessageRoleSystem {
		m.history[0].Content = effectiveSystem(m.cfg)
	}
	restoreTodosFromTurns(turns)
	if meta, err := loadSessionMeta(metaPath(path)); err == nil {
		m.session.Title, m.session.Model, m.session.Effort = meta.Title, meta.Model, meta.Effort
	}
	m.pendingPrompt, m.pendingText, m.pendingThinking, m.toolStatus = "", "", "", ""
	m.pendingTools = nil
	m.cancel = nil
	m.chatCache, m.chatCacheTurns = nil, 0
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
			b.WriteString(selectedStyle.Width(cw).Render(" "+line) + "\n")
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
			b.WriteString(selectedStyle.Width(cw).Render(" "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.String()
}

func (m *tuiModel) renderModelsList() string {
	cw := m.contentWidth()
	// Fixed full-width search box: the outer border is pinned to the content
	// width so typing never resizes it. textinput.View() renders one cell
	// wider once non-empty (the cursor), so the input width leaves one cell
	// of slack for the typed state; lipgloss pads the rest.
	boxW := cw - 2
	if boxW < 14 {
		boxW = 14
	}
	tiW := boxW - 3
	if tiW < 12 {
		tiW = 12
	}
	m.modelSearch.Width = tiW
	searchBox := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(mkBorder).Padding(0, 1).Width(boxW).Render(m.modelSearch.View())

	var b strings.Builder
	b.WriteString(searchBox + "\n\n")
	b.WriteString(metaStyle.Render("Models — ↑/↓ select · enter switches · ctrl+f favourites · esc clears, go back") + "\n\n")
	if len(m.modelChoices) == 0 {
		query := strings.TrimSpace(m.modelFilter)
		if query == "" {
			b.WriteString(emptyStyle.Render("No models to show."))
		} else {
			b.WriteString(emptyStyle.Render(fmt.Sprintf("No models match %q.", query)))
		}
		return b.String()
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.modelChoices) {
		m.cursor = len(m.modelChoices) - 1
	}
	for i, c := range m.modelChoices {
		star := " "
		starCell := " "
		if m.isFavourite(c.id) {
			star = "★"
			starCell = favStyle.Render("★")
		}
		line := displayModelID(c.id)
		if c.current {
			line += "  ● current"
		}
		if c.transport != "chat" {
			line += fmt.Sprintf("  [/%s]", c.transport)
		}
		line = truncateRunes(line, cw-6)
		if i == m.cursor {
			b.WriteString(selectedStyle.Width(cw).Render(fmt.Sprintf(" %2d %s %s", i+1, star, line)) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %2d %s %s\n", i+1, starCell, boldModelName(c.id, line)))
		}
	}
	b.WriteString("\n" + metaStyle.Render("★ favourites stay on top. Transports are marked: /chat, /responses and /messages all converse."))
	return b.String()
}

// boldModelName renders the model name inside a picker row bright and bold,
// leaving the provider prefix and trailing markers (● current, transport) in
// the terminal default. line must already be truncated: truncateRunes counts
// ANSI escapes, so the style is applied after the fact.
func boldModelName(id, line string) string {
	full := displayModelID(id)
	prefix := ""
	if i := strings.LastIndex(full, "/"); i >= 0 {
		prefix = full[:i+1]
	}
	if !strings.HasPrefix(line, prefix) {
		return line
	}
	rest := line[len(prefix):]
	end := strings.Index(rest, "  ")
	if end < 0 {
		end = len(rest)
	}
	if end == 0 {
		return line
	}
	return prefix + modelNameStyle.Render(rest[:end]) + rest[end:]
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
			b.WriteString(selectedStyle.Width(cw).Render(fmt.Sprintf(" %2d  %s", i+1, line)) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("  %2d  %s\n", i+1, line))
		}
	}
	return b.String()
}

func (m *tuiModel) renderHelp() string {
	return emptyStyle.Width(m.contentWidth()).Render(`Conversation first: your message shows instantly, the reply streams into the same thread.

Keys
  enter send · tab swaps work/plan mode (cyan box = plan) · shift+enter/alt+enter/ctrl+j newline · esc cancel/back · pgup/pgdn/wheel scroll
  wheel/pgup pauses autoscroll until you reach the bottom again
   shift+drag (opt+drag in iTerm/Terminal) selects + copies natively · alt+m toggles plain-drag selection (wheel paused while off)
  ctrl+g help · ctrl+r history · ctrl+l sessions · ctrl+u tools · ctrl+x errors · ctrl+y thinking display
  ctrl+o details · ctrl+p/ctrl+m model · ctrl+n new session · ctrl+t thinking level (cycles low→high, wraps) · ctrl+s save · ctrl+q quit · ctrl+c/ctrl+d cancel · clear input · quit
  ↑ on an empty chatbox reloads your last prompt: edit it and enter resends from there
  in lists: ↑↓ move · enter open · esc back
  /model picker: type to filter · ↑↓ select · enter switches · ctrl+f stars/unstars favourites (kept on top) · esc clears search, then backs

Modes
   WORK (green chatbox) — default: the agent can read, edit, write and run commands.
   PLAN (cyan chatbox) — read-only: the agent investigates and replies with a plan; write/edit/shell/MCP are blocked, task is explore-only.
   Switch with tab or /plan (or start with -plan). Scrollback labels each message "You (PLAN)" in cyan or plain "You" in orange.

Commands
   /help /history /sessions (/resume) /tools /errors /think /save /clear /new /plan /quit (or :q)
   /compact (/summarize) — prune + summarize older history with the cheap model; disk log untouched
   /plan (or tab) — toggle plan mode: read-only investigation, the reply is the plan (write/edit/shell/MCP blocked, task explore-only)
   /model [filter] — list models with provider prefixes (go/…, zen/…, openrouter/…) and switch (ctrl+p/ctrl+m does the same); missing keys report on pick/send, never block boot
  /effort [level] — thinking level for the current model (/thinking is an alias)

When the model calls AskUserQuestion: ↑↓/jk move · enter select · space toggles multi-select · type your own when needed · enter confirms.

Go plan usage (5h rolling / 7d / 30d) shows in the footer, refreshed each
minute and after every turn: dim normally, yellow past 80%, pink at the cap.

Models persist to ~/.config/valen/state.json, so the last switch sticks across runs.
Favourites live beside it in ~/.config/valen/favourites.yaml (a plain list) and
show with a ★ at the top of the picker; ctrl+f toggles them there.
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

func runTUI(ctx context.Context, cfg Config, statePath string, client *openai.Client, registry *Registry, apiKey string) error {
	session, err := newSession(cfg)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	resetTodos()
	m := newTUIModel(cfg, client, registry, session)
	m.ctx = ctx
	m.statePath = statePath
	m.apiKey = effectiveOpenCodeKey(cfg, apiKey)
	m.cfg.OpenRouterKey = effectiveOpenRouterKey(cfg)
	if m.apiKey == "" && m.client == nil {
		m.client = newClient(m.cfg, "")
	}
	// Keyless boots still open: explain immediately which key the
	// configured model needs so /model has context before first send.
	if err := missingKeyError(m.cfg, m.apiKey); err != nil {
		if modelProvider(m.cfg.Model) == ProviderOpenRouter {
			m.notice = fmt.Sprintf("OpenRouter key not found — export %s=... to use %s. /model still lists both providers.", openRouterKeyEnv, m.cfg.Model)
		} else {
			m.notice = fmt.Sprintf("OpenCode API key not found — set %s=... to use %s. /model still lists both providers.", openCodeKeyEnv, m.cfg.Model)
		}
		m.noticeErr = true
		m.refreshViewport()
	}
	m.httpClient = newHTTPClient(cfg)
	askPrompter = m.askPrompt
	// Kitty progressive enhancement (disambiguate only): shift+enter then
	// arrives as CSI 13;2u instead of plain enter (decoded in Update), while
	// unmodified enter still sends CR. The keyboard-mode stack is per screen,
	// so it must be pushed AFTER bubbletea enters the alt screen (Init runs
	// post-entry) and popped before it leaves (QuitMsg filter). Terminals
	// without support ignore the private CSI. runTUI only runs on a TTY.
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(),
		tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			if _, ok := msg.(tea.QuitMsg); ok {
				fmt.Fprint(os.Stdout, kittyKeysPop)
			}
			return msg
		}))
	m.prog = p
	_, err = p.Run()
	return err
}
