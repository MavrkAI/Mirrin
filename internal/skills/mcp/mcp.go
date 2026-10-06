// Package mcp turns MCP servers into Mirrin tools.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	mcpclient "github.com/MavrkAI/Mirrin/internal/mcp"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

var reName = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// Server is a running MCP server and its tools.
type Server struct {
	cfg    config.MCPServer
	client *mcpclient.Client
	Tools  []tools.Tool
}

// Connect starts every enabled server and builds its tools. Failures are
// logged and skipped so one broken server never takes Mirrin down.
func Connect(ctx context.Context, servers []config.MCPServer, log *slog.Logger) []*Server {
	if log == nil {
		log = slog.Default()
	}
	var out []*Server
	for _, sc := range servers {
		if sc.Enabled != nil && !*sc.Enabled {
			continue
		}
		if sc.Name == "" || sc.Command == "" {
			log.Warn("mcp server needs name and command", "server", sc.Name)
			continue
		}
		client, err := mcpclient.Start(ctx, sc.Name, sc.Command, sc.Args, sc.Env)
		if err != nil {
			log.Warn("mcp server failed to start", "server", sc.Name, "err", err)
			continue
		}
		list, err := client.ListTools(ctx)
		if err != nil {
			log.Warn("mcp tools/list failed", "server", sc.Name, "err", err)
			client.Close()
			continue
		}
		s := &Server{cfg: sc, client: client}
		for _, t := range list {
			s.Tools = append(s.Tools, s.tool(t))
		}
		log.Info("mcp server connected", "server", sc.Name, "name", client.ServerName, "tools", len(s.Tools))
		out = append(out, s)
	}
	return out
}

func (s *Server) risk(toolName string) tools.Risk {
	level := s.cfg.Risk
	if r, ok := s.cfg.ToolRisk[toolName]; ok {
		level = r
	}
	switch strings.ToLower(level) {
	case "read":
		return tools.RiskRead
	case "dangerous":
		return tools.RiskDangerous
	default:
		return tools.RiskWrite
	}
}

func (s *Server) tool(t mcpclient.Tool) tools.Tool {
	name := reName.ReplaceAllString(s.cfg.Name+"__"+t.Name, "_")
	if len(name) > 64 {
		name = name[:64]
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	if len(t.InputSchema) > 0 {
		var parsed map[string]any
		if json.Unmarshal(t.InputSchema, &parsed) == nil && parsed != nil {
			schema = parsed
			if _, ok := schema["type"]; !ok {
				schema["type"] = "object"
			}
			if _, ok := schema["properties"]; !ok {
				schema["properties"] = map[string]any{}
			}
		}
	}
	desc := t.Description
	if desc == "" {
		desc = t.Name
	}
	desc = fmt.Sprintf("[%s] %s", s.cfg.Name, desc)
	spec := llm.ToolSpec{Name: name, Description: desc, Schema: schema}
	return &mcpTool{spec: spec, risk: s.risk(t.Name), server: s, remote: t.Name}
}

type mcpTool struct {
	spec   llm.ToolSpec
	risk   tools.Risk
	server *Server
	remote string
}

func (m *mcpTool) Spec() llm.ToolSpec { return m.spec }
func (m *mcpTool) Risk() tools.Risk   { return m.risk }
func (m *mcpTool) Run(ctx context.Context, call tools.Call) (string, error) {
	if err := m.server.client.Err(); err != nil {
		return "", err
	}
	out, isErr, err := m.server.client.CallTool(ctx, m.remote, call.Input)
	if err != nil {
		return "", err
	}
	if isErr {
		return "", fmt.Errorf("%s", out)
	}
	if out == "" {
		out = "(no output)"
	}
	return out, nil
}

// Close stops the server.
func (s *Server) Close() { s.client.Close() }
