package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/sashabaranov/go-openai"
)

const askFixtureArgs = `{"questions":[{"question":"Pick color?","header":"Color","options":[{"label":"Red"},{"label":"Blue"}]},{"question":"Pick fruit?","header":"Fruit","multiSelect":true,"options":[{"label":"Apple"},{"label":"Pear"}]}]}`

const askFixtureOutput = `User has answered your questions: "Pick color?"="Blue", "Pick fruit?"="Apple, Pear". You can now continue with the user's answers in mind.`

func stubAskPrompter(t *testing.T, answers []string, err error) {
	t.Helper()
	old := askPrompter
	askPrompter = func(context.Context, []AskQuestion) ([]string, error) { return answers, err }
	t.Cleanup(func() { askPrompter = old })
}

func askFixtureQuestions() []AskQuestion {
	return []AskQuestion{
		{Question: "Pick color?", Header: "Color", Options: []AskOption{{Label: "Red"}, {Label: "Blue"}}},
		{Question: "Pick fruit?", Header: "Fruit", MultiSelect: true, Options: []AskOption{{Label: "Apple"}, {Label: "Pear"}}},
	}
}

func TestAskUserQuestionFormatsMultipleAnswers(t *testing.T) {
	stubAskPrompter(t, []string{"Blue", "Apple, Pear"}, nil)
	registry := NewRegistry()
	if unknown := registry.LoadBuiltins([]string{"AskUserQuestion"}); len(unknown) != 0 {
		t.Fatalf("unknown tools: %v", unknown)
	}
	out, err := registry.Call(context.Background(), "AskUserQuestion", askFixtureArgs)
	if err != nil {
		t.Fatal(err)
	}
	if out != askFixtureOutput {
		t.Fatalf("out=%q", out)
	}
}

func TestAskUserQuestionValidatesArgsAndAnswers(t *testing.T) {
	registry := NewRegistry()
	registry.LoadBuiltins([]string{"AskUserQuestion"})
	if _, err := registry.Call(context.Background(), "AskUserQuestion", `{`); err == nil {
		t.Fatal("expected malformed args error")
	}
	if _, err := registry.Call(context.Background(), "AskUserQuestion", `{"questions":[]}`); err == nil {
		t.Fatal("expected empty questions error")
	}

	stubAskPrompter(t, []string{"Blue"}, nil)
	if _, err := registry.Call(context.Background(), "AskUserQuestion", askFixtureArgs); err == nil {
		t.Fatal("expected answer-count error")
	}
}

func TestHeadlessAskCollectsOptionsAndConfirms(t *testing.T) {
	var prompt bytes.Buffer
	answers, err := headlessAsk(context.Background(), askFixtureQuestions(), strings.NewReader("2\n1,2\ny\n"), &prompt)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 || answers[0] != "Blue" || answers[1] != "Apple, Pear" {
		t.Fatalf("answers=%q", answers)
	}
	if !strings.Contains(prompt.String(), "Review your answers:") {
		t.Fatalf("missing confirmation stage: %q", prompt.String())
	}
}

func TestHeadlessAskTypeOwnAndEditConfirmation(t *testing.T) {
	qs := []AskQuestion{{Question: "Pick color?", Options: []AskOption{{Label: "Red"}, {Label: "Blue"}}}}
	answers, err := headlessAsk(context.Background(), qs, strings.NewReader("2\nultraviolet\ne\n1\ny\n"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0] != "Red" {
		t.Fatalf("answers=%q", answers)
	}
}

func TestHeadlessAskRejectsMixedOwnSelection(t *testing.T) {
	qs := []AskQuestion{{Question: "Pick fruit?", MultiSelect: true, Options: []AskOption{{Label: "Apple"}, {Label: "Pear"}}}}
	_, err := headlessAsk(context.Background(), qs, strings.NewReader("1,3\n"), io.Discard)
	if err == nil {
		t.Fatal("expected EOF after rejecting mixed own selection")
	}
}

func TestAskOverlaySelectsTypeOwnAndConfirms(t *testing.T) {
	m := newTUIModel(Config{Model: "m"}, nil, nil, nil)
	reply := make(chan askReply, 1)
	picks := []map[int]bool{{}, {}}
	m.ask = &askState{qs: askFixtureQuestions(), picks: picks, ownText: make([]string, 2), phase: "pick", reply: reply}
	m.mode = viewAsk

	_, _ = m.handleAskKey("down")  // Blue
	_, _ = m.handleAskKey("enter") // answer question one
	_, _ = m.handleAskKey(" ")     // select Apple
	_, _ = m.handleAskKey("down")  // Pear
	_, _ = m.handleAskKey(" ")     // select Pear
	_, _ = m.handleAskKey("enter") // answer question two, show confirmation
	_, _ = m.handleAskKey("enter") // submit answers

	select {
	case r := <-reply:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if len(r.answers) != 2 || r.answers[0] != "Blue" || r.answers[1] != "Apple, Pear" {
			t.Fatalf("answers=%q", r.answers)
		}
	default:
		t.Fatal("expected submitted answers")
	}
	if m.ask != nil || m.mode != viewChat {
		t.Fatalf("overlay did not close: ask=%v mode=%v", m.ask != nil, m.mode)
	}
}

func TestAskUserQuestionChatLoop(t *testing.T) {
	stubAskPrompter(t, []string{"Blue", "Apple, Pear"}, nil)
	var bodies []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if calls == 0 {
			w.Write(completion(toolCall("AskUserQuestion", askFixtureArgs), ""))
		} else {
			w.Write(completion(nil, "done"))
		}
		calls++
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"AskUserQuestion"})
	cfg := newTestConfig(srv.URL)
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	reply, _, err := runChatTurn(context.Background(), cfg, nil, "key", srv.Client(), registry, &history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "done" || calls != 2 || len(bodies) != 2 {
		t.Fatalf("reply=%q calls=%d bodies=%d", reply.Content, calls, len(bodies))
	}
	if !strings.Contains(bodies[0], "AskUserQuestion") {
		t.Fatalf("first request did not offer the question tool: %s", bodies[0])
	}
	assertChatToolAnswer(t, bodies[1])
}

func assertChatToolAnswer(t *testing.T, body string) {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	msgs, _ := req["messages"].([]any)
	for _, item := range msgs {
		msg, _ := item.(map[string]any)
		if msg["role"] != "tool" {
			continue
		}
		if content, _ := msg["content"].(string); content == askFixtureOutput {
			return
		}
	}
	t.Fatalf("tool answer missing from chat history: %s", body)
}

func TestAskUserQuestionMessagesLoop(t *testing.T) {
	stubAskPrompter(t, []string{"Blue", "Apple, Pear"}, nil)
	var bodies []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if calls == 0 {
			w.Write(messagesToolResp("call-1", "AskUserQuestion", askFixtureArgs))
		} else {
			w.Write(messagesTextResp("done"))
		}
		calls++
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"AskUserQuestion"})
	cfg := messagesTestConfig(srv.URL)
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	reply, _, err := runMessagesTurn(context.Background(), cfg, nil, "key", srv.Client(), registry, &history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "done" || calls != 2 || len(bodies) != 2 {
		t.Fatalf("reply=%q calls=%d bodies=%d", reply.Content, calls, len(bodies))
	}
	if !strings.Contains(bodies[0], "AskUserQuestion") {
		t.Fatalf("first request did not offer the question tool: %s", bodies[0])
	}
	assertMessagesToolAnswer(t, bodies[1])
}

func assertMessagesToolAnswer(t *testing.T, body string) {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	msgs, _ := req["messages"].([]any)
	for _, item := range msgs {
		msg, _ := item.(map[string]any)
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			block, _ := b.(map[string]any)
			if block["type"] == "tool_result" {
				if content, _ := block["content"].(string); content == askFixtureOutput {
					return
				}
			}
		}
	}
	t.Fatalf("tool answer missing from messages input: %s", body)
}

func TestAskUserQuestionResponsesLoop(t *testing.T) {
	stubAskPrompter(t, []string{"Blue", "Apple, Pear"}, nil)
	var bodies []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if calls == 0 {
			w.Write(responsesToolResp("call-1", "AskUserQuestion", askFixtureArgs))
		} else {
			w.Write(responsesTextResp("done"))
		}
		calls++
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"AskUserQuestion"})
	cfg := responsesTestConfig(srv.URL)
	history := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	reply, _, err := runResponsesTurn(context.Background(), cfg, nil, "key", srv.Client(), registry, &history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "done" || calls != 2 || len(bodies) != 2 {
		t.Fatalf("reply=%q calls=%d bodies=%d", reply.Content, calls, len(bodies))
	}
	if !strings.Contains(bodies[0], "AskUserQuestion") {
		t.Fatalf("first request did not offer the question tool: %s", bodies[0])
	}
	assertResponsesToolAnswer(t, bodies[1])
}

func assertResponsesToolAnswer(t *testing.T, body string) {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	input, _ := req["input"].([]any)
	for _, item := range input {
		entry, _ := item.(map[string]any)
		if entry["type"] != "function_call_output" {
			continue
		}
		if output, _ := entry["output"].(string); output == askFixtureOutput {
			return
		}
	}
	t.Fatalf("tool answer missing from responses input: %s", body)
}

func TestAskNoFreeTextHidesOwnRow(t *testing.T) {
	qs := []AskQuestion{{Question: "Allow?", NoFreeText: true, Options: []AskOption{{Label: "Yes"}, {Label: "No"}}}}

	// Headless: no own row printed, number past the last option rejected.
	var out bytes.Buffer
	answers, err := headlessAsk(context.Background(), qs, strings.NewReader("3\n2\ny\n"), &out)
	if err != nil || len(answers) != 1 || answers[0] != "No" {
		t.Fatalf("answers=%q err=%v", answers, err)
	}
	if strings.Contains(out.String(), "Type your own") || !strings.Contains(out.String(), "enter number(s) 1–2") {
		t.Fatalf("own row leaked: %q", out.String())
	}

	// TUI: no own row rendered, cursor stops at the last option, enter answers.
	m := newTUIModel(Config{Model: "m"}, nil, nil, nil)
	reply := make(chan askReply, 1)
	m.ask = &askState{qs: qs, picks: []map[int]bool{{}}, ownText: make([]string, 1), phase: "pick", reply: reply}
	m.mode = viewAsk
	if strings.Contains(m.renderAsk(), "Type your own") {
		t.Fatalf("own row rendered: %q", m.renderAsk())
	}
	_, _ = m.handleAskKey("down")
	_, _ = m.handleAskKey("down") // clamped at No
	_, _ = m.handleAskKey("enter")
	if m.ask.phase != "review" {
		t.Fatalf("phase=%q, enter at last option must answer, not open own", m.ask.phase)
	}
	if got := m.askAnswers(); got[0] != "No" {
		t.Fatalf("answers=%q", got)
	}

	// Default questions keep the row.
	m.ask = &askState{qs: askFixtureQuestions()[:1], picks: []map[int]bool{{}}, ownText: make([]string, 1), phase: "pick", reply: reply}
	if !strings.Contains(m.renderAsk(), "Type your own") {
		t.Fatal("model question lost its own row")
	}
}

func TestAskHighlightSubject(t *testing.T) {
	qs := []AskQuestion{{Question: "Allow read?", Highlight: []string{"/tmp/secret-dir"}, NoFreeText: true, Options: []AskOption{{Label: "Yes"}}}}
	var out bytes.Buffer
	if _, err := headlessAsk(context.Background(), qs, strings.NewReader("1\ny\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\n  /tmp/secret-dir\n") {
		t.Fatalf("headless subject missing: %q", out.String())
	}
	m := newTUIModel(Config{Model: "m"}, nil, nil, nil)
	m.ask = &askState{qs: qs, picks: []map[int]bool{{}}, ownText: make([]string, 1), phase: "pick", reply: make(chan askReply, 1)}
	if got := ansi.Strip(m.renderAsk()); !strings.Contains(got, "\n/tmp/secret-dir") {
		t.Fatalf("TUI subject missing: %q", got)
	}
	if accessSubjectStyle.GetBold() != true || accessSubjectStyle.GetForeground() != mkYellow {
		t.Fatal("subject style must be bold yellow")
	}
}

func TestAskOwnCursorEditingKeepsOptionsVisible(t *testing.T) {
	m := newTUIModel(Config{Model: "m"}, nil, nil, nil)
	reply := make(chan askReply, 1)
	qs := []AskQuestion{{Question: "Pick color?", Options: []AskOption{{Label: "Red"}, {Label: "Blue"}}}}
	m.ask = &askState{qs: qs, picks: []map[int]bool{{}}, ownText: make([]string, 1), phase: "pick", reply: reply}
	m.mode = viewAsk

	_, _ = m.handleAskKey("down")
	_, _ = m.handleAskKey("down")
	_, _ = m.handleAskKey("enter") // enter the own box
	if m.ask.phase != "own" {
		t.Fatalf("phase=%q", m.ask.phase)
	}
	_, _ = m.handleAskKey("h")
	_, _ = m.handleAskKey("i")
	_, _ = m.handleAskKey("x")
	if m.ask.own != "hix" || m.ask.ownPos != 3 {
		t.Fatalf("own=%q pos=%d", m.ask.own, m.ask.ownPos)
	}
	_, _ = m.handleAskKey("left")
	_, _ = m.handleAskKey("left")
	_, _ = m.handleAskKey("!")
	if m.ask.own != "h!ix" || m.ask.ownPos != 2 {
		t.Fatalf("insert at cursor: own=%q pos=%d", m.ask.own, m.ask.ownPos)
	}
	_, _ = m.handleAskKey("backspace")
	if m.ask.own != "hix" || m.ask.ownPos != 1 {
		t.Fatalf("backspace: own=%q pos=%d", m.ask.own, m.ask.ownPos)
	}
	_, _ = m.handleAskKey("delete")
	if m.ask.own != "hx" || m.ask.ownPos != 1 {
		t.Fatalf("delete: own=%q pos=%d", m.ask.own, m.ask.ownPos)
	}
	_, _ = m.handleAskKey("home")
	if m.ask.ownPos != 0 {
		t.Fatalf("home pos=%d", m.ask.ownPos)
	}
	_, _ = m.handleAskKey("end")
	if m.ask.ownPos != 2 {
		t.Fatalf("end pos=%d", m.ask.ownPos)
	}
	// The options stay visible while the box is active, with a block cursor.
	got := ansi.Strip(m.renderAsk())
	if !strings.Contains(got, "Red") || !strings.Contains(got, "Blue") {
		t.Fatalf("options hidden while typing:\n%s", got)
	}
	if !strings.Contains(got, "hx▏") {
		t.Fatalf("cursor missing:\n%s", got)
	}
	// Multibyte runes edit by rune, not by byte.
	m.ask.own = "éé"
	m.ask.ownPos = 2
	_, _ = m.handleAskKey("left")
	_, _ = m.handleAskKey("a")
	if m.ask.own != "éaé" {
		t.Fatalf("rune insert: own=%q", m.ask.own)
	}
	_, _ = m.handleAskKey("esc")
	if m.ask.phase != "pick" {
		t.Fatalf("esc must go back to pick: %q", m.ask.phase)
	}
	_, _ = m.handleAskKey("down")
	_, _ = m.handleAskKey("down")
	_, _ = m.handleAskKey("enter")
	if m.ask.own != "" || m.ask.ownPos != 0 {
		t.Fatalf("re-entering must reset the box: %q %d", m.ask.own, m.ask.ownPos)
	}
}

func TestAskReviewEscRevisesModelButDismissesAccess(t *testing.T) {
	newModel := func(qs []AskQuestion) (*tuiModel, chan askReply) {
		m := newTUIModel(Config{Model: "m"}, nil, nil, nil)
		reply := make(chan askReply, 1)
		picks := make([]map[int]bool, len(qs))
		for i := range picks {
			picks[i] = map[int]bool{}
		}
		m.ask = &askState{qs: qs, picks: picks, ownText: make([]string, len(qs)), phase: "review", reply: reply}
		m.mode = viewAsk
		return m, reply
	}

	// Model batch: esc restarts from the first question, nothing is sent.
	m, reply := newModel(askFixtureQuestions())
	m.ask.picks[0][1] = true
	m.ask.picks[1][0] = true
	m.ask.ownText[0] = "ultraviolet"
	m.ask.idx = 1
	_, _ = m.handleAskKey("esc")
	if m.ask == nil || m.ask.phase != "pick" || m.ask.idx != 0 || m.ask.cursor != 0 {
		t.Fatalf("model esc must restart: %+v", m.ask)
	}
	if len(m.ask.picks[0]) != 0 || len(m.ask.picks[1]) != 0 || m.ask.ownText[0] != "" {
		t.Fatalf("restart must clear answers: picks=%v own=%q", m.ask.picks, m.ask.ownText)
	}
	select {
	case r := <-reply:
		t.Fatalf("restart must not send: %+v", r)
	default:
	}
	// "e" also revises, "n" still cancels a model batch.
	m.ask.phase = "review"
	_, _ = m.handleAskKey("e")
	if m.ask == nil || m.ask.phase != "pick" {
		t.Fatal("e must revise a model batch")
	}
	m.ask.phase = "review"
	_, _ = m.handleAskKey("n")
	if m.ask != nil {
		t.Fatal("n must cancel a model batch")
	}
	select {
	case r := <-reply:
		if r.err != errAskDismissed {
			t.Fatalf("n err=%v", r.err)
		}
	default:
		t.Fatal("n must send the dismissal")
	}

	// Access batch: esc and n still dismiss (downgrade path).
	qs := []AskQuestion{{Question: "Allow?", NoFreeText: true, Options: []AskOption{{Label: "Yes"}, {Label: "No"}}}}
	am, areply := newModel(qs)
	_, _ = am.handleAskKey("esc")
	if am.ask != nil {
		t.Fatal("access esc must dismiss")
	}
	select {
	case r := <-areply:
		if r.err != errAskDismissed {
			t.Fatalf("access esc err=%v", r.err)
		}
	default:
		t.Fatal("access esc must send the dismissal")
	}
	am2, _ := newModel(qs)
	_, _ = am2.handleAskKey("e")
	if am2.ask == nil {
		t.Fatal("e must not dismiss an access batch")
	}
}

func TestAskReviewHintsDistinguishReviseFromCancel(t *testing.T) {
	m := newTUIModel(Config{Model: "m"}, nil, nil, nil)
	m.ask = &askState{qs: askFixtureQuestions(), picks: []map[int]bool{{}, {}}, ownText: make([]string, 2), phase: "review"}
	m.mode = viewAsk
	if got := ansi.Strip(m.renderAsk()); !strings.Contains(got, "esc revises") {
		t.Fatalf("model review must hint revise:\n%s", got)
	}
	qs := []AskQuestion{{Question: "Allow?", NoFreeText: true, Options: []AskOption{{Label: "Yes"}}}}
	m.ask = &askState{qs: qs, picks: []map[int]bool{{}}, ownText: make([]string, 1), phase: "review"}
	if got := ansi.Strip(m.renderAsk()); !strings.Contains(got, "esc cancels") {
		t.Fatalf("access review must hint cancel:\n%s", got)
	}
}

func TestCondenseThinkingKeepsLastParagraph(t *testing.T) {
	if got := condenseThinking("stale first\n\n  current   second\nline  "); got != "current second line" {
		t.Fatalf("condensed=%q", got)
	}
	if got := condenseThinking(""); got != "" {
		t.Fatalf("empty condensed=%q", got)
	}
	long := strings.Repeat("word ", 400)
	got := condenseThinking(long)
	if !strings.HasPrefix(got, "… ") || len([]rune(got)) > 502 {
		t.Fatalf("long condensed=%q", got)
	}
}

func TestAskThinkingHighlightsAboveQuestion(t *testing.T) {
	m := newTUIModel(Config{Model: "m"}, nil, nil, nil)
	m.pendingThinking = "stale reasoning here\n\nthe choice follows from X because Y"
	_, _ = m.Update(questionMsg{qs: askFixtureQuestions(), reply: make(chan askReply, 1)})
	if m.ask.thinking != "the choice follows from X because Y" {
		t.Fatalf("thinking=%q", m.ask.thinking)
	}
	got := ansi.Strip(m.renderAsk())
	ti, qi := strings.Index(got, "the choice follows"), strings.Index(got, "Question 1 of 2")
	if ti < 0 || qi < 0 || ti > qi {
		t.Fatalf("thinking must sit above the question:\n%s", got)
	}
	if got := ansi.Strip(m.renderAsk()); strings.Contains(got, "stale reasoning") {
		t.Fatalf("stale paragraph leaked:\n%s", got)
	}
	if askThinkStyle.GetForeground() != mkFg || !askThinkStyle.GetBold() {
		t.Fatal("thinking highlights must render bright white bold")
	}
	// No reasoning: no highlight block at all.
	m2 := newTUIModel(Config{Model: "m"}, nil, nil, nil)
	_, _ = m2.Update(questionMsg{qs: askFixtureQuestions(), reply: make(chan askReply, 1)})
	if m2.ask.thinking != "" || strings.HasPrefix(ansi.Strip(m2.renderAsk()), "Question") == false {
		t.Fatalf("empty thinking must stay bare: %q", m2.ask.thinking)
	}
}
