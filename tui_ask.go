package main

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

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
		rows := overflow + 1
		if a.qs[a.idx].NoFreeText {
			rows = overflow // no own row: the cursor never reaches overflow
		}
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
				a.ownPos = 0
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
			m.dismissAsk(errAskDismissed)
		}
	case "own":
		a.clampOwnPos()
		runes := []rune(a.own)
		switch s {
		case "enter":
			if strings.TrimSpace(a.own) == "" {
				a.phase = "pick"
			} else {
				a.ownText[a.idx] = strings.TrimSpace(a.own)
				a.advanceAsk()
			}
		case "left":
			if a.ownPos > 0 {
				a.ownPos--
			}
		case "right":
			if a.ownPos < len(runes) {
				a.ownPos++
			}
		case "home", "ctrl+a":
			a.ownPos = 0
		case "end", "ctrl+e":
			a.ownPos = len(runes)
		case "backspace":
			if a.ownPos > 0 {
				a.own = string(append(runes[:a.ownPos-1], runes[a.ownPos:]...))
				a.ownPos--
			}
		case "delete":
			if a.ownPos < len(runes) {
				a.own = string(append(runes[:a.ownPos], runes[a.ownPos+1:]...))
			}
		case "esc":
			a.phase = "pick"
		default:
			if utf8.RuneCountInString(s) == 1 {
				for _, r := range s {
					a.insertOwnRune(r)
				}
			}
		}
	case "review":
		switch s {
		case "enter", "y":
			a.reply <- askReply{answers: m.askAnswers()}
			m.ask = nil
			m.resumeTimer()
			m.show(viewChat)
		case "esc":
			if a.isAccessOnly() {
				m.dismissAsk(errAskDismissed)
			} else {
				a.restartAsk()
			}
		case "e":
			if !a.isAccessOnly() {
				a.restartAsk()
			}
		case "n":
			m.dismissAsk(errAskDismissed)
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

// isAccessOnly reports whether every question in the batch is an access
// decision (NoFreeText). Model batches always allow free text, so they can
// be told apart this way without extra bookkeeping.
func (a *askState) isAccessOnly() bool {
	if len(a.qs) == 0 {
		return true
	}
	for _, q := range a.qs {
		if !q.NoFreeText {
			return false
		}
	}
	return true
}

// restartAsk drops all picks and free text and returns to the first
// question: esc at the confirmation stage means "I made a mistake", not
// "cancel the questions".
func (a *askState) restartAsk() {
	for i := range a.picks {
		a.picks[i] = map[int]bool{}
	}
	for i := range a.ownText {
		a.ownText[i] = ""
	}
	a.idx = 0
	a.cursor = 0
	a.phase = "pick"
	a.own = ""
	a.ownPos = 0
	a.rowTop, a.rowBottom = 0, 0
}

func (a *askState) clampOwnPos() {
	if n := len([]rune(a.own)); a.ownPos > n {
		a.ownPos = n
	}
	if a.ownPos < 0 {
		a.ownPos = 0
	}
}

func (a *askState) insertOwnRune(r rune) {
	runes := []rune(a.own)
	a.clampOwnPos()
	end := a.ownPos
	runes = append(runes, 0)
	copy(runes[end+1:], runes[end:])
	runes[end] = r
	a.own = string(runes)
	a.ownPos = end + 1
}

// condenseThinking keeps the last paragraph of the model's reasoning as the
// highlight above the questions: later reasoning explains the question,
// earlier paragraphs are stale context. Whitespace collapses to one line
// and very long paragraphs keep only their tail.
func condenseThinking(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	last := ""
	for _, p := range strings.Split(s, "\n\n") {
		if strings.TrimSpace(p) != "" {
			last = p
		}
	}
	last = strings.Join(strings.Fields(last), " ")
	const maxRunes = 500
	if r := []rune(last); len(r) > maxRunes {
		tail := string(r[len(r)-maxRunes:])
		if i := strings.Index(tail, " "); i >= 0 {
			tail = strings.TrimSpace(tail[i+1:])
		}
		last = "… " + tail
	}
	return last
}
