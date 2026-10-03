package main

import (
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

func TestPromptFilesValenTuned(t *testing.T) {
	for file, body := range map[string]string{"meta": promptMeta, "default": promptDefault} {
		for _, want := range []string{"2–4", "todowrite", "NEVER commit"} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s prompt missing %q", file, want)
			}
		}
		if strings.Contains(body, "read` (not cat") == false {
			t.Fatalf("%s prompt missing file-tools-over-shell rule", file)
		}
		if strings.Contains(body, "bash") && !strings.Contains(body, "bash)") {
			t.Fatalf("%s prompt must not reference opencode's bash tool", file)
		}
	}
}
