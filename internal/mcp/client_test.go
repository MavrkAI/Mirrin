package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// TestMain doubles as a tiny MCP server when MIRRIN_MCP_TESTSERVER is set.
// Otherwise it gives the package a throwaway MIRRIN_HOME: starting a server
// resolves "$NAME" env values (procenv.Expand), which falls back to the
// home's secrets.env.
func TestMain(m *testing.M) {
	if os.Getenv("MIRRIN_MCP_TESTSERVER") == "1" {
		runTestServer()
		return
	}
	home, err := os.MkdirTemp("", "mirrin-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("MIRRIN_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func runTestServer() {
	in := bufio.NewReader(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil || req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "testsrv", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "shout", "description": "Uppercase text", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}}}}
		case "tools/call":
			var p struct {
				Name string            `json:"name"`
				Args map[string]string `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			text := "SHOUT: " + p.Args["text"]
			if p.Name == "getenv" { // lets a test see the server's environment
				text = "[" + os.Getenv(p.Args["name"]) + "]"
			}
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		default:
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "no such method"}})
			continue
		}
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
}

func TestClientAgainstTestServer(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := Start(ctx, "t", exe, nil, map[string]string{"MIRRIN_MCP_TESTSERVER": "1"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.ServerName != "testsrv" {
		t.Fatalf("handshake: %q", c.ServerName)
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "shout" {
		t.Fatalf("tools: %v %v", tools, err)
	}
	out, isErr, err := c.CallTool(ctx, "shout", json.RawMessage(`{"text":"hello"}`))
	if err != nil || isErr || out != "SHOUT: hello" {
		t.Fatalf("call: %q %v %v", out, isErr, err)
	}
	if err := c.call(ctx, "nope", nil, nil); err == nil {
		t.Fatal("expected error for unknown method")
	}
}

func TestServerGetsNoDaemonSecrets(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-leak")
	t.Setenv("MY_GITHUB_TOKEN", "ghp_passed_on_purpose")
	ctx := context.Background()
	c, err := Start(ctx, "t", exe, nil, map[string]string{"MIRRIN_MCP_TESTSERVER": "1", "GITHUB_TOKEN": "$MY_GITHUB_TOKEN", "MODE": "plain"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for name, want := range map[string]string{
		"ANTHROPIC_API_KEY": "[]",
		"MY_GITHUB_TOKEN":   "[]",
		"GITHUB_TOKEN":      "[ghp_passed_on_purpose]",
		"MODE":              "[plain]",
	} {
		out, _, err := c.CallTool(ctx, "getenv", json.RawMessage(`{"name":"`+name+`"}`))
		if err != nil || out != want {
			t.Errorf("%s: got %q (%v), want %q", name, out, err, want)
		}
	}
}
