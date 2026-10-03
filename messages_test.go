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

func messagesTextResp(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "m",
		"stop_reason": "end_turn",
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
	return b
}

func messagesToolResp(id, name, args string) []byte {
	var input any
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		input = map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "m",
		"stop_reason": "tool_use",
		"content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": name, "input": input},
		},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
	return b
}

func messagesTestConfig(baseURL string) Config {
	return Config{Model: "minimax-m3", BaseURL: baseURL, SessionID: "ses-test", MaxRounds: 4}
}

// messagesTwoToolResp returns one assistant message with two tool_use blocks.
func messagesTwoToolResp() []byte {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "m",
		"stop_reason": "tool_use",
		"content": []any{
			map[string]any{"type": "tool_use", "id": "call-1", "name": "shell", "input": map[string]any{"command": "echo hi"}},
			map[string]any{"type": "tool_use", "id": "call-2", "name": "shell", "input": map[string]any{"command": "echo yo"}},
		},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
	return b
}

// TestMessagesParallelToolBatch proves a multi-call batch runs through the
// shared parallel dispatcher and both results fold back as ordered
// tool_result blocks in the next round's request.
func TestMessagesParallelToolBatch(t *testing.T) {
	var bodies []map[string]any
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		var out []byte
		if calls == 0 {
			out = messagesTwoToolResp()
		} else {
			out = messagesTextResp("all done")
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := messagesTestConfig(srv.URL)

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
	msgs, _ := bodies[1]["messages"].([]any)
	var ids, outputs []string
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		content, _ := mm["content"].([]any)
		for _, b := range content {
			bb, _ := b.(map[string]any)
			if bb["type"] != "tool_result" {
				continue
			}
			id, _ := bb["tool_use_id"].(string)
			ids = append(ids, id)
			out, _ := bb["content"].(string)
			outputs = append(outputs, out)
		}
	}
	if strings.Join(ids, ",") != "call-1,call-2" {
		t.Fatalf("tool_result order=%v", ids)
	}
	if len(outputs) != 2 || !strings.Contains(outputs[0], "hi") || !strings.Contains(outputs[1], "yo") {
		t.Fatalf("tool_result outputs=%v", outputs)
	}
}

func TestMessagesThinkingMapping(t *testing.T) {
	if messagesThinking("") != nil {
		t.Fatal("empty must omit")
	}
	if got := messagesThinking("none"); got["type"] != "disabled" {
		t.Fatalf("none=%v", got)
	}
	got := messagesThinking("HIGH")
	if got["type"] != "enabled" || got["budget_tokens"] != messagesThinkingBudget {
		t.Fatalf("high=%v", got)
	}
	if messagesThinking("bogus") != nil {
		t.Fatal("unknown levels must omit")
	}
}

func TestSplitChatHistory(t *testing.T) {
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "be brief"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
		{Role: openai.ChatMessageRoleAssistant, Content: "", ToolCalls: []openai.ToolCall{
			{ID: "call-1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "shell", Arguments: `{"command":"ls"}`}},
		}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "call-1", Content: "main.go"},
	}
	system, msgs := splitChatHistory(history)
	if system != "be brief" {
		t.Fatalf("system=%q", system)
	}
	if len(msgs) != 3 {
		t.Fatalf("msgs=%v", msgs)
	}
	if msgs[0].Role != "user" {
		t.Fatalf("msgs[0]=%v", msgs[0])
	}
	blocks, ok := msgs[1].Content.([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("assistant blocks=%v", msgs[1].Content)
	}
	toolUse, _ := blocks[0].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "call-1" {
		t.Fatalf("tool_use=%v", toolUse)
	}
	resBlocks, ok := msgs[2].Content.([]any)
	if !ok || len(resBlocks) != 1 {
		t.Fatalf("tool_result blocks=%v", msgs[2].Content)
	}
	res, _ := resBlocks[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != "call-1" {
		t.Fatalf("tool_result=%v", res)
	}
}

// TestSplitChatHistorySendBudget: long tool results shrink to the send
// budget on the wire while stored history keeps the full text.
func TestSplitChatHistorySendBudget(t *testing.T) {
	long := strings.Repeat("z", sendToolOutputChars+1000)
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleTool, ToolCallID: "call-1", Content: long},
	}
	_, msgs := splitChatHistory(history)
	resBlocks, _ := msgs[0].Content.([]any)
	res, _ := resBlocks[0].(map[string]any)
	content, _ := res["content"].(string)
	if len([]rune(content)) > sendToolOutputChars+200 || !strings.Contains(content, "send-truncated") {
		t.Fatalf("wire copy must shrink: %d runes", len([]rune(content)))
	}
	if history[0].Content != long {
		t.Fatal("stored history must keep the full result")
	}
}

// TestMessagesCacheBreakpoints: system, last tool, and last message block
// carry ephemeral cache breakpoints; empty system omits the field.
func TestMessagesCacheBreakpoints(t *testing.T) {
	if messagesSystemParam("") != nil {
		t.Fatal("empty system must omit")
	}
	blocks, _ := messagesSystemParam("sys").([]any)
	if len(blocks) != 1 {
		t.Fatalf("system=%v", messagesSystemParam("sys"))
	}
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}
	_, msgs := splitChatHistory(history)
	last, _ := msgs[len(msgs)-1].Content.([]any)
	blk, _ := last[0].(map[string]any)
	if cc, _ := blk["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Fatalf("last block must be cached: %v", msgs)
	}
	r := NewRegistry()
	r.LoadBuiltins([]string{"shell", "read"})
	tools := r.MessagesTools()
	for i, tool := range tools {
		_, has := tool.CacheControl["type"]
		if (i == len(tools)-1) != has {
			t.Fatalf("only the last tool takes the breakpoint: %+v", tools)
		}
	}
}

func TestMessagesReplyMapsThinking(t *testing.T) {
	resp := messagesResponse{StopReason: "end_turn"}
	resp.Content = []messagesBlock{{Type: "thinking", Thinking: "plan"}, {Type: "text", Text: "done"}}
	msg, err := messagesReplyToOpenAI(resp)
	if err != nil {
		t.Fatal(err)
	}
	thinking, answer := splitThinking(msg.Content)
	if thinking != "plan" || answer != "done" {
		t.Fatalf("thinking=%q answer=%q", thinking, answer)
	}
}

func TestMessagesToolLoop(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var out []byte
		if calls == 0 {
			out = messagesToolResp("call-1", "shell", `{"command":"echo hi"}`)
		} else {
			out = messagesTextResp("all done")
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := messagesTestConfig(srv.URL)

	var stdout, stderr bytes.Buffer
	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), registry, "hi", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "all done") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "[tool shell]") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestMessagesRequestShape(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotKey, gotSession, gotUA, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("x-api-key")
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("User-Agent")
		gotVer = r.Header.Get("anthropic-version")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(messagesTextResp("ok"))
	}))
	defer srv.Close()

	registry := NewRegistry()
	registry.LoadBuiltins([]string{"shell"})
	cfg := messagesTestConfig(srv.URL)
	cfg.System = "be brief"
	cfg.Thinking = "high"

	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), registry, "hi", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer key" || gotKey != "key" || gotSession != "ses-test" || gotUA != userAgent || gotVer != anthropicVersion {
		t.Fatalf("headers auth=%q key=%q session=%q ua=%q ver=%q", gotAuth, gotKey, gotSession, gotUA, gotVer)
	}
	sysBlocks, _ := gotBody["system"].([]any)
	if len(sysBlocks) != 1 {
		t.Fatalf("system must be one cached block: %v", gotBody["system"])
	}
	sysBlock, _ := sysBlocks[0].(map[string]any)
	if sysBlock["text"] != "be brief" {
		t.Fatalf("system block: %v", sysBlock)
	}
	if cc, _ := sysBlock["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Fatalf("system block must carry the cache breakpoint: %v", sysBlock)
	}
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Fatal("max_tokens is required")
	}
	if gotBody["max_tokens"] != float64(defaultMaxTokens) {
		t.Fatalf("max_tokens must default %d, got %v", defaultMaxTokens, gotBody["max_tokens"])
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", gotBody["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if _, ok := tool["input_schema"]; !ok {
		t.Fatalf("tool needs input_schema: %v", tool)
	}
	if cc, _ := tool["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Fatalf("last tool must carry the cache breakpoint: %v", tool)
	}
	thinking, _ := gotBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking=%v", gotBody["thinking"])
	}
	if _, present := gotBody["temperature"]; present {
		t.Fatal("temperature must be omitted when unset")
	}
}

func TestMessagesTemperatureSentWhenSet(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write(messagesTextResp("ok"))
	}))
	defer srv.Close()

	cfg := messagesTestConfig(srv.URL)
	temp := float32(0.5)
	cfg.Temperature = &temp
	if err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), NewRegistry(), "hi", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if gotBody["temperature"] != float64(0.5) {
		t.Fatalf("temperature=%v", gotBody["temperature"])
	}
}

func TestMessagesSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"type":"error","error":{"type":"billing","message":"Insufficient funds"}}`))
	}))
	defer srv.Close()

	cfg := messagesTestConfig(srv.URL)
	err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), NewRegistry(), "hi", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "api:") {
		t.Fatalf("err=%v", err)
	}
	if fe := friendlyError(err); !strings.Contains(fe.Title, "Out of credits") {
		t.Fatalf("friendly=%+v", fe)
	}
}

func TestMessagesRejectsEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`))
	}))
	defer srv.Close()

	cfg := messagesTestConfig(srv.URL)
	err := chat(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), NewRegistry(), "hi", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "empty choices") {
		t.Fatalf("err=%v", err)
	}
}
