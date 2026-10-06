package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// OpenAICompat speaks the OpenAI chat-completions dialect, which OpenAI,
// Google Gemini (its OpenAI-compatible endpoint), Ollama and most local
// servers all accept. One file, three providers.
type OpenAICompat struct {
	name    string
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// NewOpenAICompat builds a provider. name is for display ("openai", "gemini", "ollama").
func NewOpenAICompat(name, baseURL, apiKey, model string) *OpenAICompat {
	return &OpenAICompat{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Timeout: 10 * time.Minute},
	}
}

func (o *OpenAICompat) Name() string { return o.name + "/" + o.model }

// Known endpoints.
const (
	OpenAIBaseURL = "https://api.openai.com/v1"
	GeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	OllamaBaseURL = "http://127.0.0.1:11434/v1"
)

type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content,omitempty"` // string, or []map[string]any for text+image parts
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (o *OpenAICompat) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.baseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("%s %d: %s", o.name, resp.StatusCode, truncate(msg, 300))
	}
	return json.Unmarshal(data, out)
}

// Complete runs one chat-completions call.
func (o *OpenAICompat) Complete(ctx context.Context, req Request) (*Response, error) {
	body := o.body(req)
	var out struct {
		Choices []struct {
			Message      oaMessage `json:"message"`
			FinishReason string    `json:"finish_reason"`
		} `json:"choices"`
		Usage oaUsage `json:"usage"`
	}
	if err := o.do(ctx, http.MethodPost, "/chat/completions", body, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, errors.New(o.name + ": empty response")
	}
	ch := out.Choices[0]
	content, _ := ch.Message.Content.(string)
	return o.toResponse(content, ch.Message.ToolCalls, ch.FinishReason, out.Usage), nil
}

// Stream runs one chat-completions call with SSE, delivering text as it arrives.
func (o *OpenAICompat) Stream(ctx context.Context, req Request, onDelta func(string)) (*Response, error) {
	body := o.body(req)
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if o.apiKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.http.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("%s %d: %s", o.name, resp.StatusCode, truncate(strings.TrimSpace(string(data)), 300))
	}
	var content strings.Builder
	calls := map[int]*oaToolCall{}
	order := []int{}
	finish := ""
	var usage oaUsage
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *oaUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				content.WriteString(c.Delta.Content)
				onDelta(c.Delta.Content)
			}
			for _, tc := range c.Delta.ToolCalls {
				cur, ok := calls[tc.Index]
				if !ok {
					cur = &oaToolCall{Type: "function"}
					calls[tc.Index] = cur
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Function.Name += tc.Function.Name
				}
				cur.Function.Arguments += tc.Function.Arguments
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	var toolCalls []oaToolCall
	for _, i := range order {
		toolCalls = append(toolCalls, *calls[i])
	}
	return o.toResponse(content.String(), toolCalls, finish, usage), nil
}

// oaUsage is what a chat completion used. prompt_tokens counts every input
// token, cached or not; prompt_tokens_details.cached_tokens says how many of
// them were read from the prompt cache, which costs less.
type oaUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// tokens splits the prompt into uncached input and cache reads, as
// Anthropic reports them, so a price's CacheRead applies to the cached part.
func (u oaUsage) tokens() (input, cacheRead, output int64) {
	cached := min(max(u.PromptTokensDetails.CachedTokens, 0), u.PromptTokens)
	return u.PromptTokens - cached, cached, u.CompletionTokens
}

func (o *OpenAICompat) toResponse(content string, toolCalls []oaToolCall, finish string, u oaUsage) *Response {
	inTok, cached, outTok := u.tokens()
	resp := &Response{Message: Message{Role: RoleAssistant}, InputTokens: inTok, OutputTokens: outTok, CacheRead: cached}
	if content != "" {
		resp.Message.Blocks = append(resp.Message.Blocks, Block{Type: BlockText, Text: content})
	}
	for i, tc := range toolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		resp.Message.Blocks = append(resp.Message.Blocks, Block{Type: BlockToolUse, ToolUseID: id, ToolName: tc.Function.Name, Input: json.RawMessage(args)})
	}
	switch {
	case len(toolCalls) > 0:
		resp.StopReason = StopToolUse
	case finish == "length":
		resp.StopReason = StopMaxTokens
	case finish == "content_filter":
		resp.StopReason = StopRefusal
	default:
		resp.StopReason = StopEndTurn
	}
	return resp
}

// body builds the chat-completions request.
func (o *OpenAICompat) body(req Request) map[string]any {
	msgs := []oaMessage{}
	if sys := strings.TrimSpace(req.System + "\n\n" + req.SystemVolatile); sys != "" {
		msgs = append(msgs, oaMessage{Role: "system", Content: sys})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleAssistant:
			am := oaMessage{Role: "assistant"}
			for _, b := range m.Blocks {
				switch b.Type {
				case BlockText:
					if cur, _ := am.Content.(string); cur != "" {
						am.Content = cur + "\n" + b.Text
					} else {
						am.Content = b.Text
					}
				case BlockToolUse:
					tc := oaToolCall{ID: b.ToolUseID, Type: "function"}
					tc.Function.Name = b.ToolName
					tc.Function.Arguments = string(b.Input)
					if tc.Function.Arguments == "" {
						tc.Function.Arguments = "{}"
					}
					am.ToolCalls = append(am.ToolCalls, tc)
				}
			}
			msgs = append(msgs, am)
		default:
			var text string
			var photos []Block // pictures the user sent (image.go)
			// Tool messages must directly follow the assistant's tool_calls, so
			// screenshots (which only a user message can carry) go after all of them.
			var shots []oaMessage
			for _, b := range m.Blocks {
				switch b.Type {
				case BlockText:
					if text != "" {
						text += "\n"
					}
					text += b.Text
				case BlockImage:
					photos = append(photos, b)
				case BlockToolResult:
					content := b.Text
					if b.IsError {
						content = "ERROR: " + content
					}
					msgs = append(msgs, oaMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: content})
					if len(b.Image) > 0 {
						mt := b.ImageType
						if mt == "" {
							mt = "image/png"
						}
						shots = append(shots, oaMessage{Role: "user", Content: []map[string]any{
							{"type": "text", "text": "Screenshot from tool call " + b.ToolUseID + ":"},
							{"type": "image_url", "image_url": map[string]string{"url": "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(b.Image)}},
						}})
					}
				}
			}
			msgs = append(msgs, shots...)
			if um := userContent(text, photos); um.Content != "" {
				msgs = append(msgs, um)
			}
		}
	}

	body := map[string]any{"model": o.model, "messages": msgs}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8000
	}
	if strings.Contains(o.baseURL, "api.openai.com") {
		body["max_completion_tokens"] = maxTokens
	} else {
		body["max_tokens"] = maxTokens
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Schema,
				},
			})
		}
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	return body
}

// ListModels returns model ids the endpoint offers, sorted.
func (o *OpenAICompat) ListModels(ctx context.Context) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := o.do(ctx, http.MethodGet, "/models", nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

// ModelLister is implemented by providers that can enumerate models.
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
