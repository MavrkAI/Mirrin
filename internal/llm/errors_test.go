package llm

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestDescribeProviderErrors(t *testing.T) {
	refused := &url.Error{Op: "Post", URL: "http://127.0.0.1:11434/v1/chat/completions",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}
	cases := []struct {
		name string
		err  error
		want []string // all must appear
	}{
		{"anthropic no credentials", errors.New("POST \"https://api.anthropic.com/v1/messages\": no Anthropic credentials found. The SDK tried these sources in order:\n  1. ..."),
			[]string{"don't have an API key for Anthropic", "console.anthropic.com", "mirrin init", "Ollama"}},
		{"anthropic bad key", errors.New(`anthropic 401: {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`),
			[]string{"Anthropic didn't accept the API key", "console.anthropic.com/settings/keys"}},
		{"openai bad key", errors.New("openai 401: Incorrect API key provided: sk-abc"),
			[]string{"OpenAI didn't accept the API key", "platform.openai.com/api-keys"}},
		{"gemini bad key is a 400", errors.New("gemini 400: API key not valid. Please pass a valid API key."),
			[]string{"Google Gemini didn't accept the API key"}},
		{"anthropic no credit", errors.New(`anthropic 400: {"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`),
			[]string{"out of credit", "console.anthropic.com/settings/billing"}},
		{"openai quota", errors.New("openai 429: You exceeded your current quota, please check your plan and billing details."),
			[]string{"OpenAI account is out of credit"}},
		{"rate limit", errors.New("anthropic 429: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\"}}"),
			[]string{"rate-limiting", "minute"}},
		{"overloaded", errors.New(`anthropic 529: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
			[]string{"Anthropic is overloaded"}},
		{"ollama model not pulled", errors.New(`ollama 404: model "llama3.1" not found, try pulling it first`),
			[]string{"llama3.1 isn't downloaded", "ollama pull llama3.1"}},
		{"anthropic unknown model", errors.New(`anthropic 404: {"type":"error","error":{"type":"not_found_error","message":"model: claude-nope"}}`),
			[]string{"Anthropic doesn't offer the model claude-nope", "mirrin models"}},
		{"ollama not running", refused, []string{"Ollama isn't running", "ollama serve"}},
		{"offline", errors.New(`Post "https://api.openai.com/v1/chat/completions": dial tcp: lookup api.openai.com: no such host`),
			[]string{"can't reach OpenAI", "internet"}},
		{"wrapped by the local API", errors.New(`api 500: anthropic 401: {"type":"error"}`),
			[]string{"Anthropic didn't accept the API key"}},
		{"missing key from New", &KeyMissingError{Provider: "gemini", Env: "GEMINI_API_KEY"},
			[]string{"Google Gemini needs an API key", "aistudio.google.com", "GEMINI_API_KEY"}},
		{"other 4xx keeps only the message", errors.New(`anthropic 400: {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`),
			[]string{"Anthropic turned the request down: prompt is too long."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Describe(c.err)
			if !ok {
				t.Fatalf("not recognised: %v", c.err)
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q\n  missing %q", got, w)
				}
			}
			if strings.ContainsAny(got, "{}\n") {
				t.Errorf("raw detail leaked: %q", got)
			}
		})
	}
}

func TestFriendlyAndExplain(t *testing.T) {
	for _, e := range []string{"disk full", `Post "http://127.0.0.1:7742/message/stream": dial tcp 127.0.0.1:7742: connect: connection refused`} {
		if _, ok := Describe(errors.New(e)); ok {
			t.Fatalf("unrelated error dressed up as a model error: %s", e)
		}
	}
	if got := Friendly(errors.New("disk full\nstack...")); got != "Something went wrong on my end: disk full" {
		t.Fatalf("fallback = %q", got)
	}
	raw := errors.New("openai 401: Incorrect API key provided")
	wrapped := Explain(fmt.Errorf("turn: %w", raw))
	if !errors.Is(wrapped, raw) {
		t.Fatal("Explain must keep the original reachable")
	}
	if !strings.HasPrefix(wrapped.Error(), "OpenAI didn't accept") || Friendly(wrapped) != wrapped.Error() {
		t.Fatalf("explained = %q", wrapped)
	}
	if Explain(nil) != nil {
		t.Fatal("Explain(nil) must be nil")
	}
	plain := errors.New("no such approval")
	if Explain(plain) != plain {
		t.Fatal("unrecognised errors pass through unchanged")
	}
}

func TestCredentialError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&KeyMissingError{Provider: "openai"}, true},
		{errors.New("POST \"https://api.anthropic.com/v1/messages\": no Anthropic credentials found. The SDK tried..."), true},
		{errors.New(`anthropic 401: {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`), true},
		{errors.New("openai 401: Incorrect API key provided: sk-abc"), true},
		{errors.New("gemini 400: API key not valid. Please pass a valid API key."), true},
		{Explain(errors.New("openai 401: Incorrect API key provided")), true}, // still seen through the friendly wrapper
		{errors.New("anthropic 429: rate limited"), false},
		{errors.New("ollama 404: model \"llama3.1\" not found"), false},
		{errors.New("custom 401: unauthorized"), false}, // a local server's key isn't one to go and get
		{errors.New("dial tcp: connection refused"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := CredentialError(c.err); got != c.want {
			t.Errorf("CredentialError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
