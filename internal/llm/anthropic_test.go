package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func TestAnthropicToolSchemaKeepsRequiredAndExtras(t *testing.T) {
	// As an MCP server's schema arrives: decoded into map[string]any.
	var schema map[string]any
	_ = json.Unmarshal([]byte(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",
		"properties":{"path":{"type":"string"},"opts":{"$ref":"#/$defs/opts"},"mode":{"anyOf":[{"type":"string"},{"type":"null"}]}},
		"required":["path"],"additionalProperties":false,"$defs":{"opts":{"type":"object"}},
		"oneOf":[{"required":["path"]},{"required":["opts"]}],"anyOf":[{}],"allOf":[{}],"not":{},"if":{},"then":{},"else":{}}`), &schema)
	a := &Anthropic{model: "claude-opus-5"}
	p := a.params(Request{Messages: []Message{Text(RoleUser, "hi")}, Tools: []ToolSpec{
		{Name: "fs__read", Description: "read", Schema: schema},
		{Name: "echo", Description: "echo", Schema: map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{"s"}}},
	}})
	var got []map[string]any
	for _, tool := range p.Tools {
		b, err := json.Marshal(tool.OfTool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		got = append(got, m)
	}
	if r, _ := got[0]["required"].([]any); len(r) != 1 || r[0] != "path" {
		t.Fatalf("MCP required fields lost: %v", got[0])
	}
	if got[0]["additionalProperties"] != false || got[0]["$defs"] == nil || got[0]["type"] != "object" {
		t.Fatalf("schema keys dropped: %v", got[0])
	}
	for _, k := range []string{"$schema", "oneOf", "anyOf", "allOf", "not", "if", "then", "else"} {
		if _, ok := got[0][k]; ok {
			t.Fatalf("%s at the top level would be refused by the API: %v", k, got[0])
		}
	}
	if props, _ := got[0]["properties"].(map[string]any); props["mode"] == nil {
		t.Fatalf("nested schemas should be sent as they are: %v", got[0])
	}
	if r, _ := got[1]["required"].([]any); len(r) != 1 || r[0] != "s" {
		t.Fatalf("built-in required fields lost: %v", got[1])
	}
}

func TestAnthropicLargeMaxTokensStillCompletes(t *testing.T) {
	var streamed, plain int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			plain++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"short"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
			return
		}
		streamed++
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`message_start`, `{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":1}}}`,
			`content_block_start`, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`content_block_delta`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a long "}}`,
			`content_block_delta`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}`,
			`content_block_stop`, `{"type":"content_block_stop","index":0}`,
			`message_delta`, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
			`message_stop`, `{"type":"message_stop"}`,
		} {
			if strings.HasPrefix(ev, "{") {
				fmt.Fprintf(w, "data: %s\n\n", ev)
			} else {
				fmt.Fprintf(w, "event: %s\n", ev)
			}
		}
	}))
	defer srv.Close()
	a := &Anthropic{client: anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("k"), option.WithMaxRetries(0)), model: "claude-opus-5"}

	resp, err := a.Complete(context.Background(), Request{Messages: []Message{Text(RoleUser, "write it all")}, MaxTokens: 32000})
	if err != nil {
		t.Fatalf("a large max_tokens should still complete: %v", err)
	}
	if resp.Message.PlainText() != "a long answer" || resp.StopReason != StopEndTurn || streamed != 1 {
		t.Fatalf("got %+v (streamed %d)", resp, streamed)
	}
	if resp, err := a.Complete(context.Background(), Request{Messages: []Message{Text(RoleUser, "hi")}, MaxTokens: 1000}); err != nil || resp.Message.PlainText() != "short" || plain != 1 {
		t.Fatalf("small calls stay plain: %v %+v", err, resp)
	}
}
