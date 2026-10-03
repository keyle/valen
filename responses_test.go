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

	"github.com/sashabaranov/go-openai"
)

func responsesTextResp(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": "resp_1", "model": "m", "status": "completed",
		"output": []any{map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		}},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
	})
	return b
}

func responsesToolResp(id, name, args string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": "resp_1", "model": "m", "status": "completed",
		"output": []any{map[string]any{
			"type": "function_call", "id": "fc_" + id, "call_id": id, "name": name, "arguments": args,
		}},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
	})
	return b
}

func responsesTestConfig(baseURL string) Config {
	return Config{Model: "muse-spark-1.3-contributor", BaseURL: baseURL, SessionID: "ses-test", MaxRounds: 4}
}

// responsesTwoToolResp returns one response with two function_call items.
func responsesTwoToolResp() []byte {
	b, _ := json.Marshal(map[string]any{
		"id": "resp_1", "model": "m", "status": "completed",
		"output": []any{
			map[string]any{"type": "function_call", "id": "fc_call-1", "call_id": "call-1", "name": "shell", "arguments": `{"command":"echo hi"}`},
			map[string]any{"type": "function_call", "id": "fc_call-2", "call_id": "call-2", "name": "shell", "arguments": `{"command":"echo yo"}`},
		},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
	})
	return b
}

// TestResponsesParallelToolBatch proves a multi-call batch runs through the
// shared parallel dispatcher and both results fold back as ordered
// function_call_output items in the next round's stateless input.
func TestResponsesParallelToolBatch(t *testing.T) {
	var bodies []map[string]any
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		var out []byte
		if calls == 0 {
			out = responsesTwoToolResp()
		} else {
			out = responsesTextResp("all done")
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := responsesTestConfig(srv.URL)

	var stdout, stderr bytes.Buffer
	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), registry, "hi", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "all done") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if calls != 2 || len(bodies) != 2 {
		t.Fatalf("calls=%d bodies=%d", calls, len(bodies))
	}
	input, _ := bodies[1]["input"].([]any)
	var ids, outputs []string
	for _, item := range input {
		m, _ := item.(map[string]any)
		if m["type"] != "function_call_output" {
			continue
		}
		id, _ := m["call_id"].(string)
		ids = append(ids, id)
		out, _ := m["output"].(string)
		outputs = append(outputs, out)
	}
	if strings.Join(ids, ",") != "call-1,call-2" {
		t.Fatalf("function_call_output order=%v", ids)
	}
	if len(outputs) != 2 || !strings.Contains(outputs[0], "hi") || !strings.Contains(outputs[1], "yo") {
		t.Fatalf("function_call_output outputs=%v", outputs)
	}
}

func TestResponsesReasoningMapping(t *testing.T) {
	if responsesReasoning("") != nil || responsesReasoning("none") != nil {
		t.Fatal("empty and none must omit (Spark rejects none with 400)")
	}
	if got := responsesReasoning("high"); got["effort"] != "high" {
		t.Fatalf("high=%v", got)
	}
	if got := responsesReasoning("MAX"); got["effort"] != "max" {
		t.Fatalf("passthrough=%v", got)
	}
}

func TestSplitResponsesInput(t *testing.T) {
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "be brief"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		{Role: openai.ChatMessageRoleAssistant, Content: "checking", ToolCalls: []openai.ToolCall{
			{ID: "call-1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "shell", Arguments: `{"command":"ls"}`}},
		}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "call-1", Content: "main.go"},
	}
	instructions, input := splitResponsesInput(history)
	if instructions != "be brief" {
		t.Fatalf("instructions=%q", instructions)
	}
	if len(input) != 4 {
		t.Fatalf("input=%v", input)
	}
	commentary, _ := input[1].(map[string]any)
	if commentary["phase"] != "commentary" {
		t.Fatalf("assistant text before call must be commentary: %v", commentary)
	}
	call, _ := input[2].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call-1" {
		t.Fatalf("function call=%v", call)
	}
	out, _ := input[3].(map[string]any)
	if out["type"] != "function_call_output" || out["call_id"] != "call-1" {
		t.Fatalf("tool output=%v", out)
	}
}

// TestSplitResponsesInputSendBudget: long outputs shrink on the wire while
// stored history keeps the full text.
func TestSplitResponsesInputSendBudget(t *testing.T) {
	long := strings.Repeat("z", sendToolOutputChars+1000)
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleTool, ToolCallID: "call-1", Content: long},
	}
	_, input := splitResponsesInput(history)
	item, _ := input[0].(map[string]any)
	content, _ := item["output"].(string)
	if len([]rune(content)) > sendToolOutputChars+200 || !strings.Contains(content, "send-truncated") {
		t.Fatalf("wire copy must shrink: %d runes", len([]rune(content)))
	}
	if history[0].Content != long {
		t.Fatal("stored history must keep the full result")
	}
}

func TestResponsesReplyMapsSummary(t *testing.T) {
	resp := responsesResponse{}
	resp.Output = []responsesOutputItem{
		{Type: "reasoning", Summary: []responsesSummaryPart{{Type: "summary_text", Text: "plan"}}},
		{Type: "message", Content: []responsesContent{{Type: "output_text", Text: "done"}}},
	}
	msg, err := responsesReplyToOpenAI(resp)
	if err != nil {
		t.Fatal(err)
	}
	thinking, answer := splitThinking(msg.Content)
	if thinking != "plan" || answer != "done" {
		t.Fatalf("thinking=%q answer=%q", thinking, answer)
	}
}

func TestResponsesToolLoop(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var out []byte
		if calls == 0 {
			out = responsesToolResp("call-1", "shell", `{"command":"echo hi"}`)
		} else {
			out = responsesTextResp("all done")
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := responsesTestConfig(srv.URL)

	var stdout, stderr bytes.Buffer
	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), registry, "hi", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "all done") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestResponsesRequestShape(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotSession, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("User-Agent")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(responsesTextResp("ok"))
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := responsesTestConfig(srv.URL)
	cfg.System = "be brief"
	cfg.Thinking = "high"

	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), registry, "hi", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer key" || gotSession != "ses-test" || gotUA != userAgent {
		t.Fatalf("headers auth=%q session=%q ua=%q", gotAuth, gotSession, gotUA)
	}
	if gotBody["model"] != "muse-spark-1.3-contributor" {
		t.Fatalf("model=%v", gotBody["model"])
	}
	if gotBody["instructions"] != "be brief" {
		t.Fatalf("instructions=%v", gotBody["instructions"])
	}
	if store, _ := gotBody["store"].(bool); store {
		t.Fatal("store must be false (stateless replay)")
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", gotBody["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["parameters"] == nil {
		t.Fatalf("tool=%v", tool)
	}
	reasoning, _ := gotBody["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning=%v", gotBody["reasoning"])
	}
}

func TestResponsesSurfacesAPIError(t *testing.T) {
	fastRetry(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit"}}`))
	}))
	defer srv.Close()

	cfg := responsesTestConfig(srv.URL)
	err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), NewRegistry(), "hi", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "responses HTTP 429") {
		t.Fatalf("err=%v", err)
	}
	if fe := friendlyError(err); !strings.Contains(fe.Title, "Rate limited") {
		t.Fatalf("friendly=%+v", fe)
	}
}
