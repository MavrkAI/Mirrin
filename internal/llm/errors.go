package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/anthropics/anthropic-sdk-go"
)

// KeyMissingError means a hosted provider was chosen without an API key.
type KeyMissingError struct {
	Provider string
	Env      string // the environment variable that would supply it
}

func (e *KeyMissingError) Error() string {
	msg := fmt.Sprintf("%s needs an API key. Get one at %s, then run `mirrin init`", label(e.Provider), KeyURL(e.Provider))
	if e.Env != "" {
		msg += " (or set " + e.Env + ")"
	}
	return msg + "."
}

// UserError carries a plain explanation of a model failure for the person,
// while the original error stays reachable for logs and errors.Is.
type UserError struct {
	Msg string
	Err error
}

func (e *UserError) Error() string { return e.Msg }
func (e *UserError) Unwrap() error { return e.Err }

// LogValue keeps the raw provider error in logs, where it helps debugging.
func (e *UserError) LogValue() slog.Value { return slog.StringValue(e.Err.Error()) }

// Explain wraps a recognised model error so its message is the friendly one.
// Anything else comes back unchanged, and nil stays nil.
func Explain(err error) error {
	if err == nil {
		return nil
	}
	var ue *UserError
	if errors.As(err, &ue) {
		return err
	}
	if msg, ok := Describe(err); ok {
		return &UserError{Msg: msg, Err: err}
	}
	return err
}

// Friendly is what to tell the person when a turn fails: the plain
// explanation when the error is recognised, else a short generic line.
func Friendly(err error) string {
	if msg, ok := Describe(err); ok {
		return msg
	}
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return "Something went wrong on my end: " + truncate(s, 300)
}

// KeyURL is where to create an API key for a provider.
func KeyURL(provider string) string {
	switch provider {
	case "openai":
		return "https://platform.openai.com/api-keys"
	case "gemini":
		return "https://aistudio.google.com/apikey"
	case "ollama":
		return "https://ollama.com"
	}
	return "https://console.anthropic.com/settings/keys"
}

func billingURL(provider string) string {
	switch provider {
	case "anthropic":
		return "https://console.anthropic.com/settings/billing"
	case "openai":
		return "https://platform.openai.com/settings/organization/billing"
	case "gemini":
		return "https://aistudio.google.com"
	}
	return ""
}

func label(provider string) string {
	switch provider {
	case "anthropic":
		return "Anthropic"
	case "openai":
		return "OpenAI"
	case "gemini":
		return "Google Gemini"
	case "ollama":
		return "Ollama"
	case "custom", "openai-compatible":
		return "Your model server"
	}
	return "The model provider"
}

var (
	// reStatus finds the "<provider> <code>: <body>" our providers produce,
	// even when something else has wrapped it (e.g. "api 500: ...").
	reStatus = regexp.MustCompile(`\b(anthropic|openai|gemini|ollama|custom) (\d{3}): `)
	// reModel pulls the model name out of a not-found message.
	reModel = regexp.MustCompile("model(?::\\s*|\\s+['\"`])([A-Za-z0-9][\\w.:/-]*)")
)

// Describe explains a model error in one or two plain sentences that say what
// happened and what to do next, and reports whether it recognised the error.
func Describe(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var ue *UserError
	if errors.As(err, &ue) {
		return ue.Msg, true
	}
	var km *KeyMissingError
	if errors.As(err, &km) {
		return km.Error(), true
	}
	text := err.Error()
	lower := strings.ToLower(text)
	provider, status, body := httpFailure(err)
	b := strings.ToLower(body)

	switch {
	case strings.Contains(lower, "no anthropic credentials found"):
		return noKey("anthropic"), true
	case strings.Contains(b, "credit balance is too low"), strings.Contains(b, "insufficient_quota"),
		strings.Contains(b, "exceeded your current quota"), status == 402:
		msg := fmt.Sprintf("Your %s account is out of credit", label(provider))
		if u := billingURL(provider); u != "" {
			msg += ". Add some at " + u
		}
		return msg + ", then try again.", true
	case status == 401, status == 403, strings.Contains(b, "invalid x-api-key"), strings.Contains(b, "incorrect api key"),
		strings.Contains(b, "api key not valid"), strings.Contains(b, "invalid api key"), strings.Contains(b, "authentication_error"):
		if provider == "ollama" || provider == "custom" {
			return label(provider) + " refused the request. Check its API key in the config, then try again.", true
		}
		return fmt.Sprintf("%s didn't accept the API key. Check it at %s, then run `mirrin init` to update it.", label(provider), KeyURL(provider)), true
	case status == 404:
		model := ""
		if m := reModel.FindStringSubmatch(body); m != nil {
			model = m[1]
		}
		if provider == "ollama" {
			if model == "" {
				return "That model isn't downloaded yet. Run `ollama list` to see what you have, and `ollama pull <model>` to get one.", true
			}
			return fmt.Sprintf("The model %s isn't downloaded yet. Run `ollama pull %s`, or pick another model from the menu bar.", model, model), true
		}
		if model == "" {
			return label(provider) + " doesn't offer that model. Run `mirrin models` to see what it does, and pick one from the menu bar.", true
		}
		return fmt.Sprintf("%s doesn't offer the model %s. Run `mirrin models` to see what it does, and pick one from the menu bar.", label(provider), model), true
	case status == 429:
		return label(provider) + " is rate-limiting me right now. Give it a minute and try again.", true
	case status == 529, strings.Contains(b, "overloaded"):
		return label(provider) + " is overloaded right now. Try again in a minute.", true
	case status >= 500:
		return fmt.Sprintf("%s is having trouble right now (error %d). Try again in a minute.", label(provider), status), true
	case status >= 400:
		msg := apiMessage(body)
		if msg == "" && !strings.HasPrefix(strings.TrimSpace(body), "{") {
			msg = strings.TrimSpace(body) // OpenAI-compatible errors arrive already unwrapped
		}
		if msg != "" {
			return fmt.Sprintf("%s turned the request down: %s.", label(provider), strings.TrimSuffix(truncate(msg, 200), ".")), true
		}
	}

	// Network failures, only when they are on the way to a provider we know:
	// the same words from the local API mean something else entirely.
	if status == 0 && provider != "" {
		refused := errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(lower, "connection refused") || strings.Contains(lower, "actively refused")
		switch {
		case refused && provider == "ollama":
			return "Ollama isn't running. Open the Ollama app (or run `ollama serve`) and try again.", true
		case refused, strings.Contains(lower, "no such host"), strings.Contains(lower, "network is unreachable"),
			strings.Contains(lower, "i/o timeout"), strings.Contains(lower, "tls handshake timeout"):
			return fmt.Sprintf("I can't reach %s right now. Check your internet connection and try again.", label(provider)), true
		}
	}
	return "", false
}

// httpFailure finds the provider, HTTP status and body in a model error:
// the SDK's typed error, or the "<provider> <code>: <body>" our providers
// produce, even when something else has wrapped it. Without a status it still
// names the provider when the error mentions its host.
func httpFailure(err error) (provider string, status int, body string) {
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		return "anthropic", apierr.StatusCode, apierr.RawJSON()
	}
	text := err.Error()
	if m := reStatus.FindStringSubmatchIndex(text); m != nil {
		status, _ = strconv.Atoi(text[m[4]:m[5]])
		return text[m[2]:m[3]], status, text[m[1]:]
	}
	return providerFromHost(strings.ToLower(text)), 0, ""
}

// CredentialError reports whether err means the model has no usable key: none
// was given, or the provider turned it down. Those get better only when the
// person adds or changes a key, so it is worth reloading the config for.
func CredentialError(err error) bool {
	if err == nil {
		return false
	}
	var ue *UserError
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var km *KeyMissingError
	if errors.As(err, &km) {
		return true
	}
	if strings.Contains(strings.ToLower(err.Error()), "no anthropic credentials found") {
		return true
	}
	provider, status, body := httpFailure(err)
	if provider == "ollama" || provider == "custom" {
		return false
	}
	b := strings.ToLower(body)
	return status == 401 || status == 403 || strings.Contains(b, "invalid x-api-key") ||
		strings.Contains(b, "incorrect api key") || strings.Contains(b, "api key not valid")
}

// apiMessage pulls the human part out of a provider's JSON error body.
func apiMessage(body string) string {
	var e struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &e) != nil {
		return ""
	}
	if e.Error.Message != "" {
		return e.Error.Message
	}
	return e.Message
}

// noKey is the guidance for a hosted provider with no key at all.
func noKey(provider string) string {
	return fmt.Sprintf("I don't have an API key for %s yet. Get one at %s and run `mirrin init` to add it, or install Ollama (https://ollama.com) to think locally for free.", label(provider), KeyURL(provider))
}

func providerFromHost(s string) string {
	switch {
	case strings.Contains(s, "api.anthropic.com"):
		return "anthropic"
	case strings.Contains(s, "api.openai.com"):
		return "openai"
	case strings.Contains(s, "generativelanguage.googleapis.com"):
		return "gemini"
	case strings.Contains(s, ":11434"):
		return "ollama"
	}
	return ""
}
