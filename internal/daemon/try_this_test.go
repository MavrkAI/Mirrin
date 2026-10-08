package daemon

import (
	"context"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/skills/browser"
)

// The screen offers to show a web chore only when the twin has a browser:
// /screen says so under connected.
func TestTheScreenKnowsWhenThereIsABrowser(t *testing.T) {
	td := newTestDaemon(t, echo)
	ctx := context.Background()
	if td.connected().Browser || td.screenData(ctx).Connected.Browser {
		t.Fatal("no browser, but the screen is told there is one")
	}
	sess := browser.NewSession(config.Browser{Enabled: true, Headless: true}, t.TempDir(), nil)
	t.Cleanup(sess.Close)
	td.browser = sess
	if !td.connected().Browser || !td.screenData(ctx).Connected.Browser {
		t.Fatal("a browser, but the screen isn't told")
	}
}
