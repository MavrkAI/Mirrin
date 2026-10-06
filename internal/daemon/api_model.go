package daemon

import (
	"context"
	"errors"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// The model key card on Accounts (api.ModelKeyBackend) and the welcome
// check it tests keys with (api.BrainChecker).

// modelProviders are the providers the card offers, in its order.
var modelProviders = []string{"anthropic", "openai", "gemini", "ollama", "openai-compatible"}

// checkModel proves a provider answers with these settings (llm.Check: it
// generates nothing, so it costs nothing). Tests replace it.
var checkModel = func(ctx context.Context, s llm.ProviderSettings) error {
	p, err := llm.New(s)
	if err != nil {
		return err
	}
	return llm.Check(ctx, p)
}

// modelSettings is what a key would run with: an empty key or model keeps
// the one configured.
func (d *Daemon) modelSettings(provider, key, model string) (s llm.ProviderSettings, supplied bool, env string, err error) {
	known := false
	for _, p := range modelProviders {
		known = known || p == provider
	}
	if !known {
		return s, false, "", &api.HumanError{Sentence: "I don't know that model provider.", Fix: "Pick one from the list."}
	}
	key = strings.TrimSpace(key)
	if strings.ContainsAny(key, "\r\n\t ") {
		return s, false, "", &api.HumanError{Sentence: "That key has spaces or line breaks in it.", Fix: "Copy the key again, on its own, and paste it."}
	}
	c := d.Config()
	if model = strings.TrimSpace(model); model == "" {
		model = c.ProviderModel(provider)
	}
	if model == "" {
		model = llm.DefaultModel(provider)
	}
	supplied = key != ""
	if env = llm.DefaultKeyEnv(provider); env == "" {
		env = "MIRRIN_CUSTOM_API_KEY"
	}
	// An exported key wins over a saved one, under any of its names.
	if name, exported := config.Exported(env); supplied && exported != "" && exported != key {
		return s, false, "", &api.HumanError{Sentence: "A different key is set in your environment.", Fix: "Remove " + name + " from the environment, restart Mirrin, then paste your new key."}
	}
	if !supplied {
		key = c.ProviderKey(provider)
	}
	s = llm.ProviderSettings{Provider: provider, Model: model, APIKey: key, BaseURL: c.ProviderBaseURL(provider)}
	if err := llm.MissingKey(s); err != nil {
		return s, false, "", &api.HumanError{Sentence: "That provider needs a key.", Fix: "Paste your API key, then press Test."}
	}
	return s, supplied, env, nil
}

// CheckBrain tests a key without saving it (POST /welcome/check, POST /accounts/model/check).
func (d *Daemon) CheckBrain(ctx context.Context, provider, key, model string) error {
	s, _, _, err := d.modelSettings(provider, key, model)
	if err != nil {
		return err
	}
	return brainError(checkModel(ctx, s))
}

// ModelKey is the card's state. The key is only ever shown masked.
func (d *Daemon) ModelKey(ctx context.Context) api.ModelKeyState {
	c := d.Config()
	return api.ModelKeyState{Provider: c.LLM.Provider, Model: c.LLM.Model, Key: api.MaskKey(c.ProviderKey(c.LLM.Provider)), Providers: modelProviders}
}

// SaveModelKey checks a key and makes it the twin's model: the key goes in
// secrets.env, the choice in the config, and the next reply uses it.
func (d *Daemon) SaveModelKey(ctx context.Context, provider, key, model string) error {
	s, supplied, env, err := d.modelSettings(provider, key, model)
	if err != nil {
		return err
	}
	if err := checkModel(ctx, s); err != nil {
		return brainError(err)
	}
	p, err := llm.New(s)
	if err != nil {
		return brainError(err)
	}
	if supplied {
		if err := config.SaveSecrets(map[string]string{env: s.APIKey}); err != nil {
			return errors.New("couldn't save the key: " + err.Error())
		}
	}
	err = d.UpdateConfig(func(c *config.Config) {
		if c.LLM.Providers == nil {
			c.LLM.Providers = map[string]config.ProviderConfig{}
		}
		if c.LLM.Provider != provider && c.LLM.Provider != "" {
			// keep what the old provider had, for switching back
			prev := c.LLM.Providers[c.LLM.Provider]
			prev.Model = c.LLM.Model
			prev.BaseURL = c.ProviderBaseURL(c.LLM.Provider)
			if prev.APIKey == "" {
				prev.APIKey = c.LLM.APIKey
			}
			if prev.APIKeyEnv == "" {
				prev.APIKeyEnv = c.LLM.APIKeyEnv
			}
			c.LLM.Providers[c.LLM.Provider] = prev
		}
		pc := c.LLM.Providers[provider]
		if !supplied && provider == c.LLM.Provider {
			if pc.APIKey == "" {
				pc.APIKey = c.LLM.APIKey
			}
			if pc.APIKeyEnv == "" {
				pc.APIKeyEnv = c.LLM.APIKeyEnv
			}
		}
		pc.Model = s.Model
		if supplied {
			pc.APIKey, pc.APIKeyEnv = "", env
		}
		c.LLM.Providers[provider] = pc
		c.LLM.BaseURL = s.BaseURL
		c.LLM.Provider = provider
		c.LLM.Model = s.Model
		c.LLM.APIKey = ""
		c.LLM.APIKeyEnv = pc.APIKeyEnv
	})
	if err != nil {
		return err
	}
	d.agent.SetProvider(p) // a same-provider key change is invisible to UpdateConfig
	d.modelDown.Store(false)
	return nil
}

var (
	_ api.ModelKeyBackend = (*Daemon)(nil)
	_ api.BrainChecker    = (*Daemon)(nil)
)
