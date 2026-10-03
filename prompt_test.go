package main

import (
	"context"
	"strings"
	"testing"
)

func TestPromptForModelFamily(t *testing.T) {
	meta := promptForModel("muse-spark-1.3-contributor")
	if !strings.Contains(meta, "valen") || !strings.Contains(meta, "{{MODEL_NAME}}") {
		t.Fatalf("meta prompt missing valen identity or placeholder")
	}
	def := promptForModel("gpt-5-luna")
	if !strings.Contains(def, "valen") {
		t.Fatalf("default prompt missing valen identity")
	}
	if meta == def {
		t.Fatal("meta and default prompts must differ")
	}
	// Case-insensitive muse match.
	if promptForModel("MUSE-GLIMMER") != meta {
		t.Fatal("muse match must be case-insensitive")
	}
}

func TestPromptRoutingTable(t *testing.T) {
	cases := map[string]string{
		"muse-spark-1.3":    promptMeta,
		"claude-opus-4":     promptAnthropic,
		"anthropic-x":       promptAnthropic,
		"trinity-1":         promptTrinity,
		"gpt-codex":         promptCodex,
		"copilot-gpt-5":     promptCopilot,
		"gpt-4o":            promptBeast,
		"o1-preview":        promptBeast,
		"my-o3-mini":        promptBeast,
		"gpt-6-luna":        promptGPTAstra,
		"gpt-5.6-luna":      promptGPT,
		"grok-4.7":          promptGPT,
		"gemini-2.5-pro":    promptGemini,
		"kimi-k3":           promptKimi,
		"glm-5.3-flash":     promptDefault,
		"deepseek-v4-pro":   promptDefault,
		"mimo-v2.6-pro":     promptDefault,
		"minimax-m3":        promptDefault,
		"qwen3.8-max":       promptDefault,
		"longcat-2.0":       promptDefault,
		"something-unknown": promptDefault,
	}
	for model, want := range cases {
		if got := promptForModel(model); got != want {
			t.Fatalf("model %q routed to wrong prompt", model)
		}
	}
	// "mimo"/"protocol" must not trip the o1/o3 matcher.
	if promptForModel("mimo-v2.6-pro") == promptBeast {
		t.Fatal("mimo must not route to beast")
	}
	if hasOSeries("mimo-v2.6-pro") || hasOSeries("protocol-x") {
		t.Fatal("o-series matcher too greedy")
	}
}

func TestSystemPromptEnvAndOverride(t *testing.T) {
	cfg := Config{Model: "muse-spark-1.3-contributor"}
	got := systemPrompt(cfg)
	for _, want := range []string{"Muse Spark", "muse-spark-1.3-contributor", "<env>", "Working directory:", "todowrite", "write/edit"} {
		if !strings.Contains(got, want) {
			t.Fatalf("system prompt missing %q", want)
		}
	}
	if strings.Contains(got, "{{MODEL_NAME}}") || strings.Contains(got, "{{MODEL_ID}}") {
		t.Fatal("placeholders must be replaced")
	}
	cfg.System = "custom override"
	if got := systemPrompt(cfg); got != "custom override" {
		t.Fatalf("override=%q", got)
	}
}

func TestSystemPromptPlanOverlay(t *testing.T) {
	off := systemPrompt(Config{Model: "muse-spark-1.3-contributor"})
	on := systemPrompt(Config{Model: "muse-spark-1.3-contributor", Plan: true})
	if strings.Contains(off, "Plan Mode") {
		t.Fatal("build prompt must not contain the plan overlay")
	}
	if !strings.Contains(on, "Plan Mode") || !strings.Contains(on, "READ-ONLY") {
		t.Fatal("plan prompt must append the read-only overlay")
	}
	if strings.TrimSpace(promptBuildSwitch) == "" {
		t.Fatal("build-switch notice must not be empty")
	}
}

func TestPromptFilesValenTuned(t *testing.T) {
	files := map[string]string{
		"meta": promptMeta, "default": promptDefault, "anthropic": promptAnthropic,
		"beast": promptBeast, "codex": promptCodex, "copilot": promptCopilot,
		"gemini": promptGemini, "gpt": promptGPT, "astra": promptGPTAstra,
		"kimi": promptKimi, "trinity": promptTrinity,
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

func TestPlanModeBlocksMutations(t *testing.T) {
	pctx := withPlanMode(context.Background())
	if !isPlanMode(pctx) || isPlanMode(context.Background()) || isPlanMode(nil) {
		t.Fatal("plan ctx marking broken")
	}
	if _, err := WriteTool().Fn(pctx, `{"path":"a.txt","content":"x"}`); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("write in plan mode: %v", err)
	}
	if _, err := EditTool().Fn(pctx, `{"path":"a.txt","oldString":"x","newString":"y"}`); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("edit in plan mode: %v", err)
	}
	if _, err := ShellTool().Fn(pctx, `{"command":"ls"}`); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("shell in plan mode: %v", err)
	}
	if _, err := TaskTool(nil).Fn(pctx, `{"prompt":"x","subagent_type":"general"}`); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("task(general) in plan mode: %v", err)
	}
	// Read-only tools stay open.
	if _, err := ReadTool().Fn(context.Background(), `{"path":"go.mod"}`); err != nil {
		t.Skipf("go.mod not readable here: %v", err)
	}
}

func TestApplyPlanModeToggles(t *testing.T) {
	m := newTestModel(t, testConfig("https://unused"))
	before := m.history[0].Content
	m.applyPlanMode(true)
	if !m.cfg.Plan {
		t.Fatal("plan flag not set")
	}
	if m.history[0].Content == before || !strings.Contains(m.history[0].Content, "Plan Mode") {
		t.Fatal("system message must gain the plan overlay")
	}
	m.applyPlanMode(false)
	if m.cfg.Plan {
		t.Fatal("plan flag not cleared")
	}
	if strings.Contains(m.history[0].Content, "Plan Mode") {
		t.Fatal("system message must drop the plan overlay")
	}
	if !strings.Contains(m.headerView(), "✳ valen") {
		t.Fatalf("header=%q", m.headerView())
	}
}
