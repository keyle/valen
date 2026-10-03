package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

// Agent guidelines, opencode-adapted and valen-tuned. One file per model
// family (mirroring opencode's session/prompt/*.txt); the plan overlay and
// build-switch notice implement plan mode.

var (
	//go:embed prompt_meta.txt
	promptMeta string
	//go:embed prompt_default.txt
	promptDefault string
	//go:embed prompt_anthropic.txt
	promptAnthropic string
	//go:embed prompt_beast.txt
	promptBeast string
	//go:embed prompt_codex.txt
	promptCodex string
	//go:embed prompt_copilot.txt
	promptCopilot string
	//go:embed prompt_gemini.txt
	promptGemini string
	//go:embed prompt_gpt.txt
	promptGPT string
	//go:embed prompt_gpt_astra.txt
	promptGPTAstra string
	//go:embed prompt_kimi.txt
	promptKimi string
	//go:embed prompt_trinity.txt
	promptTrinity string
	//go:embed prompt_plan.txt
	promptPlan string
	//go:embed prompt_build_switch.txt
	promptBuildSwitch string
)

// promptForModel selects the agent guidelines per model family, mirroring
// opencode's system.ts routing: muse→meta, claude→anthropic, old
// reasoning (gpt-4/o1/o3)→beast, gpt-6→astra, gpt/grok→gpt, codex→codex,
// copilot→copilot, gemini→gemini, kimi→kimi, trinity→trinity, else default.
func promptForModel(model string) string {
	l := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(l, "muse"):
		return promptMeta
	case strings.Contains(l, "claude"), strings.Contains(l, "anthropic"):
		return promptAnthropic
	case strings.Contains(l, "trinity"):
		return promptTrinity
	case strings.Contains(l, "codex"):
		return promptCodex
	case strings.Contains(l, "copilot"):
		return promptCopilot
	case strings.Contains(l, "gpt-4"), hasOSeries(l):
		return promptBeast
	case strings.Contains(l, "gpt-6"):
		return promptGPTAstra
	case strings.Contains(l, "gpt"), strings.Contains(l, "grok"):
		return promptGPT
	case strings.Contains(l, "gemini"):
		return promptGemini
	case strings.Contains(l, "kimi"):
		return promptKimi
	default:
		return promptDefault
	}
}

// hasOSeries matches opencode's o1/o3 routing without catching substrings
// like "mimo" or "protocol": token-boundary match on o1/o3 variants.
func hasOSeries(l string) bool {
	for _, tok := range strings.FieldsFunc(l, func(r rune) bool {
		return r == '-' || r == '_' || r == '/' || r == ' ' || r == ':'
	}) {
		if tok == "o1" || tok == "o3" || strings.HasPrefix(tok, "o1-") || strings.HasPrefix(tok, "o3-") {
			return true
		}
	}
	return strings.HasPrefix(l, "o1") || strings.HasPrefix(l, "o3")
}

// friendlyModelName mirrors opencode's {{MODEL_NAME}} for the muse family.
func friendlyModelName(model string) string {
	if strings.Contains(strings.ToLower(model), "muse-glimmer") {
		return "Muse Glimmer"
	}
	if strings.Contains(strings.ToLower(model), "muse") {
		return "Muse Spark"
	}
	return model
}

// systemPrompt builds the full system message: a small env header (like
// opencode's environment block) plus the family prompt file, with the plan
// overlay appended in plan mode. cfg.System still wins when set, so local
// overrides stay reversible.
func systemPrompt(cfg Config) string {
	if strings.TrimSpace(cfg.System) != "" {
		return cfg.System
	}
	body := promptForModel(cfg.Model)
	body = strings.ReplaceAll(body, "{{MODEL_NAME}}", friendlyModelName(cfg.Model))
	body = strings.ReplaceAll(body, "{{MODEL_ID}}", cfg.Model)
	if cfg.Plan {
		body += "\n\n" + promptPlan
	}
	cwd := getFileRoot()
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	header := fmt.Sprintf("You are powered by the model named %s. The exact model ID is %s.\n<env>\n  Working directory: %s\n  Platform: %s\n  Today's date: %s\n</env>\n\n",
		friendlyModelName(cfg.Model), cfg.Model, cwd, runtime.GOOS, time.Now().Format("Mon Jan 02 2006"))
	return header + body
}

// effectiveSystem returns the configured system prompt, or the harness
// prompt files when none is set.
func effectiveSystem(cfg Config) string {
	return systemPrompt(cfg)
}

// seedHistory builds the initial history for a turn.
func seedHistory(cfg Config) []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: effectiveSystem(cfg)}}
}

type planCtxKey struct{}

// withPlanMode marks ctx read-only so tool Fns can refuse mutations.
func withPlanMode(ctx context.Context) context.Context {
	return context.WithValue(ctx, planCtxKey{}, true)
}

// isPlanMode reports whether ctx carries the plan-mode read-only mark.
func isPlanMode(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(planCtxKey{}).(bool)
	return v
}

// planRefusal is the tool output when plan mode blocks a mutation. Returned
// as a Go error so Registry.Run formats it as tool output (not a turn
// failure) — the model sees it and replans instead of the turn dying.
func planRefusal(tool string) string {
	return fmt.Sprintf("plan mode is read-only — %s is blocked. Investigate with read/glob/grep/web/task(explore), present the plan as your reply, and let the user exit plan mode to execute.", tool)
}
