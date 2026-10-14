package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/keyle/valen/internal/dvlog"
	"github.com/keyle/valen/internal/guard"
	"github.com/sashabaranov/go-openai"
)

// maxParallelTools bounds concurrent tool execution in one round. Outputs
// stay capped by truncate, so memory stays flat; the semaphore only caps how
// many run at once.
const maxParallelTools = 8

// maxParallelTasks bounds concurrent subagents in one round; extra task calls
// queue. Each is a full provider loop, so the cap guards rate limits.
const maxParallelTasks = 5

// loopFingerprint identifies a tool call for repeat detection: same tool
// with the same (normalized) arguments doing the same work.
func loopFingerprint(tc openai.ToolCall) string {
	args := normalizeArgs(tc.Function.Arguments)
	var value any
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			args = string(canonical)
		}
	}
	return tc.Function.Name + "\x00" + args
}

// doomLoopTool reports the tool name when calls would repeat the same single
// tool call a third consecutive time — the death pattern (re-reading the
// same file, retrying the same command). Multi-call batches break the chain:
// they run as one parallel unit, not a loop step. "" means no loop.
func doomLoopTool(history []openai.ChatCompletionMessage, calls []openai.ToolCall) string {
	if len(calls) != 1 {
		return ""
	}
	want := loopFingerprint(calls[0])
	found := 0
	for i := len(history) - 1; i >= 0 && found < 2; i-- {
		m := history[i]
		if m.Role == openai.ChatMessageRoleUser || (m.Role == openai.ChatMessageRoleAssistant && len(m.ToolCalls) == 0) {
			break
		}
		if m.Role != openai.ChatMessageRoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		if len(m.ToolCalls) != 1 || loopFingerprint(m.ToolCalls[0]) != want {
			return ""
		}
		found++
	}
	if found < 2 {
		return ""
	}
	return calls[0].Function.Name
}

func doomLoopError(name string) error {
	return fmt.Errorf("stopped: tool %q called 3 times with identical input — ask for a different approach or /clear", name)
}

// messageOverheadTokens covers the per-message JSON envelope the runes/4
// content estimate misses (role, tool_call ids, structure): ~150 chars.
// Without it the estimator under-counts real prefill and compaction fires
// late, after the oversized send already burned.
const messageOverheadTokens = 40

// estimateTokens approximates prompt tokens as runes/4 over content plus
// tool args, plus envelope overhead per message. No tokenizer dep; good
// enough for a fail-fast guard now that schemas ride separately (A3).
func estimateTokens(msgs []openai.ChatCompletionMessage) int {
	var runes int
	for _, m := range msgs {
		runes += len([]rune(m.Content))
		runes += len([]rune(m.ReasoningContent))
		for _, tc := range m.ToolCalls {
			runes += len([]rune(tc.Function.Arguments))
		}
	}
	return runes/4 + len(msgs)*messageOverheadTokens
}

// requestToolsTokens estimates the tool-schema block re-sent on every round
// (every builtin + MCP schema). Stateless transports pay this × rounds, so
// the budget guard must see it: a 30k schema block with a 45k history is a
// 75k send, not 45k.
func requestToolsTokens(registry *Registry) int {
	if registry == nil {
		return 0
	}
	raw, err := json.Marshal(registry.OpenAITools())
	if err != nil {
		return 0
	}
	return len([]rune(string(raw))) / 4
}

func contextBudget(cfg Config) int {
	if cfg.MaxContextTokens > 0 {
		return cfg.MaxContextTokens
	}
	// Auto: the model's own window when known, else the safe default.
	if w := modelContextWindow(cfg.Model); w > 0 {
		return w
	}
	return defaultMaxContextTokens
}

// isMaxRoundsStop reports the round-backstop error: the loop spent its whole
// window without a final answer. Budget and overflow errors never match.
func isMaxRoundsStop(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "stopped after")
}

// checkContextBudget fail-fasts a round that would send a ridiculous context
// (and burn tokens × rounds on stateless transports) instead of letting it
// crawl into a 429. toolsTokens is the re-sent schema block (see
// requestToolsTokens). Compaction is the real fix; this is the guardrail.
func checkContextBudget(cfg Config, history []openai.ChatCompletionMessage, toolsTokens int) error {
	budget := contextBudget(cfg)
	if est := estimateTokens(history) + toolsTokens; est > budget {
		return fmt.Errorf("context too large: est. %d tokens exceeds budget %d — /compact to summarize or /clear for a fresh history (or raise max_context_tokens)", est, budget)
	}
	return nil
}

// Only known read-only builtins and task subagents may overlap. Mutating,
// interactive and unknown tools run serially; the registry owns its central
// gates (including MCP). Subagent file writes are safe via the per-path lock
// in write/edit; shell writes are not covered (the prompt says so).
// The coordinator emits results as completed, but history stays request-ordered.
func runToolCalls(ctx context.Context, registry *Registry, calls []openai.ToolCall, onEvent func(Event)) []openai.ChatCompletionMessage {
	if len(calls) == 0 {
		return nil
	}
	for _, tc := range calls {
		switch tc.Function.Name {
		case "read", "glob", "grep", "web_search", "web_fetch", "task":
		default:
			return runToolCallsSerial(ctx, registry, calls, onEvent)
		}
	}
	if len(calls) == 1 {
		return runToolCallsSerial(ctx, registry, calls, onEvent)
	}
	if onEvent != nil {
		for _, tc := range calls {
			onEvent(Event{Kind: "tool_start", Text: tc.Function.Name, Args: tc.Function.Arguments, ID: tc.ID})
		}
	}
	batch0 := time.Now()
	type result struct {
		index  int
		output string
		durMs  int64
	}
	results := make(chan result, len(calls))
	out := make([]openai.ChatCompletionMessage, len(calls))
	sem := make(chan struct{}, maxParallelTools)
	taskSem := make(chan struct{}, maxParallelTasks)
	for i, tc := range calls {
		go func(i int, tc openai.ToolCall) {
			// Subagents queue on their own cap and skip the read-tool slots.
			s := sem
			if tc.Function.Name == "task" {
				s = taskSem
			}
			select {
			case s <- struct{}{}:
			case <-ctx.Done():
				results <- result{i, "error: " + ctx.Err().Error(), 0}
				return
			}
			defer func() { <-s }()
			t0 := time.Now()
			var output string
			if err := ctx.Err(); err != nil {
				output = "error: " + err.Error()
			} else {
				output = registry.Run(ctx, tc.Function.Name, tc.Function.Arguments)
			}
			durMs := time.Since(t0).Milliseconds()
			dvlog.Logf("tool %s args=%dB dur=%s out=%dB %q", tc.Function.Name, len(tc.Function.Arguments), dvlog.Dur(time.Since(t0)), len(output), dvlog.Preview(tc.Function.Arguments, 160))
			results <- result{i, output, durMs}
		}(i, tc)
	}
	for range calls {
		res := <-results
		tc := calls[res.index]
		out[res.index] = openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: res.output}
		if onEvent != nil {
			onEvent(Event{Kind: "tool_result", Text: tc.Function.Name, Output: res.output, ID: tc.ID, DurMs: res.durMs})
		}
	}
	dvlog.Logf("batch %d tools total=%s", len(calls), dvlog.Dur(time.Since(batch0)))
	return out
}

// runToolCallsSerial preserves execution order for potentially dependent calls.
func runToolCallsSerial(ctx context.Context, registry *Registry, calls []openai.ToolCall, onEvent func(Event)) []openai.ChatCompletionMessage {
	batch0 := time.Now()
	out := make([]openai.ChatCompletionMessage, 0, len(calls))
	for _, tc := range calls {
		if onEvent != nil {
			onEvent(Event{Kind: "tool_start", Text: tc.Function.Name, Args: tc.Function.Arguments, ID: tc.ID})
		}
		t0 := time.Now()
		var res string
		if err := ctx.Err(); err != nil {
			res = "error: " + err.Error()
		} else {
			res = registry.Run(ctx, tc.Function.Name, tc.Function.Arguments)
		}
		durMs := time.Since(t0).Milliseconds()
		dvlog.Logf("tool %s args=%dB dur=%s out=%dB serial %q", tc.Function.Name, len(tc.Function.Arguments), dvlog.Dur(time.Since(t0)), len(res), dvlog.Preview(tc.Function.Arguments, 160))
		if onEvent != nil {
			onEvent(Event{Kind: "tool_result", Text: tc.Function.Name, Output: res, ID: tc.ID, DurMs: durMs})
		}
		out = append(out, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: res})
	}
	dvlog.Logf("batch %d tools serial total=%s", len(calls), dvlog.Dur(time.Since(batch0)))
	return out
}

// runModelTurn owns execution policy; round owns only transport and replay.
func runModelTurn(ctx context.Context, cfg Config, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event), round func(context.Context, []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error)) (openai.ChatCompletionMessage, openai.Usage, error) {
	var usage openai.Usage
	guard.StripNudges(history)
	toolsTok := requestToolsTokens(registry)
	maxStagnant := cfg.MaxStagnantRounds
	if maxStagnant <= 0 {
		maxStagnant = guard.DefaultMaxStagnantRounds
	}
	var previousBatch string
	identical := 0
	for n := 0; n < cfg.MaxRounds; n++ {
		if err := ctx.Err(); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		if err := checkContextBudget(cfg, *history, toolsTok); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		if onEvent != nil {
			onEvent(Event{Kind: "round_start", Text: fmt.Sprintf("Round %d/%d", n+1, cfg.MaxRounds)})
		}
		t0 := time.Now()
		msg, u, err := roundWithRetry(ctx, onEvent, *history, round)
		usage.PromptTokens += u.PromptTokens
		usage.CompletionTokens += u.CompletionTokens
		usage.TotalTokens += u.TotalTokens
		dvlog.Logf("round %d %s dur=%s in=%d out=%d content=%dB calls=%s err=%v", n+1, modelTransport(cfg.Model), dvlog.Dur(time.Since(t0)), u.PromptTokens, u.CompletionTokens, len(msg.Content), dvlog.CallsLine(msg.ToolCalls), err)
		if err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		if err := ctx.Err(); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		if err := validateModelReply(msg); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		if len(msg.ToolCalls) == 0 {
			*history = append(*history, msg)
			return msg, usage, nil
		}
		if name := doomLoopTool(*history, msg.ToolCalls); name != "" {
			return openai.ChatCompletionMessage{}, usage, doomLoopError(name)
		}
		fingerprints := make([]string, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			fingerprints[i] = loopFingerprint(tc)
		}
		// Preserve batch order: serial writes with reversed order are different work.
		batch, _ := json.Marshal(fingerprints)
		if string(batch) == previousBatch {
			identical++
		} else {
			previousBatch, identical = string(batch), 1
		}
		if identical > maxStagnant {
			return openai.ChatCompletionMessage{}, usage, fmt.Errorf("stopped: tool batch repeated for %d consecutive rounds (max_stagnant_rounds=%d)", identical, maxStagnant)
		}
		*history = append(*history, msg)
		*history = append(*history, runToolCalls(ctx, registry, msg.ToolCalls, onEvent)...)
		if err := ctx.Err(); err != nil {
			return openai.ChatCompletionMessage{}, usage, err
		}
		leanHistoryForNextRound(cfg, history)
	}
	return openai.ChatCompletionMessage{}, usage, fmt.Errorf("stopped after %d rounds without a final answer", cfg.MaxRounds)
}

// Visible completion and valid calls are required regardless of transport.
func validateModelReply(msg openai.ChatCompletionMessage) error {
	if len(msg.ToolCalls) == 0 {
		_, answer := splitThinking(msg.Content)
		if strings.TrimSpace(answer) == "" {
			return errors.New("api: empty choices in response (no visible answer or tool calls)")
		}
	}
	ids := make(map[string]bool, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		if strings.TrimSpace(tc.ID) == "" || strings.TrimSpace(tc.Function.Name) == "" || tc.Type != openai.ToolTypeFunction {
			return errors.New("api: incomplete tool call (missing id, name or function type)")
		}
		if ids[tc.ID] {
			return fmt.Errorf("api: duplicate tool call ID %q", tc.ID)
		}
		ids[tc.ID] = true
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &object); err != nil || object == nil {
			return fmt.Errorf("api: invalid tool arguments for %q", tc.ID)
		}
	}
	return nil
}

func outputTokens(cfg Config) int {
	if cfg.MaxTokens > 0 {
		return cfg.MaxTokens
	}
	return defaultMaxTokens
}

// runChatTurn keeps fallback capability local to this execution loop.
func runChatTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	stream := true
	tools := registry.OpenAITools()
	return runModelTurn(ctx, cfg, registry, history, onEvent, func(ctx context.Context, history []openai.ChatCompletionMessage) (openai.ChatCompletionMessage, openai.Usage, error) {
		req := openai.ChatCompletionRequest{Model: wireModelID(cfg.Model, cfg.Routing), Messages: chatSendMessages(history), Tools: tools, MaxTokens: outputTokens(cfg)}
		if cfg.Temperature != nil {
			req.Temperature = *cfg.Temperature
		}
		applyThinkingToRequest(&req, cfg.Model, cfg.Thinking)
		round := func() (openai.ChatCompletionMessage, openai.Usage, error) {
			if stream {
				msg, usage, err := chatRoundStream(ctx, cfg, apiKey, hc, req, onEvent)
				if !isStreamRejected(err) {
					return msg, usage, err
				}
				stream = false
			}
			return chatRoundOnce(ctx, cfg, apiKey, hc, req)
		}
		msg, usage, err := round()
		if errors.Is(err, errContentFilter) && ctx.Err() == nil {
			// Provider filters are often false positives and non-deterministic;
			// retry the round once. Nothing was executed from the partial reply.
			msg, usage, err = round()
		}
		return msg, usage, err
	})
}
