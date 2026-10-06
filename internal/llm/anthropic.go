package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Anthropic is the Claude provider.
type Anthropic struct {
	client anthropic.Client
	model  string
}

// NewAnthropic builds a Claude-backed provider. An empty apiKey falls back to
// ANTHROPIC_API_KEY or an `ant auth login` profile.
func NewAnthropic(apiKey, model string) *Anthropic {
	opts := []option.RequestOption{}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if model == "" {
		model = "claude-opus-5"
	}
	return &Anthropic{client: anthropic.NewClient(opts...), model: model}
}

func (a *Anthropic) Name() string { return "anthropic/" + a.model }

// Complete runs one Messages API call.
func (a *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	params := a.params(req)
	// The SDK refuses a plain call whose max_tokens could take over ten
	// minutes (above about 21k); stream those and hand back the whole message.
	if _, err := anthropic.CalculateNonStreamingTimeout(int(params.MaxTokens), params.Model, nil); err != nil {
		return a.stream(ctx, params, nil)
	}
	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return fromMessage(resp), nil
}

// Stream runs one Messages API call, delivering text as it arrives.
func (a *Anthropic) Stream(ctx context.Context, req Request, onDelta func(string)) (*Response, error) {
	return a.stream(ctx, a.params(req), onDelta)
}

// stream runs a streaming call; onDelta may be nil.
func (a *Anthropic) stream(ctx context.Context, params anthropic.MessageNewParams, onDelta func(string)) (*Response, error) {
	stream := a.client.Messages.NewStreaming(ctx, params)
	message := anthropic.Message{}
	for stream.Next() {
		event := stream.Current()
		if err := message.Accumulate(event); err != nil {
			return nil, err
		}
		if ev, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok && onDelta != nil {
			if d, ok := ev.Delta.AsAny().(anthropic.TextDelta); ok && d.Text != "" {
				onDelta(d.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, wrapErr(err)
	}
	return fromMessage(&message), nil
}

func wrapErr(err error) error {
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		msg := apierr.RawJSON()
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		if msg == "" {
			msg = apierr.Error()
		}
		return fmt.Errorf("anthropic %d: %s", apierr.StatusCode, msg)
	}
	return err
}

func (a *Anthropic) params(req Request) anthropic.MessageNewParams {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  toParams(req.Messages),
	}
	if params.MaxTokens == 0 {
		params.MaxTokens = 16000
	}
	if req.System != "" {
		params.System = append(params.System, anthropic.TextBlockParam{
			Text:         req.System,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		})
	}
	if req.SystemVolatile != "" {
		params.System = append(params.System, anthropic.TextBlockParam{Text: req.SystemVolatile})
	}
	if req.Effort != "" && supportsEffort(a.model) {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(req.Effort)}
	}
	for _, t := range req.Tools {
		tool := anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: inputSchema(t.Schema),
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &tool})
	}
	return params
}

// inputSchema maps a JSON Schema object onto the SDK's type. "required"
// arrives as []any from schemas decoded off the wire (MCP servers), and the
// top-level keys in schemaExtras ride along as extra fields. Anything else at
// the top level is dropped: the API refuses oneOf, anyOf and allOf there, and
// one MCP server publishing them would otherwise break every request.
func inputSchema(s map[string]any) anthropic.ToolInputSchemaParam {
	out := anthropic.ToolInputSchemaParam{}
	for k, v := range s {
		switch {
		case k == "properties":
			out.Properties = v
		case k == "required":
			switch r := v.(type) {
			case []string:
				out.Required = r
			case []any:
				for _, x := range r {
					if name, ok := x.(string); ok {
						out.Required = append(out.Required, name)
					}
				}
			}
		case schemaExtras[k]:
			if out.ExtraFields == nil {
				out.ExtraFields = map[string]any{}
			}
			out.ExtraFields[k] = v
		}
	}
	return out
}

// schemaExtras are the top-level schema keys passed through besides
// properties and required. Nested schemas are sent as they are.
var schemaExtras = map[string]bool{"$defs": true, "definitions": true, "additionalProperties": true, "description": true, "title": true}

func fromMessage(resp *anthropic.Message) *Response {
	out := &Response{
		Message:      Message{Role: RoleAssistant},
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		CacheRead:    resp.Usage.CacheReadInputTokens,
		CacheWrite:   resp.Usage.CacheCreationInputTokens,
	}
	for _, block := range resp.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			out.Message.Blocks = append(out.Message.Blocks, Block{Type: BlockText, Text: v.Text})
		case anthropic.ToolUseBlock:
			out.Message.Blocks = append(out.Message.Blocks, Block{
				Type:      BlockToolUse,
				ToolUseID: v.ID,
				ToolName:  v.Name,
				Input:     json.RawMessage(v.JSON.Input.Raw()),
			})
		}
	}
	switch resp.StopReason {
	case anthropic.StopReasonEndTurn:
		out.StopReason = StopEndTurn
	case anthropic.StopReasonToolUse:
		out.StopReason = StopToolUse
	case anthropic.StopReasonMaxTokens:
		out.StopReason = StopMaxTokens
	case anthropic.StopReasonRefusal:
		out.StopReason = StopRefusal
		if resp.StopDetails.Explanation != "" {
			out.Message.Blocks = append(out.Message.Blocks, Block{Type: BlockText, Text: "I'm afraid I can't help with that: " + resp.StopDetails.Explanation})
		}
	default:
		out.StopReason = StopOther
	}
	return out
}

func toParams(msgs []Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			switch b.Type {
			case BlockText:
				if b.Text == "" {
					continue
				}
				blocks = append(blocks, anthropic.NewTextBlock(b.Text))
			case BlockImage:
				if img, ok := imageParam(b); ok {
					blocks = append(blocks, img)
				}
			case BlockToolUse:
				var input any
				if err := json.Unmarshal(b.Input, &input); err != nil || input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(b.ToolUseID, input, b.ToolName))
			case BlockToolResult:
				if len(b.Image) == 0 {
					blocks = append(blocks, anthropic.NewToolResultBlock(b.ToolUseID, b.Text, b.IsError))
					continue
				}
				mt := b.ImageType
				if mt == "" {
					mt = "image/png"
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{OfToolResult: &anthropic.ToolResultBlockParam{
					ToolUseID: b.ToolUseID,
					IsError:   anthropic.Bool(b.IsError),
					Content: []anthropic.ToolResultBlockParamContentUnion{
						{OfText: &anthropic.TextBlockParam{Text: b.Text}},
						{OfImage: &anthropic.ImageBlockParam{Source: anthropic.ImageBlockParamSourceUnion{OfBase64: &anthropic.Base64ImageSourceParam{
							Data: base64.StdEncoding.EncodeToString(b.Image), MediaType: anthropic.Base64ImageSourceMediaType(mt)}}}},
					},
				}})
			}
		}
		if len(blocks) == 0 {
			continue
		}
		if m.Role == RoleAssistant {
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		} else {
			out = append(out, anthropic.NewUserMessage(blocks...))
		}
	}
	return out
}

// supportsEffort reports whether the model accepts output_config.effort.
// Haiku 4.5 and older 3.x/4.0/4.1 models reject it with a 400.
func supportsEffort(model string) bool {
	m := strings.ToLower(model)
	if strings.Contains(m, "haiku") || strings.Contains(m, "claude-3") {
		return false
	}
	for _, old := range []string{"-4-0", "-4-1", "opus-4-20", "sonnet-4-20"} {
		if strings.Contains(m, old) {
			return false
		}
	}
	return true
}
