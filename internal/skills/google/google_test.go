package google

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
)

func TestClientAndFlow(t *testing.T) {
	dir := t.TempDir()
	a := NewAuth(config.Calendar{CredentialsFile: filepath.Join(dir, "c.json"), TokenFile: filepath.Join(dir, "t.json")})
	if a.HasCredentials() || a.Connected() {
		t.Fatal("fresh auth should have nothing")
	}
	if err := a.WriteClient("id.apps.googleusercontent.com", "sekret"); err != nil {
		t.Fatal(err)
	}
	u, err := a.BeginURL("http://127.0.0.1:7742/oauth/google")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"client_id=id.apps.googleusercontent.com", "gmail.modify", "drive.readonly", "calendar", "redirect_uri=http%3A%2F%2F127.0.0.1%3A7742%2Foauth%2Fgoogle", "access_type=offline"} {
		if !strings.Contains(u, want) {
			t.Fatalf("auth url missing %q: %s", want, u)
		}
	}
	if err := a.Finish(context.Background(), "wrong-state", "code"); err == nil || !strings.Contains(err.Error(), "start again") {
		t.Fatalf("state mismatch should fail: %v", err)
	}
	if err := a.ImportCredentials([]byte(`{"web":{"client_id":"x.apps.googleusercontent.com","client_secret":"y"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := a.ImportCredentials([]byte(`{"nope":1}`)); err == nil {
		t.Fatal("bad json accepted")
	}
}
