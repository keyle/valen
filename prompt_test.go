package main

import (
	"context"
	"strings"
	"testing"

	"valen/internal/prompts"
)

// effectiveSystem wires Config to the prompts package: family file, env
// header, plan overlay, or the explicit override.
func TestEffectiveSystemEnvAndOverride(t *testing.T) {
	cfg := Config{Model: "muse-spark-1.3-contributor"}
	got := effectiveSystem(cfg)
	for _, want := range []string{"Muse Spark", "muse-spark-1.3-contributor", "<env>", "Working directory:", "todowrite", "write/edit"} {
		if !strings.Contains(got, want) {
			t.Fatalf("system prompt missing %q", want)
		}
	}
	if strings.Contains(got, "{{MODEL_NAME}}") || strings.Contains(got, "{{MODEL_ID}}") {
		t.Fatal("placeholders must be replaced")
	}
	off := effectiveSystem(Config{Model: "muse-spark-1.3-contributor"})
	on := effectiveSystem(Config{Model: "muse-spark-1.3-contributor", Plan: true})
	if strings.Contains(off, "Plan Mode") {
		t.Fatal("build prompt must not contain the plan overlay")
	}
	if !strings.Contains(on, "Plan Mode") || !strings.Contains(on, "READ-ONLY") {
		t.Fatal("plan prompt must append the read-only overlay")
	}
	cfg.System = "custom override"
	if got := effectiveSystem(cfg); got != "custom override" {
		t.Fatalf("override=%q", got)
	}
	if strings.TrimSpace(prompts.BuildSwitch()) == "" {
		t.Fatal("build-switch notice must not be empty")
	}
}

func TestPlanModeBlocksMutations(t *testing.T) {
	pctx := prompts.WithPlanMode(context.Background())
	if !prompts.IsPlanMode(pctx) {
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
