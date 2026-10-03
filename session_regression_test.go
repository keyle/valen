package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func testConfig(url string) Config {
	return Config{Model: "m", BaseURL: url, APIKeyEnv: "TEST_KEY", SessionID: "ses-tui", MaxRounds: 4}
}

func newTestModel(t *testing.T, cfg Config) *tuiModel {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	session, err := newSession(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(cfg, newClient(cfg, "key"), NewRegistry(), session)
	m.ctx = context.Background()
	m.apiKey = "key"
	return m
}

func runSubmit(t *testing.T, m *tuiModel, prompt string) {
	t.Helper()
	cmd := m.submit(prompt)
	if cmd == nil {
		t.Fatalf("submit %q returned nil cmd (busy=%v notice=%q)", prompt, m.busy, m.notice)
	}
	_, _ = m.Update(cmd())
}

func TestTUISessionRecordsEventsAcrossCompaction(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("overflow=%v", recovery), func(t *testing.T) {
			requests, summaries := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req openai.ChatCompletionRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if req.Messages[0].Content == compactionSummarizerSystem {
					summaries++
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"handoff summary"}}]}`)
					return
				}
				requests++
				switch {
				case requests == 1:
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"reading both files","tool_calls":[{"id":"call-a","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}},{"id":"call-b","type":"function","function":{"name":"read","arguments":"{\"path\":\"b\"}"}}]}}]}`)
				case requests == 2 && recovery:
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"message":"context length exceeded"}}`)
				default:
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
				}
			}))
			defer srv.Close()
			cfg := testConfig(srv.URL)
			cfg.System = "sys"
			cfg.Compaction = normalizeCompactionConfig(CompactionConfig{KeepTokens: 100}, contextBudget(cfg))
			m := newTestModel(t, cfg)
			for i := 0; i < 20; i++ {
				m.history = append(m.history,
					openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("old question ", 50)},
					openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: strings.Repeat("old answer ", 50)})
			}
			before := len(m.history)
			output := func(args string) string {
				return "File " + args + " (100 lines, showing 1-100):\n" + strings.Repeat("1: source content\n", 100)
			}
			if err := m.registry.Add(Tool{Name: "read", Schema: json.RawMessage(`{"type":"object"}`), Fn: func(ctx context.Context, args string) (string, error) {
				return output(args), nil
			}}); err != nil {
				t.Fatal(err)
			}
			runSubmit(t, m, "read both")
			if summaries == 0 || len(m.history) >= before {
				t.Fatal("fixture did not shrink history across the old beforeLen boundary")
			}
			turn := m.session.Turns[0]
			if (!recovery && turn.Error != nil) || len(turn.Tools) != 2 {
				t.Fatalf("event records lost during compaction: error=%v tools=%d", turn.Error, len(turn.Tools))
			}
			for i, path := range []string{"a", "b"} {
				tr := turn.Tools[i]
				args := fmt.Sprintf(`{"path":%q}`, path)
				if tr.ID != "call-"+path || tr.Args != args || tr.Output != output(args) {
					t.Fatalf("same-name event mismatched or truncated: %+v", tr)
				}
			}
			want := append([]openai.ChatCompletionMessage(nil), m.history...)
			m.resumeSessionPath(m.session.Path)
			if !reflect.DeepEqual(m.history, want) || len(m.session.Turns[0].Tools) != 2 {
				t.Fatal("resume lost the compacted continuation or original tool records")
			}
		})
	}
}

func TestTUISessionFailureAndCancellationCheckpoint(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancel), func(t *testing.T) {
			defer resetTodos()
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"saving plan","tool_calls":[{"id":"plan","type":"function","function":{"name":"todowrite","arguments":"{\"todos\":[{\"content\":\"continue work\",\"status\":\"in_progress\",\"priority\":\"high\"}]}"}}]}}]}`)
					return
				}
				w.WriteHeader(http.StatusPaymentRequired)
				fmt.Fprint(w, `{"error":{"message":"insufficient account funds"}}`)
			}))
			defer srv.Close()
			m := newTestModel(t, testConfig(srv.URL))
			tool := TodoWriteTool()
			write := tool.Fn
			tool.Fn = func(ctx context.Context, args string) (string, error) {
				out, err := write(ctx, args)
				if cancel {
					m.cancel()
				}
				return out, err
			}
			if err := m.registry.Add(tool); err != nil {
				t.Fatal(err)
			}
			runSubmit(t, m, "plan then continue")
			turn := m.session.Turns[0]
			if turn.Error == nil || len(turn.Tools) != 1 || len(turn.History) != 4 {
				t.Fatalf("failed turn lost progress: %+v", turn)
			}
			want := append([]openai.ChatCompletionMessage(nil), m.history...)
			resetTodos()
			m.resumeSessionPath(m.session.Path)
			if !reflect.DeepEqual(m.history, want) || len(getTodos()) != 1 || getTodos()[0].Content != "continue work" {
				t.Fatal("resume lost failed/cancelled tool work or plan")
			}
		})
	}
}

func TestTUISessionResumesLatestCheckpointAndLegacyTools(t *testing.T) {
	defer resetTodos()
	var sessionHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionHeader = r.Header.Get("x-opencode-session")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"continued"}}]}`)
	}))
	defer srv.Close()
	m := newTestModel(t, testConfig(srv.URL))
	s, err := newSession(Config{SessionID: "ses-resumed"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := append(seedHistory(m.cfg),
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: compactionSummaryPrefix + "working summary", ReasoningContent: "retained reasoning"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "checkpoint prompt"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "checkpoint answer"})
	for _, turn := range []Turn{
		{Prompt: "obsolete checkpoint", History: seedHistory(m.cfg)},
		{Prompt: "checkpoint prompt", Reply: "checkpoint answer", History: checkpoint},
		{Prompt: "legacy follow-up", Tools: []ToolRecord{
			{ID: "ask-id", Name: "AskUserQuestion", Args: `{"questions":[]}`, Output: "User has answered your questions: color=blue"},
			{Name: "todowrite", Args: `{"todos":[]}`, Output: `[{"content":"keep plan","status":"pending","priority":"low"}]`},
		}, Reply: "legacy answer", Usage: Usage{Total: 7}},
	} {
		if err := s.append(turn); err != nil {
			t.Fatal(err)
		}
	}
	m.session.Turns = []Turn{{Prompt: "old session", Reply: "OLD CACHED ANSWER"}}
	_ = m.renderChat()
	m.busy = true
	m.resumeSessionPath(s.Path)
	if m.session.ID != "ses-tui" || m.cfg.SessionID != "ses-tui" || !strings.Contains(m.notice, "Working") {
		t.Fatal("busy resume replaced the active session")
	}
	m.busy = false
	oldClient, oldHTTP := m.client, m.httpClient
	m.resumeSessionPath(s.Path)
	if m.cfg.SessionID != s.ID || m.client == oldClient || m.httpClient == oldHTTP || m.session.TotalUsage.Total != 7 {
		t.Fatal("resume did not replace session attribution/clients/usage")
	}
	if len(m.history) != len(checkpoint)+6 || !reflect.DeepEqual(m.history[:len(checkpoint)], checkpoint) {
		t.Fatal("latest checkpoint was not restored exactly before legacy replay")
	}
	for _, i := range []int{5, 7} {
		call, result := m.history[i], m.history[i+1]
		if len(call.ToolCalls) != 1 || call.ToolCalls[0].ID == "" || result.Role != openai.ChatMessageRoleTool || result.ToolCallID != call.ToolCalls[0].ID {
			t.Fatalf("legacy tool pair invalid: %+v %+v", call, result)
		}
	}
	if m.history[6].Content != "User has answered your questions: color=blue" || len(getTodos()) != 1 || getTodos()[0].Content != "keep plan" {
		t.Fatal("legacy Ask answer or todo plan lost")
	}
	if strings.Contains(m.renderChat(), "OLD CACHED ANSWER") {
		t.Fatal("resume retained another session's rendered cache")
	}
	runSubmit(t, m, "continue")
	if sessionHeader != s.ID {
		t.Fatalf("resumed request attributed to %q, want %q", sessionHeader, s.ID)
	}
}
