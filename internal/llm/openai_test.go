package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompatToolRoundTrip(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"b-model"},{"id":"a-model"}]}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		msgs := got["messages"].([]any)
		if len(msgs) == 2 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"echo","arguments":"{\"s\":\"hi\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"It said hi."},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":4}}`))
	}))
	defer srv.Close()
	p := NewOpenAICompat("test", srv.URL, "k", "m")
	req := Request{System: "sys", SystemVolatile: "now", Messages: []Message{Text(RoleUser, "say hi")},
		Tools: []ToolSpec{{Name: "echo", Description: "e", Schema: map[string]any{"type": "object"}}}}
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopToolUse || resp.Message.Blocks[0].ToolName != "echo" || string(resp.Message.Blocks[0].Input) != `{"s":"hi"}` {
		t.Fatalf("bad tool call: %+v", resp.Message)
	}
	if got["messages"].([]any)[0].(map[string]any)["content"] != "sys\n\nnow" {
		t.Fatalf("system prompt not merged: %v", got["messages"].([]any)[0])
	}
	// Second turn: tool result goes back as a tool message.
	req.Messages = append(req.Messages, resp.Message, Message{Role: RoleUser, Blocks: []Block{{Type: BlockToolResult, ToolUseID: "c1", Text: "echoed hi"}}})
	resp, err = p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopEndTurn || resp.Message.PlainText() != "It said hi." {
		t.Fatalf("bad final: %+v", resp)
	}
	msgs := got["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "c1" || last["content"] != "echoed hi" {
		t.Fatalf("tool result not sent as tool message: %v", last)
	}
	asst := msgs[len(msgs)-2].(map[string]any)
	if asst["tool_calls"] == nil {
		t.Fatalf("assistant tool_calls missing: %v", asst)
	}
	models, err := p.ListModels(context.Background())
	if err != nil || len(models) != 2 || models[0] != "a-model" {
		t.Fatalf("models: %v %v", models, err)
	}
}

func TestOpenAICompatScreenshotFollowsEveryToolMessage(t *testing.T) {
	p := NewOpenAICompat("t", "http://x", "", "m")
	body := p.body(Request{Messages: []Message{
		Text(RoleUser, "look at both"),
		{Role: RoleAssistant, Blocks: []Block{
			{Type: BlockToolUse, ToolUseID: "c1", ToolName: "screenshot_page", Input: json.RawMessage(`{}`)},
			{Type: BlockToolUse, ToolUseID: "c2", ToolName: "fetch_url", Input: json.RawMessage(`{}`)},
		}},
		{Role: RoleUser, Blocks: []Block{
			{Type: BlockToolResult, ToolUseID: "c1", Text: "screenshot: /d/s.png", Image: []byte("png"), ImageType: "image/png"},
			{Type: BlockToolResult, ToolUseID: "c2", Text: "page text"},
		}},
	}})
	b, _ := json.Marshal(body["messages"])
	var msgs []map[string]any
	_ = json.Unmarshal(b, &msgs)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m["role"].(string))
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tool,tool,user" {
		t.Fatalf("tool messages must directly follow the tool calls; got %s", got)
	}
	if msgs[2]["tool_call_id"] != "c1" || msgs[3]["tool_call_id"] != "c2" {
		t.Fatalf("tool results out of order: %v", msgs)
	}
	if !strings.Contains(string(b), "Screenshot from tool call c1") {
		t.Fatal("screenshot should say which call it belongs to")
	}
}

func TestOpenAICompatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			http.Error(w, "expected stream", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo. "}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c9","function":{"name":"ec","arguments":"{\"s\""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"ho","arguments":":\"x\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
		} {
			_, _ = w.Write([]byte("data: " + d + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	p := NewOpenAICompat("t", srv.URL, "", "m")
	var got string
	resp, err := p.Stream(context.Background(), Request{Messages: []Message{Text(RoleUser, "hi")}}, func(d string) { got += d })
	if err != nil {
		t.Fatal(err)
	}
	if got != "Hello. " || resp.Message.Blocks[0].Text != "Hello. " {
		t.Fatalf("deltas: %q", got)
	}
	tu := resp.Message.Blocks[1]
	if tu.Type != BlockToolUse || tu.ToolUseID != "c9" || tu.ToolName != "echo" || string(tu.Input) != `{"s":"x"}` {
		t.Fatalf("tool call not accumulated: %+v", tu)
	}
	if resp.StopReason != StopToolUse || resp.InputTokens != 7 {
		t.Fatalf("stop/usage: %v %d", resp.StopReason, resp.InputTokens)
	}
}
