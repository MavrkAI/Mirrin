package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
)

// The Accounts model key card: Test checks a key without saving it, Save
// makes it the model, and the key is only ever shown masked.
func TestModelKeyCard(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("hi") })
	t.Setenv("OPENAI_API_KEY", "")
	old := checkModel
	var checked []llm.ProviderSettings
	checkModel = func(_ context.Context, s llm.ProviderSettings) error {
		checked = append(checked, s)
		if s.APIKey != "sk-good-0123456789abcdef" {
			return errors.New("openai 401: Incorrect API key provided")
		}
		return nil
	}
	t.Cleanup(func() { checkModel = old })
	ctx := context.Background()

	var he *api.HumanError
	if err := td.CheckBrain(ctx, "openai", "sk-bad-0123456789abcdef", ""); !errors.As(err, &he) || strings.Contains(err.Error(), "sk-bad") {
		t.Fatalf("a bad key: %v", err)
	}
	if err := td.CheckBrain(ctx, "openai", "sk-good-0123456789abcdef", ""); err != nil {
		t.Fatalf("a good key: %v", err)
	}
	if td.Config().LLM.Provider == "openai" {
		t.Fatal("Test changed the model")
	}
	if err := td.CheckBrain(ctx, "nope", "k", ""); !errors.As(err, &he) {
		t.Fatalf("an unknown provider: %v", err)
	}

	if err := td.SaveModelKey(ctx, "openai", "sk-bad-0123456789abcdef", ""); err == nil {
		t.Fatal("a bad key was saved")
	}
	if err := td.SaveModelKey(ctx, "openai", "sk-good-0123456789abcdef", "gpt-test"); err != nil {
		t.Fatal(err)
	}
	c := td.Config()
	if c.LLM.Provider != "openai" || c.LLM.Model != "gpt-test" || c.ProviderKey("openai") != "sk-good-0123456789abcdef" || c.LLM.APIKey != "" {
		t.Fatalf("config: provider %s model %s", c.LLM.Provider, c.LLM.Model)
	}
	if s, _ := config.ReadSecrets(); s["OPENAI_API_KEY"] != "sk-good-0123456789abcdef" {
		t.Fatal("the key isn't in secrets.env")
	}
	st := td.ModelKey(ctx)
	if st.Provider != "openai" || st.Key != "sk-g••••cdef" || len(st.Providers) == 0 {
		t.Fatalf("card: %+v", st)
	}
}
