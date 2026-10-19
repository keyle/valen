package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/keyle/valen/internal/dvlog"
	"github.com/keyle/valen/internal/prompts"
	"github.com/sashabaranov/go-openai"
)

// Event reports progress during a turn so frontends stay responsive.
type Event struct {
	Kind string // "round_start", "tool_start", "tool_result", "text_delta", "thinking_delta"
	Text string
	// Args carries tool JSON arguments on tool_start; Output carries the
	// tool result on tool_result.
	Args   string
	Output string
	// ID carries the tool call ID on tool_start/tool_result so the UI
	// matches rows by call, not by name (parallel same-name calls).
	ID string
	// DurMs carries wall execution time on tool_result.
	DurMs int64
	// Index carries the reasoning part index on thinking_delta (Responses
	// summary_index, messages content-block index). Fragments that share an
	// index concatenate exactly; the first delta of a new part starts on its
	// own line, mirroring the committed join of separate summary parts.
	Index int
	// ORMeta carries OpenRouter serving metadata (provider, billed cost,
	// locally-measured throughput) on an "or_meta" event.
	ORMeta *ORMeta
}

// ORMeta is OpenRouter's best-effort per-round serving metadata: the upstream
// provider that served the request, the billed cost and the latency and
// decode throughput measured from our own stream. Any field may be absent.
type ORMeta struct {
	Provider string  `json:"provider,omitempty"`
	Cost     float64 `json:"cost,omitempty"`
	HasCost  bool    `json:"has_cost,omitempty"`
	// TTFT is seconds from request send to the first streamed token (thinking
	// or content), so it covers prefill, cache and queueing.
	TTFT float64 `json:"ttft,omitempty"`
	// TPS is the decode rate over all streamed tokens: completion tokens
	// divided by the span from first to last token.
	TPS float64 `json:"tps,omitempty"`
}

// parseOpenRouterMeta folds the serving provider and billed cost from one
// completion body (unary) or SSE chunk into the targets. Missing fields leave
// them untouched; `usage.cost` is preferred, falling back to
// `cost_details.upstream_inference_cost`.
func parseOpenRouterMeta(raw []byte, provider *string, cost *float64, hasCost *bool) {
	if !bytes.Contains(raw, []byte("provider")) && !bytes.Contains(raw, []byte("cost")) {
		return
	}
	var m struct {
		Provider string `json:"provider"`
		Usage    *struct {
			Cost        float64 `json:"cost"`
			CostDetails *struct {
				UpstreamInferenceCost float64 `json:"upstream_inference_cost"`
			} `json:"cost_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if m.Provider != "" {
		*provider = m.Provider
	}
	if m.Usage != nil {
		c := m.Usage.Cost
		if c == 0 && m.Usage.CostDetails != nil {
			c = m.Usage.CostDetails.UpstreamInferenceCost
		}
		if c != 0 {
			*cost, *hasCost = c, true
		}
	}
}

type turnConfigKey struct{}

func currentTurnConfig(ctx context.Context, fallback Config) Config {
	if cfg, ok := ctx.Value(turnConfigKey{}).(Config); ok {
		return cfg
	}
	return fallback
}

func requestTimeout(cfg Config) time.Duration {
	seconds := cfg.RequestTimeoutSeconds
	if seconds <= 0 {
		seconds = defaultRequestTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// runTurn drives the tool loop for one user turn. history grows in place with
// assistant and tool messages so the caller can keep the conversation going.
// It dispatches on model transport: chat (/chat/completions), messages
// (/messages) or responses (/responses). History stays OpenAI-shaped; the
// non-chat transports convert per round and fold replies back. A 400 that
// lists valid thinking levels is learned to disk for next time.
func runTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	ctx = context.WithValue(ctx, turnConfigKey{}, cfg)
	// Plan mode gates at the tool Fns via ctx, so mark it once here and
	// every nested call (tools, task subagent) inherits read-only.
	if cfg.Plan {
		ctx = prompts.WithPlanMode(ctx)
	}
	msg, usage, err := dispatchTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
	if err != nil {
		maybeLearnThinkingLevels(cfg.Model, err.Error())
		// One recovery compaction when the provider rejects history as too
		// long: compact send-side, then retry the turn once. A non-round
		// failure after the retry is terminal.
		if isContextOverflow(err) && compactionAuto(cfg) {
			if _, ok, cerr := compactHistory(ctx, cfg, client, apiKey, hc, history); cerr == nil && ok {
				m2, u2, err2 := dispatchTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
				usage.PromptTokens += u2.PromptTokens
				usage.CompletionTokens += u2.CompletionTokens
				usage.TotalTokens += u2.TotalTokens
				if err2 != nil {
					maybeLearnThinkingLevels(cfg.Model, err2.Error())
				}
				if err2 == nil || !isMaxRoundsStop(err2) {
					return m2, usage, err2
				}
				msg, err = m2, err2
			}
		}
		// Long-job continuation: the round backstop fired but the turn may
		// still be productive, so compact send-side and grant exactly one
		// more window. A second exhaustion is terminal and says so.
		if isMaxRoundsStop(err) && compactionAuto(cfg) {
			if _, ok, cerr := compactHistory(ctx, cfg, client, apiKey, hc, history); cerr == nil && ok {
				dvlog.Logf("maxrounds extend: compacted, second window of %d rounds", cfg.MaxRounds)
				m3, u3, err3 := dispatchTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
				usage.PromptTokens += u3.PromptTokens
				usage.CompletionTokens += u3.CompletionTokens
				usage.TotalTokens += u3.TotalTokens
				if err3 != nil {
					maybeLearnThinkingLevels(cfg.Model, err3.Error())
				}
				if err3 == nil {
					return m3, usage, nil
				}
				if isMaxRoundsStop(err3) {
					return m3, usage, fmt.Errorf("stopped after %d rounds across 2 compacted windows without a final answer — ask the model to continue with a narrower ask or /clear", cfg.MaxRounds*2)
				}
				return m3, usage, err3
			}
		}
	}
	return msg, usage, err
}

// effectiveOpenCodeKey resolves the OpenCode credential (one key serves Go
// and Zen): the boot snapshot first, then the opencode_key setting, then a
// live env read (OPENCODE_API_KEY) so an export after boot works without a
// restart.
func effectiveOpenCodeKey(cfg Config, apiKey string) string {
	if k := strings.TrimSpace(apiKey); k != "" {
		return k
	}
	if k := strings.TrimSpace(cfg.OpenCodeKey); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv(openCodeKeyEnv))
}

// effectiveOpenRouterKey resolves the OpenRouter credential the same way.
func effectiveOpenRouterKey(cfg Config) string {
	if k := strings.TrimSpace(cfg.OpenRouterKey); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv(openRouterKeyEnv))
}

// missingKeyError is nil when the model in cfg can be called; otherwise an
// "is not set" error naming the env var the provider needs. Boot never
// fails on this — it surfaces on model pick (TUI notice) and on send
// (dispatchTurn), where friendlyError turns it into a transcript message.
func missingKeyError(cfg Config, apiKey string) error {
	if modelProvider(cfg.Model) == ProviderOpenRouter {
		if effectiveOpenRouterKey(cfg) == "" {
			return fmt.Errorf("%s is not set (required by model %q)", openRouterKeyEnv, cfg.Model)
		}
		return nil
	}
	if effectiveOpenCodeKey(cfg, apiKey) == "" {
		return fmt.Errorf("%s is not set (required by model %q)", openCodeKeyEnv, cfg.Model)
	}
	return nil
}

// dispatchTurn runs one pre-send auto-compaction when over threshold, then
// the model transport. Shared by all three transports so every model gets
// the same send-side bound.
func dispatchTurn(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, registry *Registry, history *[]openai.ChatCompletionMessage, onEvent func(Event)) (openai.ChatCompletionMessage, openai.Usage, error) {
	if ctx.Err() != nil {
		return openai.ChatCompletionMessage{}, openai.Usage{}, ctx.Err()
	}
	if err := missingKeyError(cfg, apiKey); err != nil {
		return openai.ChatCompletionMessage{}, openai.Usage{}, err
	}
	// Refresh from the live env so an export after boot works without a
	// restart; downstream transports must see the usable credential, not
	// the possibly-empty boot snapshot.
	apiKey = effectiveOpenCodeKey(cfg, apiKey)
	cfg.OpenRouterKey = effectiveOpenRouterKey(cfg)
	t0 := time.Now()
	dvlog.Logf("turn start transport=%s %s", modelTransport(cfg.Model), dvlog.HistoryLine(*history, estimateTokens(*history)))
	before := estimateTokens(*history)
	maybeAutoCompact(ctx, cfg, client, apiKey, hc, history)
	if after := estimateTokens(*history); after != before {
		dvlog.Logf("pre-send compact %dtok->%dtok", before, after)
	}
	var msg openai.ChatCompletionMessage
	var usage openai.Usage
	var err error
	switch modelTransport(cfg.Model) {
	case "messages":
		msg, usage, err = runMessagesTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
	case "responses":
		msg, usage, err = runResponsesTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
	default:
		msg, usage, err = runChatTurn(ctx, cfg, client, apiKey, hc, registry, history, onEvent)
	}
	dvlog.Logf("turn end dur=%s err=%v in=%d out=%d total=%d reply=%dB",
		dvlog.Dur(time.Since(t0)), err, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, len(msg.Content))
	return msg, usage, err
}

// chatSendMessages builds the wire copy without modifying stored history.
// It is also the last line of defense against poisoned history: the client
// serializes empty `content` as an absent key, which Anthropic-backed
// providers reject (400 "text content parts must carry a string text") on
// every later turn. So no message leaves here with blank content unless it
// legitimately carries tool_calls.
func chatSendMessages(history []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(history))
	for _, m := range history {
		blank := strings.TrimSpace(m.Content) == "" && len(m.MultiContent) == 0
		switch m.Role {
		case openai.ChatMessageRoleTool:
			m.Content = fitSendBudget(m.Content)
		case openai.ChatMessageRoleUser:
			if blank {
				m.Content = "(empty message)"
			}
		case openai.ChatMessageRoleAssistant:
			if blank && len(m.ToolCalls) == 0 {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}
