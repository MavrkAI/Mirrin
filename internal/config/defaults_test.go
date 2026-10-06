package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The regression: the default spending caps were 100 and 500 whatever the
// currency, so a twin in Japan would ask before every ¥100 purchase.
func TestSpendingCapsScaleWithTheCurrency(t *testing.T) {
	jp := defaultSpending(localeFor("JP"))
	if jp.Currency != "JPY" || jp.PerActionLimit < 10000 || jp.MonthlyLimit < 5*jp.PerActionLimit {
		t.Fatalf("JPY caps %+v", jp)
	}
	au := defaultSpending(localeFor("AU"))
	if au.Currency != "AUD" || au.PerActionLimit != 100 || au.MonthlyLimit != 500 {
		t.Fatalf("AUD caps %+v", au)
	}
	if fb := defaultSpending(fallbackLocale); fb.PerActionLimit != 100 || fb.MonthlyLimit != 500 {
		t.Fatalf("fallback caps %+v", fb)
	}
	for cur, m := range capScale {
		if m < 1 {
			t.Errorf("%s: multiplier %v", cur, m)
		}
	}
}

// The regression: the caps were scaled by the region, not the currency, so
// a config in Japan that set currency: USD got US$10 000 a payment. And a
// file that never named its caps must not have them raised by an update.
func TestUnnamedCapsAreNeverScaledUp(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	regionOnce.Do(func() {}) // as if the system said Japan
	prev := region
	region = "JP"
	t.Cleanup(func() { region = prev })
	if d := Default().Spending; d.Currency != "JPY" || d.PerActionLimit != 10000 {
		t.Fatalf("a new install in Japan starts with %+v", d)
	}
	for _, body := range []string{
		"spending:\n  currency: USD\n",
		"spending:\n  currency: JPY\n",
		"llm:\n  model: claude-opus-5\n",
	} {
		if err := os.WriteFile(Path(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.Spending.PerActionLimit != 100 || c.Spending.MonthlyLimit != 500 {
			t.Errorf("%q loaded caps %+v", body, c.Spending)
		}
	}
	if err := os.WriteFile(Path(), []byte("spending:\n  currency: JPY\n  per_action_limit: 20000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil || c.Spending.PerActionLimit != 20000 || c.Spending.MonthlyLimit != 500 {
		t.Fatalf("named cap kept, unnamed one unscaled: %+v %v", c.Spending, err)
	}
	if usd := (Spending{Currency: "USD", PerActionLimit: 100}).scaledFor("USD"); usd.PerActionLimit != 100 {
		t.Fatalf("USD scaled to %v", usd.PerActionLimit)
	}
}

// The regression: max_tokens: "" (a blank left by a form or a hand edit)
// failed the whole config.
func TestLoadToleratesABlankMaxTokens(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	if err := os.WriteFile(Path(), []byte("llm:\n  model: claude-opus-5\n  max_tokens: \"\"\n  effort: low\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("a blank max_tokens must load: %v", err)
	}
	if c.LLM.MaxTokens != Default().LLM.MaxTokens || c.LLM.Effort != "low" || c.LLM.Model != "claude-opus-5" {
		t.Fatalf("llm %+v", c.LLM)
	}
	if err := os.WriteFile(Path(), []byte("llm:\n  max_tokens: 900\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(); err != nil || c.LLM.MaxTokens != 900 {
		t.Fatalf("a set max_tokens is kept: %v %v", c, err)
	}
}

// The regression: approval_ttl: "" failed the whole config, as a blank
// max_tokens once did.
func TestLoadToleratesABlankApprovalTTL(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	if err := os.WriteFile(Path(), []byte("autonomy:\n  approval_ttl: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("a blank approval_ttl must load: %v", err)
	}
	if c.Autonomy.TTL() != DefaultApprovalTTL {
		t.Fatalf("blank approval_ttl gave %s", c.Autonomy.TTL())
	}
	if got := Duration(time.Hour).String(); got != "1h" {
		t.Fatalf("1h written as %q", got)
	}
}

func TestApprovalTTLFromConfig(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	if got := Default().Autonomy.TTL(); got != 72*time.Hour {
		t.Fatalf("default %s", got)
	}
	if err := os.WriteFile(Path(), []byte("autonomy:\n  approval_ttl: 1h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil || c.Autonomy.TTL() != time.Hour {
		t.Fatalf("approval_ttl: 1h gave %v (%v)", c.Autonomy.ApprovalTTL, err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(); err != nil || c.Autonomy.TTL() != time.Hour {
		t.Fatalf("saved and loaded again: %v (%v)", c.Autonomy.ApprovalTTL, err)
	}
	if b, _ := os.ReadFile(Path()); !strings.Contains(string(b), "approval_ttl: 1h\n") {
		t.Fatalf("saved as:\n%s", b)
	}
	c.Autonomy.ApprovalTTL = Duration(-time.Hour)
	if c.Validate() == nil {
		t.Fatal("a negative approval_ttl is refused")
	}
}

// api.remote's note describes how devices sign in today: each with its own
// key, not one shared token, and names the twin rather than a persona.
func TestRemoteNoteWording(t *testing.T) {
	b, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	api := regexp.MustCompile(`(?s)// API configures the control socket\..*?\n}\n`).FindString(src)
	if api == "" {
		t.Fatal("API struct not found")
	}
	for _, bad := range []string{"Mirrin", "with the token"} {
		if strings.Contains(api, bad) {
			t.Errorf("api.remote's note says %q:\n%s", bad, api)
		}
	}
	if strings.Contains(src, "with the token") {
		t.Error("config.go still says devices connect with the token")
	}
	if !strings.Contains(api, "Tailscale") || !strings.Contains(api, "own key") {
		t.Errorf("api.remote's note should recommend a Tailscale address and say each device has its own key:\n%s", api)
	}
}

// The regression: "sir" was the default form of address whoever the
// persona, so Nyra and Pickoo called owners who never chose it
// "sir". Empty leaves it to the persona.
func TestTheDefaultFormOfAddressIsThePersonas(t *testing.T) {
	t.Setenv("MIRRIN_HOME", t.TempDir())
	if h := Default().User.Honorific; h != "" {
		t.Fatalf("default honorific %q, want the persona's own", h)
	}
}
