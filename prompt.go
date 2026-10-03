package main

import (
	_ "embed"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

//go:embed prompt_meta.txt
var promptMeta string

//go:embed prompt_default.txt
var promptDefault string

// promptForModel selects the agent guidelines per model family, mirroring
// opencode's system.ts: muse-family models get the meta prompt, everything
// else gets the default prompt.
func promptForModel(model string) string {
	if strings.Contains(strings.ToLower(model), "muse") {
		return promptMeta
	}
	return promptDefault
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
// opencode's environment block) plus the family prompt file. cfg.System
// still wins when set, so local overrides stay reversible.
func systemPrompt(cfg Config) string {
	if strings.TrimSpace(cfg.System) != "" {
		return cfg.System
	}
	body := promptForModel(cfg.Model)
	body = strings.ReplaceAll(body, "{{MODEL_NAME}}", friendlyModelName(cfg.Model))
	body = strings.ReplaceAll(body, "{{MODEL_ID}}", cfg.Model)
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
