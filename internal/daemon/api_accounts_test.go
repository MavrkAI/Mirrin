package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A connected account's problems show as notices, each linking where to
// fix it; "use a different client" sets the client aside and the page asks
// for a new one.
func TestAccountNoticesAndUsingADifferentClient(t *testing.T) {
	td, fake := googleDaemon(t)
	fake.Disable("drive")
	connectThroughThePage(t, td)
	ctx := context.Background()
	st := td.AccountStates(ctx)[0]
	if !st.Connected || len(st.Notices) != 1 {
		t.Fatalf("page: %+v", st)
	}
	if n := st.Notices[0]; !strings.Contains(n.Text, "Drive API is turned off") || !strings.HasPrefix(n.URL, "https://") || n.Link == "" {
		t.Fatalf("notice: %+v", n)
	}

	if err := td.UseDifferentGoogleClient(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(td.google.CredentialsFile + ".rejected"); err != nil {
		t.Fatalf("client not set aside: %v", err)
	}
	if _, err := os.Stat(td.google.CredentialsFile); !os.IsNotExist(err) {
		t.Fatalf("client still in place: %v", err)
	}
	st = td.AccountStates(ctx)[0]
	if st.Connected || st.HasClient || len(st.Steps) == 0 || !strings.Contains(st.Blurb, "set aside") {
		t.Fatalf("page after: %+v", st)
	}
	// A new client pasted in is used, and the set-aside note goes.
	if err := td.SaveGoogleClient(ctx, "456-def.apps.googleusercontent.com", "GOCSPX-new", ""); err != nil {
		t.Fatal(err)
	}
	if st := td.AccountStates(ctx)[0]; !st.HasClient || strings.Contains(st.Blurb, "set aside") {
		t.Fatalf("page with a new client: %+v", st)
	}
}
