package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTaskValidation: prompt required, kind restricted to explore/general,
// depth guard stops nesting past the max.
func TestTaskValidation(t *testing.T) {
	tool := TaskTool(func(ctx context.Context, prompt, kind string) (string, error) {
		return kind + ":" + prompt, nil
	})
	if _, err := tool.Fn(context.Background(), `{}`); err == nil {
		t.Fatal("missing prompt must fail")
	}
	if _, err := tool.Fn(context.Background(), `{"prompt":"hi","subagent_type":"boss"}`); err == nil {
		t.Fatal("bad kind must fail")
	}
	out, err := tool.Fn(context.Background(), `{"prompt":"find it"}`)
	if err != nil || out != "explore:find it" {
		t.Fatalf("default kind must be explore: %q %v", out, err)
	}
	deep := withTaskDepth(context.Background(), taskMaxDepth)
	if _, err := tool.Fn(deep, `{"prompt":"hi"}`); err == nil {
		t.Fatal("depth guard must fire past the max")
	}
	if _, err := TaskTool(nil).Fn(context.Background(), `{"prompt":"hi"}`); err == nil {
		t.Fatal("unwired runner must fail")
	}
}

// TestTaskExploreIsReadOnly: the explore subset carries no mutating tools.
func TestTaskExploreIsReadOnly(t *testing.T) {
	for _, name := range []string{"shell", "write", "edit", "task", "AskUserQuestion"} {
		for _, have := range taskExploreTools {
			if have == name {
				t.Fatalf("explore must not include %q", name)
			}
		}
	}
}

// TestTaskDelegatesViaLoop: runTask drives a real tool loop against a fake
// server with an isolated history — only the summary returns.
func TestTaskDelegatesViaLoop(t *testing.T) {
	srv, _ := fakeServer(t, completion(nil, "found it in a.go:12"))
	cfg := newTestConfig(srv.URL)
	got, err := runTask(context.Background(), cfg, newClient(cfg, "key"), "key", srv.Client(), "find the thing", "explore")
	if err != nil {
		t.Fatal(err)
	}
	if got != "found it in a.go:12" {
		t.Fatalf("summary=%q", got)
	}
}

// TestTaskPropagatesErrors: provider failures surface instead of an empty
// summary.
func TestTaskPropagatesErrors(t *testing.T) {
	fastRetry(t)
	badCfg := newTestConfig("http://127.0.0.1:1")
	if _, err := runTask(context.Background(), badCfg, newClient(badCfg, "key"), "key", nil, "x", "explore"); err == nil {
		t.Fatal("unreachable backend must fail")
	}
	if !strings.Contains(taskSystem("explore"), "read-only") || !strings.Contains(taskSystem("general"), "summary") {
		t.Fatal("subagent systems must state their contract")
	}
}

func TestTaskInheritsInstructionsAndReturnsVisibleSummary(t *testing.T) {
	var system string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &req); err != nil || len(req.Messages) == 0 {
			t.Errorf("bad request: %s", raw)
			return
		}
		system = req.Messages[0].Content
		_, _ = w.Write(completion(nil, "<think>private deliberation</think>found a.go:12"))
	}))
	defer srv.Close()
	cfg := newTestConfig(srv.URL)
	cfg.System = "Project rule: never commit changes."
	out, err := runTask(context.Background(), cfg, nil, "key", srv.Client(), "find it", "explore")
	if err != nil || out != "found a.go:12" || !strings.Contains(system, cfg.System) || !strings.Contains(system, "read-only") {
		t.Fatalf("out=%q err=%v system=%q", out, err, system)
	}
}
