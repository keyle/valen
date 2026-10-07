package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"valen/internal/prompts"
)

const (
	mcpHandshakeTimeout = 30 * time.Second
	mcpCallTimeout      = 2 * time.Minute
	clientInfoName      = "valen"
	clientInfoVersion   = "0.1.0"
)

// LoadMCP starts each stdio server, registers its tools and returns the closers.
// Servers that fail to start do not abort the run; their error is reported and
// the remaining servers still load. Startup (process spawn + handshake +
// tools/list) runs in parallel across servers; registration into the registry
// stays serial in config order so tool order is deterministic.
func LoadMCP(ctx context.Context, servers []MCPServer, r *Registry, warn func(string, ...any)) []func() {
	if len(servers) < 2 {
		var closers []func()
		for _, s := range servers {
			stop, err := registerMCPServer(ctx, s, r)
			if err != nil {
				warn("mcp %s: %v", s.Name, err)
				continue
			}
			closers = append(closers, stop)
		}
		return closers
	}
	type fetched struct {
		client *client.Client
		tools  []mcp.Tool
		err    error
	}
	results := make([]fetched, len(servers))
	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Add(1)
		go func(i int, s MCPServer) {
			defer wg.Done()
			c, err := startMCPClient(ctx, s)
			if err != nil {
				results[i].err = err
				return
			}
			handshake, cancel := context.WithTimeout(ctx, mcpHandshakeTimeout)
			defer cancel()
			tools, err := c.ListTools(handshake, mcp.ListToolsRequest{})
			if err != nil {
				c.Close()
				results[i].err = fmt.Errorf("list tools: %w", err)
				return
			}
			results[i].client = c
			results[i].tools = tools.Tools
		}(i, s)
	}
	wg.Wait()
	var closers []func()
	for i, s := range servers {
		f := results[i]
		if f.err != nil {
			warn("mcp %s: %v", s.Name, f.err)
			continue
		}
		failed := false
		for _, t := range f.tools {
			if err := r.Add(mcpTool(s.Name, f.client, t)); err != nil {
				f.client.Close()
				warn("mcp %s: %v", s.Name, err)
				failed = true
				break
			}
		}
		if !failed {
			c := f.client
			closers = append(closers, func() { c.Close() })
		}
	}
	return closers
}

func registerMCPServer(ctx context.Context, s MCPServer, r *Registry) (func(), error) {
	c, err := startMCPClient(ctx, s)
	if err != nil {
		return nil, err
	}

	// The handshake gets its own deadline; the transport keeps the parent ctx so
	// cancelling the handshake does not tear down the connection.
	handshake, cancel := context.WithTimeout(ctx, mcpHandshakeTimeout)
	defer cancel()

	tools, err := c.ListTools(handshake, mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("list tools: %w", err)
	}
	for _, t := range tools.Tools {
		tool := mcpTool(s.Name, c, t)
		if err := r.Add(tool); err != nil {
			c.Close()
			return nil, err
		}
	}
	return func() { c.Close() }, nil
}

func startMCPClient(ctx context.Context, s MCPServer) (*client.Client, error) {
	if strings.TrimSpace(s.Cmd) == "" {
		return nil, errors.New("cmd is required")
	}
	env := os.Environ()
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	c := client.NewClient(transport.NewStdio(s.Cmd, env, s.Args...))
	if err := c.Start(ctx); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: clientInfoName, Version: clientInfoVersion}
	handshake, cancel := context.WithTimeout(ctx, mcpHandshakeTimeout)
	defer cancel()
	if _, err := c.Initialize(handshake, req); err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	return c, nil
}

func mcpTool(server string, c *client.Client, t mcp.Tool) Tool {
	name := server + "__" + t.Name
	schema, err := json.Marshal(t.InputSchema)
	if err != nil {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	return Tool{
		Name:        name,
		Description: t.Description,
		Schema:      schema,
		Fn: func(ctx context.Context, args string) (string, error) {
			// MCP tools are opaque (valen cannot tell a read from a write), so
			// plan mode refuses them all rather than risk a mutation.
			if prompts.IsPlanMode(ctx) {
				return "", fmt.Errorf("%s", prompts.Refusal(name))
			}
			var params map[string]any
			if err := json.Unmarshal([]byte(args), &params); err != nil {
				return "", fmt.Errorf("bad arguments: %w", err)
			}
			ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
			defer cancel()
			res, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: t.Name, Arguments: params}})
			if err != nil {
				return "", err
			}
			out := mcpResultText(res)
			if res.IsError {
				return "", errors.New(strings.TrimSpace(out))
			}
			return out, nil
		},
	}
}

func mcpResultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, item := range res.Content {
		switch v := item.(type) {
		case mcp.TextContent:
			b.WriteString(v.Text + "\n")
		case *mcp.TextContent:
			b.WriteString(v.Text + "\n")
		default:
			enc, err := json.Marshal(v)
			if err == nil {
				b.Write(enc)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}
