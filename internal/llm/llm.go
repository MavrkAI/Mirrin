// Package llm is the provider-neutral model gateway.
package llm

import (
	"context"
	"encoding/json"
)

// Role of a conversation message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType discriminates content blocks.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

// Block is one piece of message content.
type Block struct {
	Type BlockType `json:"type"`
	// Text for text blocks and tool results.
	Text string `json:"text,omitempty"`
	// ToolUseID links a tool_use to its tool_result.
	ToolUseID string `json:"tool_use_id,omitempty"`
	// ToolName and Input are set on tool_use blocks.
	ToolName string          `json:"tool_name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	// IsError marks a failed tool_result.
	IsError bool `json:"is_error,omitempty"`
	// Image is an optional picture attached to a tool_result (a screenshot the
	// model should look at). Transient: not persisted with history.
	Image     []byte `json:"-"`
	ImageType string `json:"-"` // image/png or image/jpeg
	// Path is where an image block's picture is kept, relative to the data
	// folder (image.go).
	Path string `json:"path,omitempty"`
}

// Message is one turn in a conversation.
type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

// Text is a convenience constructor for a plain text message.
func Text(role Role, text string) Message {
	return Message{Role: role, Blocks: []Block{{Type: BlockText, Text: text}}}
}

// PlainText concatenates the text blocks of a message.
func (m Message) PlainText() string {
	out := ""
	for _, b := range m.Blocks {
		if b.Type == BlockText {
			if out != "" {
				out += "\n"
			}
			out += b.Text
		}
	}
	return out
}

// ToolSpec describes a tool the model may call.
type ToolSpec struct {
	Name        string
	Description string
	// Schema is a JSON Schema object: {"type":"object","properties":{...},"required":[...]}.
	Schema map[string]any
}

// Request is a single model call.
type Request struct {
	// System is the stable part of the system prompt; providers cache it.
	System string
	// SystemVolatile changes often (time, fresh memory) and sits after the cache point.
	SystemVolatile string
	Messages       []Message
	Tools          []ToolSpec
	MaxTokens      int
	Effort         string
}

// StopReason reports why the model stopped.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopOther     StopReason = "other"
)

// Response is the model's reply.
type Response struct {
	Message      Message
	StopReason   StopReason
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	// CacheWrite counts input tokens written to the prompt cache (Anthropic).
	CacheWrite int64
}

// Provider is implemented by each model backend.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (*Response, error)
}

// Streamer is implemented by providers that can deliver text as it is generated.
// onDelta receives text fragments in order; the returned Response is complete.
type Streamer interface {
	Stream(ctx context.Context, req Request, onDelta func(text string)) (*Response, error)
}

// CompleteStreaming uses Stream when the provider supports it, else Complete.
func CompleteStreaming(ctx context.Context, p Provider, req Request, onDelta func(string)) (*Response, error) {
	if s, ok := p.(Streamer); ok && onDelta != nil {
		return s.Stream(ctx, req, onDelta)
	}
	resp, err := p.Complete(ctx, req)
	if err == nil && onDelta != nil {
		if t := resp.Message.PlainText(); t != "" {
			onDelta(t)
		}
	}
	return resp, err
}
