package pagetest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// language makes the page see navigator.language (and languages) as lang.
func language(lang string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(fmt.Sprintf(`Object.defineProperty(navigator, 'language', {get: () => %q}); Object.defineProperty(navigator, 'languages', {get: () => [%q]})`, lang, lang)).Do(ctx)
		return err
	})
}

// The weather is in Fahrenheit in the US and Celsius elsewhere, and in
// Celsius when the language names no region.
func TestWeatherUnitFollowsTheLocale(t *testing.T) {
	for lang, want := range map[string]string{"en-US": "72°F Sunny", "en-GB": "22°C Sunny", "de-DE": "22°C Sunny", "en": "22°C Sunny"} {
		t.Run(lang, func(t *testing.T) {
			d := newDaemon(t)
			d.handle("GET /screen", jsonH(screen(func(m map[string]any) {
				m["weather"] = map[string]any{"temp_c": 22.2, "code": 0, "summary": "Sunny"}
			})))
			ctx := tab(t)
			run(t, ctx, language(lang), desktop(), chromedp.Navigate(d.url("/ui")))
			waitFor(t, ctx, loaded, "the screen loaded")
			if got := strings.TrimSpace(textOf(t, ctx, "#weather")); got != want {
				t.Fatalf("weather reads %q, want %q", got, want)
			}
		})
	}
}
