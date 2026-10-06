package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// retiring serves an Anthropic-style API where one model has been retired.
func retiring(t *testing.T, retired string) (*httptest.Server, func() map[string]int) {
	t.Helper()
	var mu sync.Mutex
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := strings.TrimPrefix(r.URL.Path, "/v1/models/")
		stream := false
		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			model, _ = body["model"].(string)
			stream = body["stream"] == true
		}
		mu.Lock()
		asked[model]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if model == retired {
			w.WriteHeader(404)
			fmt.Fprintf(w, `{"type":"error","error":{"type":"not_found_error","message":"model: %s"}}`, model)
			return
		}
		if r.Method == http.MethodGet {
			fmt.Fprintf(w, `{"id":%q,"type":"model","display_name":"x"}`, model)
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range [][2]string{
				{"message_start", `{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":1}}}`},
				{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
				{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"still here"}}`},
				{"content_block_stop", `{"type":"content_block_stop","index":0}`},
				{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`},
				{"message_stop", `{"type":"message_stop"}`},
			} {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev[0], ev[1])
			}
			return
		}
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"still here"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`, model)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	return srv, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range asked {
			out[k] = v
		}
		return out
	}
}

// The regression: a model id the provider retired failed every turn.
func TestRetiredModelFallsBackToTheProvidersDefault(t *testing.T) {
	_, asked := retiring(t, "claude-3-opus-20240229")
	p, err := NewWithFallback(ProviderSettings{Provider: "anthropic", Model: "claude-3-opus-20240229", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		resp, err := p.Complete(context.Background(), Request{Messages: []Message{Text(RoleUser, "hi")}, MaxTokens: 100})
		if err != nil {
			t.Fatalf("turn %d: want an answer from the default model, got %v", i+1, err)
		}
		if resp.Message.PlainText() != "still here" {
			t.Fatalf("got %q", resp.Message.PlainText())
		}
	}
	if got := asked(); got["claude-3-opus-20240229"] != 1 || got[DefaultModel("anthropic")] != 2 {
		t.Fatalf("the retired model should be tried once, then left alone: %v", got)
	}
	provider, from, to, ok := FallbackNews(p)
	if !ok || provider != "anthropic" || from != "claude-3-opus-20240229" || to != DefaultModel("anthropic") {
		t.Fatalf("news = %q %q %q %v", provider, from, to, ok)
	}
	if _, _, _, again := FallbackNews(p); again {
		t.Fatal("the fallback should be reported once")
	}
	if p.Name() != "anthropic/"+DefaultModel("anthropic") {
		t.Fatalf("name should be the model in use, got %q", p.Name())
	}
}

func TestStreamingFallsBackToo(t *testing.T) {
	retiring(t, "claude-2.1")
	p, _ := NewWithFallback(ProviderSettings{Provider: "anthropic", Model: "claude-2.1", APIKey: "k"})
	var got strings.Builder
	resp, err := CompleteStreaming(context.Background(), p, Request{Messages: []Message{Text(RoleUser, "hi")}, MaxTokens: 100}, func(s string) { got.WriteString(s) })
	if err != nil || resp.Message.PlainText() != "still here" || got.String() != "still here" {
		t.Fatalf("stream: %v %q", err, got.String())
	}
}

func TestHealthCheckOfARetiredModelPassesOnTheDefault(t *testing.T) {
	retiring(t, "claude-3-opus-20240229")
	p, _ := NewWithFallback(ProviderSettings{Provider: "anthropic", Model: "claude-3-opus-20240229", APIKey: "k"})
	if err := Check(context.Background(), p); err != nil {
		t.Fatalf("the twin works on the default, so the check passes: %v", err)
	}
	if configured, using, fell := p.(*Fallback).Using(); !fell || configured != "claude-3-opus-20240229" || using != DefaultModel("anthropic") {
		t.Fatalf("using %q for %q (%v)", using, configured, fell)
	}
	if _, _, _, ok := FallbackNews(p); !ok {
		t.Fatal("the owner should hear of it")
	}
}

func TestOtherFailuresDontFallBack(t *testing.T) {
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m, _ := body["model"].(string)
		models = append(models, m)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	defer srv.Close()
	p, _ := NewWithFallback(ProviderSettings{Provider: "openai", Model: "gpt-4o", APIKey: "bad", BaseURL: srv.URL})
	if _, err := p.Complete(context.Background(), Request{Messages: []Message{Text(RoleUser, "hi")}}); err == nil {
		t.Fatal("a bad key must still fail")
	}
	if len(models) != 1 || models[0] != "gpt-4o" {
		t.Fatalf("a bad key is not a retired model: tried %v", models)
	}
}

func TestNoFallbackWhereItCantHelp(t *testing.T) {
	for _, s := range []ProviderSettings{
		{Provider: "ollama", Model: "llama3"},
		{Provider: "openai-compatible", Model: "x", BaseURL: "http://127.0.0.1:1"},
		{Provider: "anthropic", Model: DefaultModel("anthropic"), APIKey: "k"},
	} {
		p, err := NewWithFallback(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := p.(*Fallback); ok {
			t.Errorf("%s/%s: nothing to fall back to", s.Provider, s.Model)
		}
	}
}

func TestRetired(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{errors.New(`anthropic 404: {"type":"error","error":{"type":"not_found_error","message":"model: claude-3-opus-20240229"}}`), true},
		{errors.New("openai 404: The model `gpt-4-32k` does not exist or you do not have access to it."), true},
		{errors.New("gemini 404: models/gemini-1.0-pro is not found for API version v1main"), true},
		{errors.New("openai 400: The model `text-davinci-003` has been deprecated"), true},
		{&UserError{Msg: "plain", Err: errors.New(`anthropic 404: {"error":{"message":"model: x"}}`)}, true},
		{errors.New("ollama 404: model 'llama3' not found, try pulling it first"), false},
		{errors.New("custom 404: model not found"), false},
		{errors.New(`anthropic 404: {"error":{"message":"page not found"}}`), false},
		{errors.New("openai 429: rate limit reached for model gpt-4.1"), false},
		{errors.New("anthropic 500: model overloaded"), false},
		{nil, false},
	} {
		if got := Retired(c.err); got != c.want {
			t.Errorf("Retired(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
