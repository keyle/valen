package prompts

import (
	"context"
	"strings"
	"testing"
)

func TestForModelFamily(t *testing.T) {
	meta := ForModel("muse-spark-1.3-contributor")
	if !strings.Contains(meta, "valen") || !strings.Contains(meta, "{{MODEL_NAME}}") {
		t.Fatalf("meta prompt missing valen identity or placeholder")
	}
	def := ForModel("gpt-5-luna")
	if !strings.Contains(def, "valen") {
		t.Fatalf("default prompt missing valen identity")
	}
	if meta == def {
		t.Fatal("meta and default prompts must differ")
	}
	if ForModel("MUSE-GLIMMER") != meta {
		t.Fatal("muse match must be case-insensitive")
	}
}

func TestRoutingTable(t *testing.T) {
	cases := map[string]string{
		"muse-spark-1.3":    meta,
		"claude-opus-4":     anthropic,
		"anthropic-x":       anthropic,
		"trinity-1":         trinity,
		"gpt-codex":         codex,
		"copilot-gpt-5":     copilot,
		"gpt-4o":            beast,
		"o1-preview":        beast,
		"my-o3-mini":        beast,
		"gpt-6-luna":        gptAstra,
		"gpt-5.6-luna":      gpt,
		"grok-4.7":          gpt,
		"gemini-2.5-pro":    gemini,
		"kimi-k3":           kimi,
		"glm-5.3-flash":     def,
		"deepseek-v4-pro":   def,
		"mimo-v2.6-pro":     def,
		"minimax-m3":        def,
		"qwen3.8-max":       def,
		"longcat-2.0":       def,
		"something-unknown": def,
	}
	for model, want := range cases {
		if got := ForModel(model); got != want {
			t.Fatalf("model %q routed to wrong prompt", model)
		}
	}
	if ForModel("mimo-v2.6-pro") == beast {
		t.Fatal("mimo must not route to beast")
	}
	if hasOSeries("mimo-v2.6-pro") || hasOSeries("protocol-x") {
		t.Fatal("o-series matcher too greedy")
	}
}

func TestSystemPromptEnvAndPlan(t *testing.T) {
	got := SystemPrompt("muse-spark-1.3-contributor", false, "/work")
	for _, want := range []string{"Muse Spark", "muse-spark-1.3-contributor", "<env>", "Working directory:", "/work", "todowrite", "write/edit"} {
		if !strings.Contains(got, want) {
			t.Fatalf("system prompt missing %q", want)
		}
	}
	if strings.Contains(got, "{{MODEL_NAME}}") || strings.Contains(got, "{{MODEL_ID}}") {
		t.Fatal("placeholders must be replaced")
	}
	if strings.Contains(got, "Plan Mode") {
		t.Fatal("build prompt must not contain the plan overlay")
	}
	on := SystemPrompt("muse-spark-1.3-contributor", true, "/work")
	if !strings.Contains(on, "Plan Mode") || !strings.Contains(on, "READ-ONLY") {
		t.Fatal("plan prompt must append the read-only overlay")
	}
	if strings.TrimSpace(BuildSwitch()) == "" {
		t.Fatal("build-switch notice must not be empty")
	}
}

func TestFilesValenTuned(t *testing.T) {
	files := map[string]string{
		"meta": meta, "default": def, "anthropic": anthropic,
		"beast": beast, "codex": codex, "copilot": copilot,
		"gemini": gemini, "gpt": gpt, "astra": gptAstra,
		"kimi": kimi, "trinity": trinity,
	}
	for file, body := range files {
		for _, want := range []string{"2–4", "todowrite", "NEVER commit"} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s prompt missing %q", file, want)
			}
		}
		if !strings.Contains(body, "(not cat") {
			t.Fatalf("%s prompt missing file-tools-over-shell rule", file)
		}
		if strings.Contains(strings.ToLower(body), "apply_patch") {
			t.Fatalf("%s prompt references opencode-only apply_patch", file)
		}
	}
}

func TestPlanCtx(t *testing.T) {
	pctx := WithPlanMode(context.Background())
	if !IsPlanMode(pctx) || IsPlanMode(context.Background()) || IsPlanMode(nil) {
		t.Fatal("plan ctx marking broken")
	}
	if !strings.Contains(Refusal("write"), "read-only") {
		t.Fatal("refusal must name read-only mode")
	}
}
