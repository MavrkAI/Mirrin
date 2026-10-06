// Package mcp is a minimal Model Context Protocol client over stdio. Any MCP
// server (filesystem, GitHub, Slack, a database, your own) becomes a set of
// Mirrin tools without writing Go.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MavrkAI/Mirrin/internal/procenv"
)

// ProtocolVersion is the MCP revision we speak.
const ProtocolVersion = "2025-06-18"

// Tool is a tool a server exposes.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Client talks to one server process.
type Client struct {
	name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader

	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	nextID  atomic.Int64
	closed  chan struct{}
	err     error

	ServerName    string
	ServerVersion string
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Method string `json:"method"` // set on notifications/requests from the server
}

// Start launches the server and performs the initialize handshake. The server
// sees a minimal environment plus env, never the daemon's own secrets; a
// value of "$NAME" passes the daemon's NAME through.
func Start(ctx context.Context, name, command string, args []string, env map[string]string) (*Client, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = procenv.Base()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+procenv.Expand(v))
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // servers log to stderr; keep it out of our stdout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %s: start %s: %w", name, command, err)
	}
	c := &Client{name: name, cmd: cmd, stdin: stdin, out: bufio.NewReaderSize(stdout, 1<<20), pending: map[int64]chan rpcResponse{}, closed: make(chan struct{})}
	go c.readLoop()

	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	ictx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err = c.call(ictx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mirrin", "version": "0.1"},
	}, &init)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: initialize: %w", name, err)
	}
	c.ServerName, c.ServerVersion = init.ServerInfo.Name, init.ServerInfo.Version
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) readLoop() {
	defer close(c.closed)
	for {
		line, err := c.out.ReadBytes('\n')
		if len(line) > 0 {
			var resp rpcResponse
			if json.Unmarshal(line, &resp) == nil && resp.ID != nil && resp.Method == "" {
				c.mu.Lock()
				ch, ok := c.pending[*resp.ID]
				delete(c.pending, *resp.ID)
				c.mu.Unlock()
				if ok {
					ch <- resp
				}
			}
			// notifications and server-initiated requests are ignored
		}
		if err != nil {
			c.mu.Lock()
			c.err = fmt.Errorf("mcp %s: server closed: %w", c.name, err)
			for id, ch := range c.pending {
				ch <- rpcResponse{Error: &struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				}{-1, "server closed"}}
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
	}
}

func (c *Client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *Client) notify(method string, params any) error {
	return c.send(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}

// ListTools fetches every tool the server offers.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var out struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Tools...)
		if out.NextCursor == "" {
			return all, nil
		}
		cursor = out.NextCursor
	}
}

// CallTool invokes a tool and returns its text content.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	var a any = map[string]any{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", false, fmt.Errorf("bad arguments: %w", err)
		}
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Data string `json:"data"`
			Mime string `json:"mimeType"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": a}, &out); err != nil {
		return "", false, err
	}
	var b strings.Builder
	for _, item := range out.Content {
		switch item.Type {
		case "text":
			b.WriteString(item.Text)
			b.WriteString("\n")
		case "image":
			fmt.Fprintf(&b, "[image %s, %d bytes base64]\n", item.Mime, len(item.Data))
		case "resource":
			b.WriteString("[resource]\n")
		}
	}
	return strings.TrimSpace(b.String()), out.IsError, nil
}

// Close stops the server.
func (c *Client) Close() {
	_ = c.stdin.Close()
	select {
	case <-c.closed:
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
}

// Err reports whether the server has died.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// ErrClosed is returned by tools after the server has gone away.
var ErrClosed = errors.New("mcp server closed")
