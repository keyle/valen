package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

const (
	shellTimeout  = 60 * time.Second
	maxToolOutput = 16000
)

// Tool is a function the model may call. Args arrive as the raw JSON string the
// model produced; the return value is fed back verbatim as the tool result.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Fn          func(ctx context.Context, args string) (string, error)
}

// Registry keeps tools in insertion order so requests are deterministic.
type Registry struct {
	names []string
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

func (r *Registry) Add(t Tool) error {
	if !validToolName(t.Name) {
		return fmt.Errorf("invalid tool name %q", t.Name)
	}
	if _, dup := r.tools[t.Name]; dup {
		return fmt.Errorf("duplicate tool %q", t.Name)
	}
	r.tools[t.Name] = t
	r.names = append(r.names, t.Name)
	return nil
}

func (r *Registry) Names() []string { return append([]string(nil), r.names...) }

func (r *Registry) Has(name string) bool {
	_, ok := r.tools[name]
	return ok
}

func (r *Registry) OpenAITools() []openai.Tool {
	out := make([]openai.Tool, 0, len(r.names))
	for _, n := range r.names {
		t := r.tools[n]
		out = append(out, openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: t.Name, Description: t.Description, Parameters: t.Schema}})
	}
	return out
}

func (r *Registry) Call(ctx context.Context, name, args string) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %s", name)
	}
	return t.Fn(ctx, normalizeArgs(args))
}

// Run is Call for the agent loop: failures become tool output, never a crash.
func (r *Registry) Run(ctx context.Context, name, args string) string {
	out, err := r.Call(ctx, name, args)
	if err != nil {
		out = "error: " + err.Error()
	}
	return truncate(out, maxToolOutput)
}

// LoadBuiltins registers the named builtin tools and returns unknown names.
func (r *Registry) LoadBuiltins(names []string) []string {
	builtins := map[string]func() Tool{"shell": ShellTool, "web_search": WebSearchTool, "web_fetch": WebFetchTool, "AskUserQuestion": AskUserQuestionTool, "read": ReadTool, "write": WriteTool, "edit": EditTool, "glob": GlobTool, "grep": GrepTool}
	var unknown []string
	for _, n := range names {
		f, ok := builtins[n]
		if !ok {
			unknown = append(unknown, n)
			continue
		}
		if err := r.Add(f()); err != nil {
			unknown = append(unknown, n)
		}
	}
	return unknown
}

var toolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func validToolName(name string) bool {
	return toolNameRe.MatchString(name)
}

// normalizeArgs tolerates models that omit arguments for no-arg tools.
func normalizeArgs(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := strings.ToValidUTF8(s[:max], "")
	return cut + fmt.Sprintf("\n...[truncated %d bytes]", len(s)-max)
}

func ShellTool() Tool {
	return Tool{
		Name:        "shell",
		Description: "Run a shell command and return its combined stdout and stderr",
		Schema:      json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"shell command to run"}},"required":["command"]}`),
		Fn: func(ctx context.Context, args string) (string, error) {
			var a struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			if strings.TrimSpace(a.Command) == "" {
				return "", errors.New("command is required")
			}
			ctx, cancel := context.WithTimeout(ctx, shellTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", a.Command)
			cmd.Dir = getFileRoot()
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			err := cmd.Run()
			out := strings.TrimSpace(buf.String())
			var exit *exec.ExitError
			switch {
			case errors.As(err, &exit):
				return fmt.Sprintf("%s\n[exit %d]", out, exit.ExitCode()), nil
			case err != nil:
				return "", err
			}
			return out, nil
		},
	}
}
