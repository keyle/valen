package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sashabaranov/go-openai"
)

// compaction.go: summary-mode compaction, send-side only (disk JSONL stays
// lossless). Mirrors opencode: everything except the recent keep.tokens tail
// is replaced by a short summary beside the verbatim tail, so stateless
// transports stop resending rounds × history every round.
//
// The summary itself runs through the existing runTurn loop on a cheap model
// ([compaction] model, default = session model with minimal thinking), so all
// three transports (/chat, /messages, /responses) share one path.

const (
	defaultCompactionKeepTokens = 15000
	// Per-tool output cap inside the summarizer payload only (live turns keep
	// the 16k registry cap). Matches opencode's TOOL_OUTPUT_MAX_CHARS.
	compactionToolOutputMaxChars = 2000
	// Newest tool-output tokens protected from pruning; older outputs are
	// shortened in place. Matches opencode's PRUNE_PROTECT semantics.
	compactionPruneProtectTokens = 40000
	compactionSummaryPrefix      = "[Session summary — older history compressed, recent turns follow verbatim]:\n"
)

const compactionSummarizerSystem = "You compress coding-session history into a short handoff summary so another agent can continue the work. Be terse and factual. No tools, no questions, summary text only."

const compactionSummaryTemplate = `Summarize the conversation below for continuation. Cover: objective, key decisions, completed and active work, blockers/next moves, relevant files. Keep it under ~400 lines. Conversation:
`

// normalizeCompactionConfig fills [compaction] defaults: auto on, keep 15k,
// buffer 10% of budget, empty model (session model summarizes).
func normalizeCompactionConfig(c CompactionConfig, budget int) CompactionConfig {
	if c.Auto == nil {
		t := true
		c.Auto = &t
	}
	if c.KeepTokens <= 0 {
		c.KeepTokens = defaultCompactionKeepTokens
	}
	if c.Buffer <= 0 && budget > 0 {
		c.Buffer = budget / 10
	}
	c.Model = strings.TrimSpace(c.Model)
	return c
}

func compactionAuto(cfg Config) bool {
	return cfg.Compaction.Auto != nil && *cfg.Compaction.Auto
}

// compactionThreshold is the pre-send estimate that triggers compaction:
// budget headroom (buffer) stays free below the fail-fast limit.
func compactionThreshold(cfg Config) int {
	budget := contextBudget(cfg)
	if cfg.Compaction.Buffer > 0 {
		return budget - cfg.Compaction.Buffer
	}
	return budget - budget/10
}

// compactionModel selects the summarizer: cheap [compaction] override when
// set, otherwise the session model itself.
func compactionModel(cfg Config) string {
	if cfg.Compaction.Model != "" {
		return cfg.Compaction.Model
	}
	return cfg.Model
}

// cheapestThinking picks the lowest-cost reasoning level a model accepts so
// summaries stay cheap. Empty means provider default (omit the field).
func cheapestThinking(model string) string {
	for _, lv := range []string{"minimal", "low", "none"} {
		if validThinkingLevel(model, lv) {
			return lv
		}
	}
	return ""
}

// truncateHead cuts s to max runes with a marker, keeping the head.
func truncateHead(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n[truncated]"
}

// serializeForSummary renders history as summarizer text with each tool
// output capped at 2000 chars. Attachment/image data degrades to a short
// descriptor instead of raw bytes.
func serializeForSummary(history []openai.ChatCompletionMessage) string {
	var b strings.Builder
	for _, m := range history {
		switch m.Role {
		case openai.ChatMessageRoleSystem:
			if strings.TrimSpace(m.Content) != "" {
				b.WriteString("[System]: " + describeForSummary(m.Content) + "\n")
			}
		case openai.ChatMessageRoleUser:
			b.WriteString("[User]: " + describeForSummary(m.Content) + "\n")
		case openai.ChatMessageRoleAssistant:
			if strings.TrimSpace(m.Content) != "" {
				b.WriteString("[Assistant]: " + truncateHead(describeForSummary(m.Content), compactionToolOutputMaxChars*2) + "\n")
			}
			for _, tc := range m.ToolCalls {
				args := truncateHead(strings.TrimSpace(tc.Function.Arguments), compactionToolOutputMaxChars)
				b.WriteString("[Assistant tool call]: " + tc.Function.Name + "(" + args + ")\n")
			}
		case openai.ChatMessageRoleTool:
			b.WriteString("[Tool result]: " + truncateHead(describeForSummary(m.Content), compactionToolOutputMaxChars) + "\n")
		}
	}
	return b.String()
}

// describeForSummary replaces bulky attachment/image blobs with a short
// descriptor so the kept window and the summary stay text-only.
func describeForSummary(s string) string {
	t := strings.TrimSpace(s)
	if len(t) > 4000 && isMostlyOpaque(t) {
		return fmt.Sprintf("[attached data: ~%d chars, omitted]", len(t))
	}
	return t
}

// isMostlyOpaque heuristically spots base64/data blobs: long, single-line,
// tiny whitespace fraction.
func isMostlyOpaque(s string) bool {
	if len(s) < 4000 || strings.Contains(s, "\n") && strings.Count(s, "\n") > 5 {
		lines := strings.Count(s, "\n")
		if lines > 5 {
			return false
		}
	}
	ws := 0
	for _, r := range s[:min(4000, len(s))] {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			ws++
		}
	}
	return ws < 4000/20
}

// splitForCompaction keeps leading system messages plus the newest tail
// within keepTokens; everything between is the compactable head. It returns
// head (to summarize, possibly including a prior summary) and tail (kept
// verbatim). An existing summary block at the head of the body is folded
// into head so repeats update in place instead of stacking.
func splitForCompaction(history []openai.ChatCompletionMessage, keepTokens int) (head, tail []openai.ChatCompletionMessage) {
	sys := 0
	for sys < len(history) && history[sys].Role == openai.ChatMessageRoleSystem {
		sys++
	}
	body := history[sys:]
	if len(body) == 0 {
		return nil, history
	}
	total := 0
	split := len(body)
	for i := len(body) - 1; i >= 0; i-- {
		if total+estimateTokens(body[i:i+1]) > keepTokens {
			split = i + 1
			break
		}
		total += estimateTokens(body[i : i+1])
		split = i
	}
	if split <= 0 {
		return body, history[sys:sys]
	}
	return body[:split], body[split:]
}

// pruneOldToolOutputs shortens older tool results in place, protecting the
// newest 40k tokens of tool output. It returns reclaimed token estimate.
// The call record (assistant tool_call) stays, only bulky results shrink.
func pruneOldToolOutputs(history []openai.ChatCompletionMessage) int {
	protect := compactionPruneProtectTokens
	var idx []int
	for i := range history {
		if history[i].Role == openai.ChatMessageRoleTool {
			idx = append(idx, i)
		}
	}
	kept := 0
	prunedFrom := -1
	for k := len(idx) - 1; k >= 0; k-- {
		t := estimateTokens(history[idx[k] : idx[k]+1])
		if kept+t > protect {
			prunedFrom = k
			break
		}
		kept += t
	}
	if prunedFrom < 0 {
		return 0
	}
	reclaimed := 0
	for _, k := range idx[:prunedFrom+1] {
		m := history[k]
		before := estimateTokens([]openai.ChatCompletionMessage{m})
		short := truncateHead(describeForSummary(m.Content), compactionToolOutputMaxChars)
		if short == m.Content {
			continue
		}
		history[k].Content = short + "\n[pruned: older tool output shortened]"
		reclaimed += before - estimateTokens(history[k:k+1])
	}
	return reclaimed
}

// isContextOverflow reports provider rejections meaning "history too long",
// the signal to compact once and retry instead of failing.
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	var te *transportError
	if errors.As(err, &te) {
		return overflowStatusMessage(te.Status, te.Message)
	}
	var ae *openai.APIError
	if errors.As(err, &ae) {
		return overflowStatusMessage(ae.HTTPStatusCode, ae.Message)
	}
	var re *openai.RequestError
	if errors.As(err, &re) {
		return overflowStatusMessage(re.HTTPStatusCode, err.Error())
	}
	return overflowStatusMessage(0, err.Error())
}

func overflowStatusMessage(status int, msg string) bool {
	if status == http.StatusRequestEntityTooLarge {
		return true
	}
	lower := strings.ToLower(msg)
	for _, k := range []string{"too long", "too large", "context", "exceed", "max tokens", "max_tokens", "token limit", "context length", "request entity too large"} {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

// summarizeHead runs one cheap-model turn over headText with no tools and
// returns the summary text. Auto-compaction is off inside so it cannot
// recurse; thinking drops to the cheapest level the model accepts.
func summarizeHead(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, headText, previous string) (string, error) {
	model := compactionModel(cfg)
	sumCfg := cfg
	sumCfg.Model = model
	sumCfg.Thinking = cheapestThinking(model)
	sumCfg.MaxRounds = 2
	sumCfg.Temperature = nil
	f := false
	sumCfg.Compaction.Auto = &f
	sumCfg.Tools = nil

	prompt := compactionSummaryTemplate
	if strings.TrimSpace(previous) != "" {
		prompt = "Previous summary (update it, don't restart):\n" + previous + "\n\n" + prompt
	}
	h := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: compactionSummarizerSystem},
		{Role: openai.ChatMessageRoleUser, Content: prompt + headText},
	}
	reg := NewRegistry()
	msg, _, err := runTurn(ctx, sumCfg, client, apiKey, hc, reg, &h, nil)
	if err != nil {
		return "", err
	}
	_, answer := splitThinking(msg.Content)
	if strings.TrimSpace(answer) == "" {
		answer = strings.TrimSpace(msg.Content)
	}
	if answer == "" {
		return "", fmt.Errorf("compaction: empty summary from %s", model)
	}
	return strings.TrimSpace(answer), nil
}

// compactHistory replaces the compactable head with a fresh summary beside
// the verbatim tail. It returns the summary text and true when history was
// rewritten. The head payload is bounded so the summarizer request itself
// cannot overflow: oversized heads truncate from the front.
func compactHistory(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, history *[]openai.ChatCompletionMessage) (string, bool, error) {
	h := *history
	sys := 0
	for sys < len(h) && h[sys].Role == openai.ChatMessageRoleSystem {
		sys++
	}
	systems := append([]openai.ChatCompletionMessage(nil), h[:sys]...)
	body := h[sys:]
	if len(body) < 2 {
		return "", false, nil
	}
	head, tail := splitForCompaction(h, cfg.Compaction.KeepTokens)
	if len(head) == 0 {
		return "", false, nil
	}
	previous := ""
	if strings.HasPrefix(head[0].Content, compactionSummaryPrefix) && head[0].Role == openai.ChatMessageRoleAssistant {
		previous = strings.TrimPrefix(head[0].Content, compactionSummaryPrefix)
		head = head[1:]
		if len(head) == 0 {
			return "", false, nil
		}
	}
	headText := serializeForSummary(head)
	// Bound the summarizer payload: keep head + tail + summary headroom
	// under the fail-fast budget so huge sessions compact instead of 400ing.
	maxHead := contextBudget(cfg) - cfg.Compaction.Buffer - cfg.Compaction.KeepTokens - 4096
	if maxHead < 4000 {
		maxHead = 4000
	}
	if est := len([]rune(headText)) / 4; est > maxHead {
		headText = truncateHead(headText, maxHead*4)
		headText = "[older head truncated to fit summarizer budget]\n" + headText
	}
	summary, err := summarizeHead(ctx, cfg, client, apiKey, hc, headText, previous)
	if err != nil {
		return "", false, err
	}
	sumMsg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: compactionSummaryPrefix + summary}
	*history = append(append(append([]openai.ChatCompletionMessage(nil), systems...), sumMsg), tail...)
	return summary, true, nil
}

// maybeAutoCompact prunes then summarizes when history exceeds the
// compaction threshold. It returns true when history shrank. Cheap and local
// (prune) runs before any network (summary).
func maybeAutoCompact(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, history *[]openai.ChatCompletionMessage) bool {
	if !compactionAuto(cfg) {
		return false
	}
	if estimateTokens(*history) <= compactionThreshold(cfg) {
		return false
	}
	pruneOldToolOutputs(*history)
	if estimateTokens(*history) <= compactionThreshold(cfg) {
		return true
	}
	_, ok, err := compactHistory(ctx, cfg, client, apiKey, hc, history)
	return err == nil && ok
}
