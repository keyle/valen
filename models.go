package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/sashabaranov/go-openai"
)

// fallbackGoModels is the curated OpenCode Go catalog (from /models, live Oct 2
// 2026) used when the live list cannot be reached. Entries tagged non-chat
// need a different transport (/responses, /messages) and will not work on the
// chat endpoint — see modelTransport.
var fallbackGoModels = []string{
	"glm-5.3-flash",
	"glm-5.3",
	"glm-5.2",
	"kimi-k3",
	"kimi-k2.7-code",
	"kimi-k2.6",
	"longcat-2.0",
	"longcat-2.5-preview-free",
	"deepseek-v4.1-flash",
	"deepseek-v4-pro",
	"deepseek-v4-flash",
	"deepseek-v4-flash-vision-exp",
	"mimo-v2.6-flash",
	"mimo-v2.6-pro",
	"mimo-v2.5",
	"mimo-v2.5-pro",
	"hy4-preview",
	"hy3",
	"space-bunny-free",
	"minimax-m3",
	"minimax-m2.7",
	"qwen3.8-max",
	"qwen3.8-flash",
	"qwen3.7-plus",
	"grok-4.7",
	"grok-4.6",
	"gpt-6-luna",
	"gpt-5.6-luna",
	"muse-spark-1.3-contributor",
	"muse-spark-1.2-contributor",
}

// modelTransport reports which API transport serves a model ID, matching the
// live Go endpoint table (Oct 2 2026): "chat" (/chat/completions, OpenAI
// chat), "responses" (/responses, OpenAI Responses) or "messages" (/messages,
// Anthropic Messages).
func modelTransport(id string) string {
	l := strings.ToLower(strings.TrimSpace(id))
	switch {
	case strings.HasPrefix(l, "grok-"), strings.HasPrefix(l, "gpt-"), strings.HasPrefix(l, "muse-spark-"):
		return "responses"
	case strings.HasPrefix(l, "minimax-"), strings.HasPrefix(l, "qwen"):
		return "messages"
	default:
		return "chat"
	}
}

// chatCompatible reports whether mods can talk to the model.
func chatCompatible(id string) bool { return modelTransport(id) == "chat" }

// thinkingFamily groups models by how their thinking level is expressed
// outbound: "effort" models take reasoning_effort, "toggle" models take
// enable_thinking in chat template kwargs.
func thinkingFamily(model string) string {
	l := strings.ToLower(strings.TrimSpace(model))
	for _, p := range []string{"kimi", "mimo", "qwen", "minimax", "muse-spark"} {
		if strings.HasPrefix(l, p) {
			return "toggle"
		}
	}
	return "effort"
}

// standardThinkingLevels is the full reasoning_effort vocabulary for models
// outside the known families.
var standardThinkingLevels = []string{"none", "low", "medium", "high", "xhigh", "max"}

// AvailableThinkingLevels lists the thinking levels a model accepts.
// Empty thinking (provider default) is always allowed on top of these.
func AvailableThinkingLevels(model string) []string {
	if thinkingFamily(model) == "toggle" {
		return []string{"none", "high"}
	}
	l := strings.ToLower(strings.TrimSpace(model))
	for _, p := range []string{"glm", "deepseek", "longcat", "hy", "grok", "gpt"} {
		if strings.HasPrefix(l, p) {
			return []string{"high", "max"}
		}
	}
	return append([]string(nil), standardThinkingLevels...)
}

// validThinkingLevel reports whether level is selectable for model.
// Empty means provider default and is always valid.
func validThinkingLevel(model, level string) bool {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return true
	}
	for _, l := range AvailableThinkingLevels(model) {
		if l == level {
			return true
		}
	}
	return false
}

// applyThinkingToRequest stamps the thinking level onto an outbound request.
// No-op when level is empty (provider default), mirroring the temperature
// pointer semantics: unset omits the field.
func applyThinkingToRequest(req *openai.ChatCompletionRequest, model, level string) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return
	}
	if thinkingFamily(model) == "toggle" {
		if req.ChatTemplateKwargs == nil {
			req.ChatTemplateKwargs = map[string]any{}
		}
		req.ChatTemplateKwargs["enable_thinking"] = level != "none"
		return
	}
	req.ReasoningEffort = level
}

// fetchGoModels returns the live OpenCode Go model IDs, sorted.
func fetchGoModels(ctx context.Context, client *openai.Client) ([]string, error) {
	list, err := client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list.Models))
	for _, m := range list.Models {
		if strings.TrimSpace(m.ID) != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// knownGoModel reports whether id is in the curated catalog (case-insensitive).
func knownGoModel(id string) bool {
	for _, m := range fallbackGoModels {
		if strings.EqualFold(m, strings.TrimSpace(id)) {
			return true
		}
	}
	return false
}

// matchGoModel resolves arg against ids: exact (case-insensitive) match wins,
// otherwise a case-insensitive substring search. It returns the matched IDs.
func matchGoModel(ids []string, arg string) []string {
	arg = strings.TrimSpace(arg)
	for _, id := range ids {
		if strings.EqualFold(id, arg) {
			return []string{id}
		}
	}
	var out []string
	lower := strings.ToLower(arg)
	for _, id := range ids {
		if strings.Contains(strings.ToLower(id), lower) {
			out = append(out, id)
		}
	}
	return out
}

// persistModelSelection writes the selected model/thinking back to the config
// file, preserving comments and every other key. An empty thinking removes the
// line so the provider default applies again.
func persistModelSelection(path, model, thinking string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	model = strings.TrimSpace(model)
	thinking = strings.ToLower(strings.TrimSpace(thinking))
	modelDone, thinkDone := false, false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if isKeyLine(strings.TrimSpace(line), "model") {
			lines[i] = fmt.Sprintf("model = %q", model)
			modelDone = true
		}
	}
	if !modelDone {
		lines = append(lines, fmt.Sprintf("model = %q", model))
	}
	if thinking == "" {
		kept := lines[:0]
		for _, line := range lines {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") && isKeyLine(strings.TrimSpace(line), "thinking") {
				continue
			}
			kept = append(kept, line)
		}
		lines = kept
	} else {
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if isKeyLine(strings.TrimSpace(line), "thinking") {
				lines[i] = fmt.Sprintf("thinking = %q", thinking)
				thinkDone = true
			}
		}
		if !thinkDone {
			lines = append(lines, fmt.Sprintf("thinking = %q", thinking))
		}
	}
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return os.WriteFile(path, []byte(out), info.Mode().Perm())
}

func isKeyLine(trimmed, key string) bool {
	if !strings.HasPrefix(trimmed, key) {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, key))
	return strings.HasPrefix(rest, "=")
}
