package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	sdkconfig "github.com/anthropics/anthropic-sdk-go/config"
)

// ProviderSettings is what a provider needs, resolved from config.
type ProviderSettings struct {
	Provider string // anthropic | openai | gemini | ollama | openai-compatible
	Model    string
	APIKey   string
	BaseURL  string
}

// DefaultKeyEnv is the environment variable each provider reads by default.
func DefaultKeyEnv(provider string) string {
	switch provider {
	case "openai":
		return "OPENAI_API_KEY"
	case "gemini":
		return "GEMINI_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	}
	return ""
}

// DefaultBaseURL for the hosted providers.
func DefaultBaseURL(provider string) string {
	switch provider {
	case "openai":
		return OpenAIBaseURL
	case "gemini":
		return GeminiBaseURL
	case "ollama":
		return OllamaBaseURL
	}
	return ""
}

// DefaultModel is a sensible starting model per provider.
func DefaultModel(provider string) string {
	switch provider {
	case "anthropic":
		return "claude-opus-5"
	case "openai":
		return "gpt-4.1"
	case "gemini":
		return "gemini-2.5-pro"
	case "ollama":
		return "llama3.1"
	}
	return ""
}

// New builds a provider from settings.
func New(s ProviderSettings) (Provider, error) {
	provider := strings.ToLower(strings.TrimSpace(s.Provider))
	if provider == "" {
		provider = "anthropic"
	}
	model := s.Model
	if model == "" {
		model = DefaultModel(provider)
	}
	key := s.APIKey
	if key == "" {
		if env := DefaultKeyEnv(provider); env != "" {
			key = os.Getenv(env)
		}
	}
	base := s.BaseURL
	if base == "" {
		base = DefaultBaseURL(provider)
	}
	switch provider {
	case "anthropic":
		return NewAnthropicAt(key, model, s.BaseURL), nil // only an explicit base_url
	case "openai", "gemini", "ollama":
		if key == "" && provider != "ollama" {
			return nil, &KeyMissingError{Provider: provider, Env: DefaultKeyEnv(provider)}
		}
		return NewOpenAICompat(provider, base, key, model), nil
	case "openai-compatible", "custom":
		if base == "" {
			return nil, fmt.Errorf("%s: base_url is required", provider)
		}
		return NewOpenAICompat("custom", base, key, model), nil
	}
	return nil, fmt.Errorf("unknown llm provider %q", provider)
}

// Models lists what a provider offers, when it can.
func Models(ctx context.Context, p Provider) ([]string, error) {
	if l, ok := p.(ModelLister); ok {
		return l.ListModels(ctx)
	}
	if _, ok := p.(*Anthropic); ok {
		return []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5", "claude-fable-5-1"}, nil
	}
	return nil, fmt.Errorf("%s cannot list models", p.Name())
}

// MissingKey reports a hosted provider with no credential at all. New alone
// doesn't: it builds an Anthropic client without a key, which only fails on
// its first request. An `ant auth login` profile counts as a credential.
func MissingKey(s ProviderSettings) error {
	provider := strings.ToLower(strings.TrimSpace(s.Provider))
	if provider == "" {
		provider = "anthropic"
	}
	env := DefaultKeyEnv(provider)
	if env == "" || s.APIKey != "" || os.Getenv(env) != "" {
		return nil
	}
	if provider == "anthropic" && AnthropicSignedIn() {
		return nil
	}
	return &KeyMissingError{Provider: provider, Env: env}
}

// AnthropicSignedIn reports whether the Anthropic SDK can authenticate without
// an API key: an auth token in the environment, or an `ant auth login` profile.
func AnthropicSignedIn() bool {
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		return true
	}
	_, err := sdkconfig.LoadConfig()
	return err == nil
}

// Check makes the cheapest real request that proves the provider will answer:
// the key is accepted and the model exists (for Ollama, that it is pulled).
// It generates nothing, so it costs nothing.
func Check(ctx context.Context, p Provider) error {
	switch v := p.(type) {
	case *Fallback:
		return v.check(ctx)
	case *Anthropic:
		_, err := v.client.Models.Get(ctx, v.model, anthropic.ModelGetParams{})
		return wrapErr(err)
	case *OpenAICompat:
		ids, err := v.ListModels(ctx)
		if err != nil {
			if _, status, _ := httpFailure(err); status == 403 && v.name != "ollama" {
				// A project key may chat without being allowed to list models.
				return checkByAsking(ctx, v)
			}
			return err
		}
		// Custom servers often serve more than they list; a listing is proof enough.
		if v.name == "custom" || hasModel(ids, v.model) {
			return nil
		}
		return fmt.Errorf("%s 404: model '%s' not found", v.name, v.model)
	case *unavailable:
		return v.err
	}
	_, err := p.Complete(ctx, Request{Messages: []Message{Text(RoleUser, "ping")}, MaxTokens: 1})
	return err
}

// checkByAsking proves a key with the smallest possible request. A request
// the provider turns down for any reason but the key (a 400 for a parameter
// the model doesn't take, say) still shows the key works.
func checkByAsking(ctx context.Context, p Provider) error {
	_, err := p.Complete(ctx, Request{Messages: []Message{Text(RoleUser, "ping")}, MaxTokens: 1})
	if err == nil || CredentialError(err) {
		return err
	}
	if _, status, _ := httpFailure(err); status >= 400 && status < 500 && status != 404 && status != 429 {
		return nil
	}
	return err
}

// hasModel reports whether a listing includes model, allowing for Gemini's
// "models/" prefix and Ollama's implicit ":latest" tag.
func hasModel(ids []string, model string) bool {
	norm := func(s string) string { return strings.TrimSuffix(strings.TrimPrefix(s, "models/"), ":latest") }
	want := norm(model)
	for _, id := range ids {
		if norm(id) == want {
			return true
		}
	}
	return false
}

// Unavailable is a provider that answers every request with err. The daemon
// uses it to start without a working model, so the menu bar and health page
// can say what's missing instead of the process exiting.
func Unavailable(err error) Provider { return &unavailable{err: err} }

type unavailable struct{ err error }

// IsUnavailable reports whether p is the stand-in from Unavailable.
func IsUnavailable(p Provider) bool {
	_, ok := p.(*unavailable)
	return ok
}

func (u *unavailable) Name() string { return "unavailable" }
func (u *unavailable) Complete(context.Context, Request) (*Response, error) {
	return nil, u.err
}

// OllamaChatModels lists the local models that can hold a conversation, the
// ones that can also call tools first, with Ollama's ":latest" tag dropped.
// Embedding and speech models are left out. baseURL is the OpenAI-compatible
// endpoint (".../v1"); empty means the default local server.
func OllamaChatModels(ctx context.Context, baseURL string) ([]string, error) {
	if baseURL == "" {
		baseURL = OllamaBaseURL
	}
	root := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	hc := &http.Client{Timeout: 5 * time.Second}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := ollamaJSON(ctx, hc, http.MethodGet, root+"/api/tags", nil, &tags); err != nil {
		return nil, err
	}
	var tools, chat, odd []string
	for _, m := range tags.Models {
		if ctx.Err() != nil {
			break // out of time: better a short list than guesses
		}
		name := strings.TrimSuffix(m.Name, ":latest")
		var show struct {
			Capabilities []string `json:"capabilities"`
		}
		_ = ollamaJSON(ctx, hc, http.MethodPost, root+"/api/show", map[string]string{"model": m.Name}, &show)
		caps := strings.Join(show.Capabilities, " ")
		switch {
		case len(show.Capabilities) == 0 && reNotChat.MatchString(name):
			// Older Ollama without capabilities: go by the name.
		case len(show.Capabilities) > 0 && !strings.Contains(caps, "completion"):
			// embeddings and the like
		case reNotChat.MatchString(name):
			odd = append(odd, name) // says it can chat, but the name suggests otherwise: offer it last
		case strings.Contains(caps, "tools"):
			tools = append(tools, name)
		default:
			chat = append(chat, name)
		}
	}
	return append(append(tools, chat...), odd...), nil
}

// reNotChat matches model names that are not conversational.
var reNotChat = regexp.MustCompile(`(?i)embed|whisper|rerank|bge-|minilm|clip|tts`)

func ollamaJSON(ctx context.Context, hc *http.Client, method, url string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ollama %d: %s", resp.StatusCode, url)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}
