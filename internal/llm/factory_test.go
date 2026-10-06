package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestNewWithoutKeyExplainsWhereToGetOne(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	_, err := New(ProviderSettings{Provider: "openai"})
	var km *KeyMissingError
	if !errors.As(err, &km) || km.Env != "OPENAI_API_KEY" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "platform.openai.com/api-keys") {
		t.Fatalf("no pointer to the key page: %v", err)
	}
}

func TestCheckRejectsBadAnthropicKey(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("X-Api-Key") != "good" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"claude-opus-5","type":"model","display_name":"Claude Opus 5"}`))
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	err := Check(context.Background(), NewAnthropic("sk-ant-bogus", "claude-opus-5"))
	if err == nil {
		t.Fatal("a bogus key passed the check")
	}
	if msg, ok := Describe(err); !ok || !strings.Contains(msg, "didn't accept the API key") {
		t.Fatalf("describe(%v) = %q", err, msg)
	}
	if !strings.HasSuffix(path, "/models/claude-opus-5") {
		t.Fatalf("checked %q, want the configured model", path)
	}
	if err := Check(context.Background(), NewAnthropic("good", "claude-opus-5")); err != nil {
		t.Fatalf("good key failed: %v", err)
	}
}

func TestCheckOpenAICompatModelMustExist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3:latest"},{"id":"models/gemini-2.5-pro"}]}`))
	}))
	defer srv.Close()
	cases := []struct {
		name, provider, model string
		ok                    bool
	}{
		{"pulled, tag implied", "ollama", "qwen3", true},
		{"not pulled", "ollama", "llama3.1", false},
		{"gemini prefix", "gemini", "gemini-2.5-pro", true},
		{"custom servers only need to answer", "custom", "anything", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Check(context.Background(), NewOpenAICompat(c.provider, srv.URL, "k", c.model))
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
			if err != nil {
				if msg, _ := Describe(err); !strings.Contains(msg, "ollama pull "+c.model) {
					t.Fatalf("describe = %q", msg)
				}
			}
		})
	}
}

func TestUnavailableProviderReportsItsReason(t *testing.T) {
	reason := &KeyMissingError{Provider: "openai", Env: "OPENAI_API_KEY"}
	p := Unavailable(reason)
	if _, err := p.Complete(context.Background(), Request{}); !errors.Is(err, reason) {
		t.Fatalf("complete err = %v", err)
	}
	if err := Check(context.Background(), p); !errors.Is(err, reason) {
		t.Fatalf("check err = %v", err)
	}
}

func TestOllamaChatModelsSkipsNonChatModels(t *testing.T) {
	caps := map[string][]string{
		"nomic-embed-text:latest":       {"embedding"},
		"karanchopda333/whisper:latest": {"completion"},
		"gemma2:2b":                     {"completion"},
		"qwen3:8b":                      {"completion", "tools"},
		"old-model:latest":              nil,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"nomic-embed-text:latest"},{"name":"karanchopda333/whisper:latest"},{"name":"gemma2:2b"},{"name":"qwen3:8b"},{"name":"old-model:latest"}]}`))
		case "/api/show":
			var req struct{ Model string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": caps[req.Model]})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	got, err := OllamaChatModels(context.Background(), srv.URL+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"qwen3:8b", "gemma2:2b", "old-model", "karanchopda333/whisper"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (tool-capable first, no embeddings, odd names last)", got, want)
	}
}

func TestMissingKey(t *testing.T) {
	t.Setenv("ANTHROPIC_CONFIG_DIR", t.TempDir()) // no `ant auth login` profile
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "")
	}
	cases := []struct {
		s       ProviderSettings
		env     string // set before checking
		missing bool
	}{
		{ProviderSettings{Provider: "anthropic"}, "", true}, // New would build a client that fails every turn
		{ProviderSettings{Provider: ""}, "", true},
		{ProviderSettings{Provider: "anthropic", APIKey: "k"}, "", false},
		{ProviderSettings{Provider: "anthropic"}, "ANTHROPIC_API_KEY", false},
		{ProviderSettings{Provider: "openai"}, "", true},
		{ProviderSettings{Provider: "gemini"}, "GEMINI_API_KEY", false},
		{ProviderSettings{Provider: "ollama"}, "", false},
		{ProviderSettings{Provider: "custom", BaseURL: "http://localhost:1234/v1"}, "", false},
	}
	for _, c := range cases {
		if c.env != "" {
			t.Setenv(c.env, "from-env")
		}
		err := MissingKey(c.s)
		var km *KeyMissingError
		if got := errors.As(err, &km); got != c.missing {
			t.Errorf("MissingKey(%+v) with %q set = %v, want missing=%v", c.s, c.env, err, c.missing)
		}
		if c.env != "" {
			t.Setenv(c.env, "")
		}
	}
	if !IsUnavailable(Unavailable(errors.New("x"))) || IsUnavailable(NewAnthropic("k", "m")) {
		t.Fatal("IsUnavailable")
	}
}

func TestCheckAcceptsAKeyThatCantListModels(t *testing.T) {
	// A restricted OpenAI project key: no Models:Read, but it may chat.
	cases := []struct {
		name   string
		status int
		body   string
		ok     bool
	}{
		{"chat works", 200, `{"choices":[{"message":{"role":"assistant","content":"p"},"finish_reason":"length"}]}`, true},
		{"the model wants other parameters", 400, `{"error":{"message":"Unsupported parameter: 'max_tokens'"}}`, true},
		{"the key really is bad", 401, `{"error":{"message":"Incorrect API key provided"}}`, false},
		{"no such model", 404, `{"error":{"message":"The model 'gpt-nope' does not exist"}}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/models") {
					w.WriteHeader(403)
					_, _ = w.Write([]byte(`{"error":{"message":"You have insufficient permissions for this operation. Missing scopes: api.model.read."}}`))
					return
				}
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			err := Check(context.Background(), NewOpenAICompat("openai", srv.URL, "sk-proj-restricted", "gpt-4.1"))
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
		})
	}
}
