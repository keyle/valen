package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/keyle/valen/internal/dvlog"
	"github.com/keyle/valen/internal/guard"
	"github.com/sashabaranov/go-openai"
)

// compaction.go: summary-mode compaction, send-side only (disk JSONL stays
// lossless). Everything except the recent keep.tokens tail is replaced by a
// short summary beside the verbatim tail, so stateless transports stop
// resending rounds × history every round. Summaries also fire early (keep*3)
// on huge budgets so sessions compact often, not just near the limit.
//
// Pruning preserves the latest two user turns and shields another 40k tokens
// of older tool outputs. Beyond those shields, reads collapse to path/range
// descriptors while user decisions never shrink. Summaries retain file
// identities and decisions, not file content or private thinking.
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
	// Auto-compaction also triggers at keep*this even on huge budgets, so big
	// sessions compact early and often instead of resending rounds × history
	// all the way to the fail-fast limit.
	compactionSoftThresholdFactor = 3
	compactionSummaryPrefix       = "[Session summary — older history compressed, recent turns follow verbatim]:\n"
)

const compactionSummarizerSystem = "You compress coding-session history into a short handoff summary so another agent can continue the work. Be terse and factual. No tools, no questions, summary text only."

const compactionSummaryTemplate = `Summarize the conversation below for continuation. Cover: objective, key decisions, completed and active work, blockers/next moves, relevant files. Keep it under ~400 lines. Conversation:
`

// normalizeCompactionConfig fills [compaction] defaults: auto on, keep 15k,
// buffer 10% of budget, empty model (session model summarizes), thinking
// "medium" (explicit effort so summaries don't inherit the main xhigh).
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
	c.Thinking = strings.ToLower(strings.TrimSpace(c.Thinking))
	if c.Thinking == "" {
		c.Thinking = defaultCompactionThinking
	}
	return c
}

// defaultCompactionThinking is the summarizer effort when [compaction]
// thinking is unset: medium quality at a fraction of a main-turn xhigh.
const defaultCompactionThinking = "medium"

// validateExplicitCompactionThinking rejects an explicitly set [compaction]
// thinking level the summarizer model doesn't accept, mirroring the
// top-level thinking check. Empty means "default" and always passes — the
// medium default falls back per-model at use time in compactionThinking, so
// models without a medium level (e.g. glm flash) never fail config load.
func validateExplicitCompactionThinking(model, thinking string) error {
	if strings.TrimSpace(thinking) == "" {
		return nil
	}
	if !validThinkingLevel(model, thinking) {
		return fmt.Errorf("compaction thinking %q is not available for model %q (try: %s)", thinking, model, strings.Join(AvailableThinkingLevels(model), ", "))
	}
	return nil
}

// compactionThinking resolves the summarizer effort: the explicit
// [compaction] thinking when the model accepts it, else "medium" when
// accepted, else the cheapest level the model takes. The medium-first order
// is deliberate: summaries must stay faithful, minimal thinking drops
// decisions.
func compactionThinking(cfg Config) string {
	model := compactionModel(cfg)
	if validThinkingLevel(model, cfg.Compaction.Thinking) && cfg.Compaction.Thinking != "" {
		return cfg.Compaction.Thinking
	}
	if validThinkingLevel(model, defaultCompactionThinking) {
		return defaultCompactionThinking
	}
	return cheapestThinking(model)
}

func compactionAuto(cfg Config) bool {
	return cfg.Compaction.Auto != nil && *cfg.Compaction.Auto
}

// compactionThreshold is the pre-send estimate that triggers a summary:
// budget headroom (buffer) stays free below the fail-fast limit, and on huge
// budgets keep*3 fires much earlier so sessions compact often instead of
// resending a 40k+ history every round.
func compactionThreshold(cfg Config) int {
	budget := contextBudget(cfg)
	hard := budget
	if cfg.Compaction.Buffer > 0 {
		hard = budget - cfg.Compaction.Buffer
	} else {
		hard = budget - budget/10
	}
	soft := cfg.Compaction.KeepTokens * compactionSoftThresholdFactor
	if soft < hard {
		return soft
	}
	return hard
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

// fitSendBudget preserves registry-capped outputs on the wire. Pruning may
// shorten older history, but fresh results must be available in full.
func fitSendBudget(content string) string {
	// Empty tool output must still be a non-empty string on the wire: the
	// OpenAI client drops empty `content`, and OpenRouter then rejects the
	// tool message ("text content parts must carry a string text").
	if strings.TrimSpace(content) == "" {
		return "(no output)"
	}
	return content
}

// truncateHead cuts s to max runes with a marker, keeping the head.
func truncateHead(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n[truncated]"
}

// serializeForSummary renders history as summarizer text. File reads degrade
// to path/range descriptors (content re-reads cheaply); user decisions stay
// fuller; everything else caps at 2000 chars. Attachment/image data degrades
// to a short descriptor instead of raw bytes.
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
			_, answer := splitThinking(m.Content)
			if answer != "" {
				b.WriteString("[Assistant]: " + truncateHead(describeForSummary(answer), compactionToolOutputMaxChars*2) + "\n")
			}
			for _, tc := range m.ToolCalls {
				args := truncateHead(strings.TrimSpace(tc.Function.Arguments), compactionToolOutputMaxChars)
				b.WriteString("[Assistant tool call]: " + tc.Function.Name + "(" + args + ")\n")
			}
		case openai.ChatMessageRoleTool:
			if d, ok := readDescriptor(m.Content); ok {
				b.WriteString("[Tool result]: " + d + "\n")
				continue
			}
			if isDecisionOutput(m.Content) {
				b.WriteString("[Tool result]: " + truncateHead(describeForSummary(m.Content), compactionToolOutputMaxChars*2) + "\n")
				continue
			}
			b.WriteString("[Tool result]: " + truncateHead(describeForSummary(m.Content), compactionToolOutputMaxChars) + "\n")
		}
	}
	return b.String()
}

// readDescriptor spots a `read` tool result and keeps only its identity
// lines verbatim: path, total lines, shown range, next-offset hint. The code
// itself is bloat — the model re-reads it with read {path, offset} whenever
// it matters. It returns false for non-read output.
func readDescriptor(content string) (string, bool) {
	t := strings.TrimSpace(content)
	// Re-pruning a descriptor must retain its next-offset hint.
	t = strings.TrimSuffix(t, "\n[content dropped — re-read with read {path, offset} to restore]")
	if !strings.HasPrefix(t, "File ") {
		return "", false
	}
	lines := strings.Split(t, "\n")
	if len(lines) == 0 || !strings.Contains(lines[0], "showing") || !strings.Contains(lines[0], "lines") {
		return "", false
	}
	out := lines[0]
	if last := strings.TrimSpace(lines[len(lines)-1]); strings.HasPrefix(last, "...[") && strings.Contains(last, "offset") {
		out += "\n" + last
	}
	return out + "\n[content dropped — re-read with read {path, offset} to restore]", true
}

// isReadOutput is readDescriptor's boolean form for prune selection.
func isReadOutput(content string) bool {
	_, ok := readDescriptor(content)
	return ok
}

// isDecisionOutput reports tool results that must never shrink: user answers
// (AskUserQuestion) are decisions the summary must keep verbatim.
func isDecisionOutput(content string) bool {
	return strings.HasPrefix(strings.TrimSpace(content), "User has answered")
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

// isCompactionUser excludes guard reminders from genuine turn boundaries.
func isCompactionUser(m openai.ChatCompletionMessage) bool {
	if m.Role != openai.ChatMessageRoleUser {
		return false
	}
	switch strings.TrimSpace(m.Content) {
	case guard.BatchNudgeText, guard.ActNudgeText, guard.StallRecoveryText, guard.ClaimChallengeText:
		return false
	}
	return true
}

// splitForCompaction keeps whole user turns, including every call/result
// transaction. keepTokens is soft: the current request and its entire turn
// stay verbatim even when oversized. Leading systems are handled by the caller.
func splitForCompaction(history []openai.ChatCompletionMessage, keepTokens int) (head, tail []openai.ChatCompletionMessage) {
	sys := 0
	for sys < len(history) && history[sys].Role == openai.ChatMessageRoleSystem {
		sys++
	}
	body := history[sys:]
	if estimateTokens(body) <= keepTokens {
		return nil, body
	}
	total := 0
	split := len(body)
	for i := len(body) - 1; i >= 0; i-- {
		total += estimateTokens(body[i : i+1])
		if isCompactionUser(body[i]) {
			if split < len(body) && total > keepTokens {
				break
			}
			split = i
		}
	}
	if split == len(body) || split == 0 {
		return nil, body
	}
	return body[:split], body[split:]
}

// pruneOldToolOutputs shrinks older tool results in place and returns the
// reclaimed token estimate. The latest two genuine user turns stay intact;
// older reads and other outputs share a 40k-token shield. Decisions and call
// records never shrink. Even histories without users keep the freshest batch.
func pruneOldToolOutputs(history []openai.ChatCompletionMessage) int {
	reclaimed := 0
	protectedStart := len(history)
	users := 0
	freshBatch := false
	for i := len(history) - 1; i >= 0; i-- {
		if !freshBatch && history[i].Role == openai.ChatMessageRoleAssistant && len(history[i].ToolCalls) > 0 {
			protectedStart = min(protectedStart, i)
			freshBatch = true
		}
		if isCompactionUser(history[i]) {
			protectedStart = min(protectedStart, i)
			users++
			if users == 2 {
				break
			}
		}
	}
	kept := 0
	for i := protectedStart - 1; i >= 0; i-- {
		m := history[i]
		if m.Role != openai.ChatMessageRoleTool || isDecisionOutput(m.Content) {
			continue
		}
		before := estimateTokens(history[i : i+1])
		kept += before
		if kept <= compactionPruneProtectTokens {
			continue
		}
		short, read := readDescriptor(m.Content)
		if !read {
			short = truncateHead(describeForSummary(m.Content), compactionToolOutputMaxChars)
			if short != m.Content {
				short += "\n[pruned: older tool output shortened]"
			}
		}
		if short != m.Content && len([]rune(short)) < len([]rune(m.Content)) {
			history[i].Content = short
			reclaimed += before - estimateTokens(history[i:i+1])
		}
	}
	return reclaimed
}

// isContextOverflow reports provider rejections meaning "history too long",
// the signal to compact once and retry instead of failing.
func isContextOverflow(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
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
	if status != 0 && status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	lower := strings.ToLower(msg)
	for _, k := range []string{"context_length_exceeded", "context too long", "prompt is too long", "prompt too long", "input is too long", "input too long", "too many tokens", "request entity too large"} {
		if strings.Contains(lower, k) {
			return true
		}
	}
	for _, subject := range []string{"context length", "context window", "input tokens", "input token limit", "input token count", "prompt tokens"} {
		if strings.Contains(lower, subject) && (strings.Contains(lower, "exceed") || strings.Contains(lower, "too long") || strings.Contains(lower, "too large")) {
			return true
		}
	}
	return false
}

// summarizeHead runs one cheap-model turn over headText with no tools and
// returns the summary text. Auto-compaction is off inside so it cannot
// recurse; thinking is the explicit [compaction] effort (medium default).
func summarizeHead(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, headText, previous string) (string, error) {
	model := compactionModel(cfg)
	sumCfg := cfg
	sumCfg.Model = model
	sumCfg.Thinking = compactionThinking(cfg)
	sumCfg.MaxRounds = 2
	sumCfg.Temperature = nil
	sumCfg.Plan = false // the summarizer answers, never plans: no overlay, no gates.
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
		_, previous = splitThinking(strings.TrimPrefix(head[0].Content, compactionSummaryPrefix))
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
	headRunes, previousRunes := []rune(headText), []rune(previous)
	if len(headRunes)+len(previousRunes) > maxHead*4 {
		// Drop the oldest prefix, including prior-summary text before new work.
		if len(headRunes) >= maxHead*4 {
			headText = string(headRunes[len(headRunes)-maxHead*4:])
			previous = ""
		} else {
			keepPrevious := maxHead*4 - len(headRunes)
			previous = string(previousRunes[len(previousRunes)-keepPrevious:])
		}
		headText = "[older head truncated to fit summarizer budget]\n" + headText
	}
	summary, err := summarizeHead(ctx, cfg, client, apiKey, hc, headText, previous)
	if err != nil {
		return "", false, err
	}
	sumMsg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: compactionSummaryPrefix + summary}
	*history = append(append(append([]openai.ChatCompletionMessage(nil), systems...), sumMsg), tail...)
	dvlog.Logf("compact summary ok headbytes=%d summary=%dB tail=%s", len(headText), len(summary), dvlog.HistoryLine(tail, estimateTokens(tail)))
	return summary, true, nil
}

// maybeAutoCompact keeps every send lean: prune runs unconditionally (cheap,
// local — unprotected old reads collapse to descriptors), then a summary fires
// past the threshold. It returns true when history shrank by either path.
func maybeAutoCompact(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, history *[]openai.ChatCompletionMessage) bool {
	if !compactionAuto(cfg) {
		return false
	}
	pruned := pruneOldToolOutputs(*history) > 0
	if estimateTokens(*history) <= compactionThreshold(cfg) {
		return pruned
	}
	_, ok, err := compactHistory(ctx, cfg, client, apiKey, hc, history)
	return pruned || (err == nil && ok)
}

// leanHistoryForNextRound keeps a long tool loop's per-round sends lean with
// a cheap local prune only (no network), preserving the latest two user turns.
// Summaries fire between turns via maybeAutoCompact and on
// overflow retry, never blocking the hot tool loop with an extra LLM call.
// This keeps prefix-cache affinity (bytes stay stable) and avoids doubling
// latency once past the soft threshold.
func leanHistoryForNextRound(cfg Config, history *[]openai.ChatCompletionMessage) {
	if !compactionAuto(cfg) {
		return
	}
	if n := pruneOldToolOutputs(*history); n > 0 {
		dvlog.Logf("lean prune %d outputs %s", n, dvlog.HistoryLine(*history, estimateTokens(*history)))
	}
}
