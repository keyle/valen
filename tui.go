package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
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
	// Subject of an access prompt (folder / command): bold yellow.
	accessSubjectStyle = lipgloss.NewStyle().Foreground(mkYellow).Bold(true)
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
// budget: the full render and a lazily-built compact one (reasoning collapsed)
// used when the transcript is over max_lines. Line counts
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
	orMeta   *ORMeta
	// turnCost is the total billed cost of every round in the turn (tool
	// loops issue several requests); hasTurnCost is false when no round
	// reported a cost, so free/absent pricing adds nothing to the session.
	turnCost    float64
	hasTurnCost bool
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
	// pricing carries OpenRouter catalog prices (per-token, keyed by stored
	// model ID) when the live catalog was fetched.
	pricing map[string]orPricing
	// refresh marks a background update of an already-open picker: swap the
	// catalog in place, keeping search text and cursor.
	refresh bool
}

// modelsRefreshMsg fires modelsRefreshDelay after the picker opened from the
// cache; the fetch only runs if that same picker is still showing.
type modelsRefreshMsg struct{ filter string }

// modelsRefreshDelay debounces the live fetch when the picker already has
// cached content, so a quick switch between known models never hits the net.
const modelsRefreshDelay = 5 * time.Second

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
	orMeta     *ORMeta              // last OpenRouter turn's serving metadata
	orPricing  map[string]orPricing // catalog per-token prices, keyed by stored model ID
	orSpent    float64              // cumulative billed cost across the session
	orSpentSet bool                 // a turn reported cost at least once

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
	retryErrs                []errEntry // retried provider failures, newest last (cap 50)
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
	slots           map[string]modelSlot // favourite slots, lazily read from slots.yaml
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
	search.Placeholder = "ctrl+f to filter models"
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
			m.modelSearch.Blur() // ctrl+f focuses it; j/k/space work until then
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
