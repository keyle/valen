package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const fixtureEnv = "VALEN_MCP_FIXTURE"

func TestMain(m *testing.M) {
	if os.Getenv(fixtureEnv) == "1" {
		if err := serveFixture(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	roundRetryBase, roundRetryMax = time.Millisecond, 2*time.Millisecond
	os.Exit(m.Run())
}

// serveFixture runs this test binary as a one-tool stdio MCP server.
func serveFixture() error {
	s := server.NewMCPServer("echo-fixture", "0.0.1")
	s.AddTool(
		mcp.NewTool("echo", mcp.WithDescription("Echo text back"), mcp.WithString("text", mcp.Required())),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			text := req.GetString("text", "")
			if text == "fail" {
				return mcp.NewToolResultError("tool said no"), nil
			}
			return mcp.NewToolResultText("echo:" + text), nil
		},
	)
	return server.ServeStdio(s)
}

func fixtureServer() MCPServer {
	return MCPServer{Name: "echo", Cmd: os.Args[0], Env: map[string]string{fixtureEnv: "1"}}
}

func collectWarns() (*[]string, func(string, ...any)) {
	var warns []string
	return &warns, func(format string, args ...any) { warns = append(warns, fmt.Sprintf(format, args...)) }
}

func TestLoadMCPRegistersAndCallsTools(t *testing.T) {
	registry := NewRegistry()
	warns, warn := collectWarns()
	closers := LoadMCP(context.Background(), []MCPServer{fixtureServer()}, registry, warn)
	defer closeAll(closers)

	if len(*warns) != 0 {
		t.Fatalf("unexpected warnings: %v", *warns)
	}
	if names := registry.Names(); len(names) != 1 || names[0] != "echo__echo" {
		t.Fatalf("names=%v", names)
	}
	tools := registry.OpenAITools()
	if tools[0].Function.Description != "Echo text back" || !json.Valid(mustMarshal(t, tools[0].Function.Parameters)) {
		t.Fatalf("tool=%+v", tools[0].Function)
	}

	if out := registry.Run(context.Background(), "echo__echo", `{"text":"ping"}`); !strings.Contains(out, "echo:ping") {
		t.Fatalf("out=%q", out)
	}
	if out := registry.Run(context.Background(), "echo__echo", `{"text":"fail"}`); !strings.HasPrefix(out, "error: tool said no") {
		t.Fatalf("out=%q", out)
	}
	if out := registry.Run(context.Background(), "echo__echo", `{`); !strings.HasPrefix(out, "error: bad arguments") {
		t.Fatalf("out=%q", out)
	}
}

func TestLoadMCPWarnsAndContinuesOnFailure(t *testing.T) {
	registry := NewRegistry()
	warns, warn := collectWarns()
	servers := []MCPServer{
		{Name: "broken", Cmd: "/definitely/not/a/binary"},
		fixtureServer(),
	}
	closers := LoadMCP(context.Background(), servers, registry, warn)
	defer closeAll(closers)

	if len(*warns) != 1 || !strings.Contains((*warns)[0], "mcp broken:") {
		t.Fatalf("warns=%v", *warns)
	}
	if names := registry.Names(); len(names) != 1 || names[0] != "echo__echo" {
		t.Fatalf("names=%v", names)
	}
	if len(closers) != 1 {
		t.Fatalf("closers=%d", len(closers))
	}
}

func TestLoadMCPNoServers(t *testing.T) {
	registry := NewRegistry()
	_, warn := collectWarns()
	if closers := LoadMCP(context.Background(), nil, registry, warn); len(closers) != 0 {
		t.Fatalf("closers=%d", len(closers))
	}
}

func TestMCPResultText(t *testing.T) {
	res := mcp.NewToolResultText("hello")
	res.Content = append(res.Content, mcp.NewImageContent("ZGF0YQ==", "image/png"))
	out := mcpResultText(res)
	if !strings.Contains(out, "hello") || !strings.Contains(out, "image/png") {
		t.Fatalf("out=%q", out)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
