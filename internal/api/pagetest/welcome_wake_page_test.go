package pagetest

import (
	"maps"
	"testing"

	"github.com/chromedp/chromedp"
)

// A build without a wake-word model has no persona that hears its name
// first, so no card says it answers a beat slower than Mirrin.
func TestWelcomeSaysNothingSlowerWithoutAWakeModel(t *testing.T) {
	ctx, d, _ := welcomeStep2(t, true)
	slow := `document.querySelectorAll('#cast .slow').length`
	if n := eval[int](t, ctx, slow); n != 2 {
		t.Fatalf("%d slower lines with Mirrin's model, want 2", n)
	}
	cast := make([]map[string]any, len(welcomeCast))
	for i, p := range welcomeCast {
		cast[i] = maps.Clone(p)
		cast[i]["instant_wake"] = false
	}
	d.handle("GET /welcome/personas", jsonH(cast))
	run(t, ctx, chromedp.Navigate(d.url("/welcome")))
	waitFor(t, ctx, `document.querySelectorAll('#cast input[name=persona]').length === 3`, "the cast without a wake model")
	if n := eval[int](t, ctx, slow); n != 0 {
		t.Fatalf("%d slower lines with no wake model at all", n)
	}
}
