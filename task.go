package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sashabaranov/go-openai"
	"valen/internal/prompts"
)

// task.go: foreground-only subagent delegation (opencode-style `task`).
//
// Chunky work bloats main history because every explore lands there at up to
// 16k per result. A subagent runs the same tool loop in an isolated history
// and only its final summary folds into the main turn, so research costs one
// 2k-budgeted tool result instead of rounds × full outputs. Foreground-only:
// no background execution (deferred); max nesting depth 2.

const (
	// taskMaxRounds caps subagent loops so a flailing delegate can't burn
	// the session's whole round budget.
	taskMaxRounds = 25
	// taskMaxDepth caps task-in-task nesting (general delegates only).
	taskMaxDepth = 2
)

type taskDepthKey struct{}

// taskDepth reports the current delegation nesting (0 in a main turn).
func taskDepth(ctx context.Context) int {
	if d, ok := ctx.Value(taskDepthKey{}).(int); ok {
		return d
	}
	return 0
}

func withTaskDepth(ctx context.Context, d int) context.Context {
	return context.WithValue(ctx, taskDepthKey{}, d)
}

// taskTools lists the builtin subsets per subagent kind. Explore is
// read-only; general can act but never asks questions, plans, or delegates
// further than the depth guard allows (task itself is excluded: nesting is
// tracked by context, and one level is enough for v1).
var (
	taskExploreTools = []string{"read", "glob", "grep", "web_search", "web_fetch"}
	taskGeneralTools = []string{"read", "write", "edit", "glob", "grep", "shell", "web_search", "web_fetch"}
)

func taskSystem(kind string) string {
	if kind == "general" {
		return "You are a general-purpose subagent. Complete the assigned step with the tools and return a concise summary of what you did and produced (facts, file paths, commands run). Do not ask questions."
	}
	return "You are a read-only research subagent. Gather facts with read/glob/grep/web tools only — never edit, write, or run shell commands. Return a concise factual summary with file:line references."
}

// TaskTool delegates prompt to a same-model subagent via exec. exec is wired
// in start() where client/config live; tests stub it.
func TaskTool(exec func(ctx context.Context, prompt, kind string) (string, error)) Tool {
	return Tool{
		Name:        "task",
		Description: "Delegate a self-contained subtask to a same-model subagent and get back a summary. Only the summary lands in main history, keeping exploration out of the main context. Kinds: explore (read-only research: glob/grep/read/web) for locating code and facts; general (may read, write, edit, run shell) for carrying out one bounded step. Pass a specific prompt with the goal, scope, and what to return. Max nesting depth 2.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"description":{"type":"string","description":"short label for the subtask"},"prompt":{"type":"string","description":"self-contained subtask prompt: goal, scope, what to return"},"subagent_type":{"type":"string","description":"explore (default) or general"}},"required":["prompt"]}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			var a struct {
				Description string `json:"description"`
				Prompt      string `json:"prompt"`
				Subagent    string `json:"subagent_type"`
			}
			if err := json.Unmarshal([]byte(normalizeArgs(args)), &a); err != nil {
				return "", fmt.Errorf("bad arguments: %w", err)
			}
			if strings.TrimSpace(a.Prompt) == "" {
				return "", fmt.Errorf("prompt is required")
			}
			kind := strings.ToLower(strings.TrimSpace(a.Subagent))
			if kind == "" {
				kind = "explore"
			}
			if kind != "explore" && kind != "general" {
				return "", fmt.Errorf("subagent_type must be explore or general")
			}
			if prompts.IsPlanMode(ctx) && kind != "explore" {
				return "", fmt.Errorf("%s", prompts.Refusal("task(general)"))
			}
			if taskDepth(ctx) >= taskMaxDepth {
				return "", fmt.Errorf("task: max delegation depth %d reached", taskMaxDepth)
			}
			if exec == nil {
				return "", fmt.Errorf("task: subagent runner not wired")
			}
			return exec(ctx, a.Prompt, kind)
		},
	}
}

// runTask executes one foreground subagent turn with an isolated history and
// a restricted tool subset. Silent (nil onEvent): the main timeline shows one
// task row; subagent chatter would defeat the context saving.
func runTask(ctx context.Context, cfg Config, client *openai.Client, apiKey string, hc *http.Client, prompt, kind string) (string, error) {
	tools := taskExploreTools
	if kind == "general" {
		tools = taskGeneralTools
	}
	sub := NewRegistry()
	sub.LoadBuiltins(tools)

	subCfg := cfg
	subCfg.MaxRounds = min(cfg.MaxRounds, taskMaxRounds)
	if subCfg.MaxRounds <= 0 {
		subCfg.MaxRounds = taskMaxRounds
	}

	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: taskSystem(kind)},
		{Role: openai.ChatMessageRoleUser, Content: prompt},
	}
	msg, _, err := runTurn(withTaskDepth(ctx, taskDepth(ctx)+1), subCfg, client, apiKey, hc, sub, &history, nil)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(msg.Content) == "" {
		return "", fmt.Errorf("task: subagent returned no summary")
	}
	return msg.Content, nil
}
