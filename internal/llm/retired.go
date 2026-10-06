package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// Retired reports whether err says the model itself is gone: a hosted
// provider answered that the model doesn't exist (404), or has been
// deprecated or decommissioned. Providers retire model ids on a schedule, and
// a twin set to one used to fail on every turn. Ollama's and custom servers'
// "not found" means not downloaded or not served there, which another
// model id wouldn't mend, so those don't count.
func Retired(err error) bool {
	if err == nil {
		return false
	}
	var ue *UserError
	if errors.As(err, &ue) {
		err = ue.Err
	}
	provider, status, body := httpFailure(err)
	switch provider {
	case "anthropic", "openai", "gemini":
	default:
		return false
	}
	b := strings.ToLower(body)
	if !strings.Contains(b, "model") {
		return false
	}
	switch status {
	case 404:
		return true
	case 400, 410:
		for _, w := range []string{"deprecated", "decommissioned", "retired", "no longer available", "not found", "does not exist"} {
			if strings.Contains(b, w) {
				return true
			}
		}
	}
	return false
}

// Fallback thinks with the configured model until its provider says that
// model is gone, then with the provider's current default, so a retired
// model id doesn't silence the twin. Nothing is saved: the daemon hears of
// the switch once (FallbackNews), tells the owner, and saves it only with
// their yes.
type Fallback struct {
	primary  Provider
	settings ProviderSettings
	to       string
	build    func(ProviderSettings) (Provider, error)

	mu        sync.Mutex
	active    Provider // the default model, once the configured one is gone
	announced bool
}

// NewWithFallback is New, wrapped in a Fallback where one applies.
func NewWithFallback(s ProviderSettings) (Provider, error) {
	p, err := New(s)
	if err != nil {
		return nil, err
	}
	return WithFallback(p, s), nil
}

// WithFallback wraps p, built from s, so a retired model falls back to the
// provider's default. It returns p itself where there is nothing to fall
// back to: a local or custom server, or a model that already is the default.
func WithFallback(p Provider, s ProviderSettings) Provider {
	provider := strings.ToLower(strings.TrimSpace(s.Provider))
	if provider == "" {
		provider = "anthropic"
	}
	switch provider {
	case "anthropic", "openai", "gemini":
	default:
		return p
	}
	to := DefaultModel(provider)
	if p == nil || IsUnavailable(p) || s.Model == "" || s.Model == to {
		return p
	}
	s.Provider = provider
	return &Fallback{primary: p, settings: s, to: to, build: New}
}

// Name is the model in use.
func (f *Fallback) Name() string { return f.current().Name() }

func (f *Fallback) current() Provider {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active != nil {
		return f.active
	}
	return f.primary
}

// Complete runs the request on the configured model, or on the default once
// the configured one has turned out to be retired.
func (f *Fallback) Complete(ctx context.Context, req Request) (*Response, error) {
	return f.do(func(p Provider) (*Response, error) { return p.Complete(ctx, req) })
}

// Stream is Complete with text delivered as it arrives. A retired model
// fails before any text, so the retry never repeats a word.
func (f *Fallback) Stream(ctx context.Context, req Request, onDelta func(string)) (*Response, error) {
	return f.do(func(p Provider) (*Response, error) { return CompleteStreaming(ctx, p, req, onDelta) })
}

// ListModels lists what the provider offers.
func (f *Fallback) ListModels(ctx context.Context) ([]string, error) { return Models(ctx, f.primary) }

func (f *Fallback) do(call func(Provider) (*Response, error)) (*Response, error) {
	f.mu.Lock()
	act := f.active
	f.mu.Unlock()
	if act != nil {
		return call(act)
	}
	resp, err := call(f.primary)
	if err == nil || !Retired(err) {
		return resp, err
	}
	fb, ferr := f.fallback()
	if ferr != nil {
		return nil, err
	}
	resp, err2 := call(fb)
	if err2 != nil && Retired(err2) {
		return nil, err // the default isn't there either: say what's wrong with the one configured
	}
	f.engage(fb)
	return resp, err2
}

// fallback builds the provider's default model with the same key and endpoint.
func (f *Fallback) fallback() (Provider, error) {
	s := f.settings
	s.Model = f.to
	return f.build(s)
}

func (f *Fallback) engage(fb Provider) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active == nil {
		f.active = fb
	}
}

// check is Check for a Fallback: a retired model whose provider's default
// answers is working, on the default.
func (f *Fallback) check(ctx context.Context) error {
	f.mu.Lock()
	act := f.active
	f.mu.Unlock()
	if act != nil {
		return Check(ctx, act)
	}
	err := Check(ctx, f.primary)
	if err == nil || !Retired(err) {
		return err
	}
	fb, ferr := f.fallback()
	if ferr != nil || Check(ctx, fb) != nil {
		return err
	}
	f.engage(fb)
	return nil
}

// Using reports the model configured and the one in use, and whether they
// differ because the configured one was retired.
func (f *Fallback) Using() (configured, using string, fellBack bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active != nil {
		return f.settings.Model, f.to, true
	}
	return f.settings.Model, f.settings.Model, false
}

// FallbackNews reports, once, that p fell back from a retired model: the
// provider, the retired model and the one now in use. It is false for any
// other provider, before a fallback, and after the first report.
func FallbackNews(p Provider) (provider, from, to string, ok bool) {
	f, isF := p.(*Fallback)
	if !isF {
		return "", "", "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active == nil || f.announced {
		return "", "", "", false
	}
	f.announced = true
	return f.settings.Provider, f.settings.Model, f.to, true
}

// ProviderLabel is a provider's name as people say it: "Anthropic".
func ProviderLabel(provider string) string { return label(provider) }
