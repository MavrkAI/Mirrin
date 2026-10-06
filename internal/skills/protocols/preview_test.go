package protocols

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	proto "github.com/MavrkAI/Mirrin/internal/protocols"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// packRepo is a git repository holding a one-protocol pack.
func packRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	files := map[string]string{
		"pack.yaml":           "name: news\ndescription: The day's news\n",
		"protocols/news.yaml": "name: news digest\ndescription: Headlines at 7\nschedule: \"0 7 * * *\"\nrequires: [web]\nprompt: Read the front pages and send the three stories that matter.\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "one"}} {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// install_pack with preview=true shows what a pack would add (schedule,
// needs, every word of the prompt) and installs nothing; it needs no yes,
// while installing does.
func TestInstallPackPreviewInstallsNothing(t *testing.T) {
	repo := packRepo(t)
	installed := 0
	reg := &Registry{
		Search: func(context.Context, string) ([]proto.Pack, error) {
			return []proto.Pack{{Name: "news", Repo: "file://" + repo}}, nil
		},
		Install: func(context.Context, string) (string, error) { installed++; return "news", nil },
	}
	var install tools.Tool
	for _, tl := range Tools(func() []proto.Protocol { return nil }, nil, nil, reg) {
		if tl.Spec().Name == "install_pack" {
			install = tl
		}
	}
	if install == nil {
		t.Fatal("no install_pack")
	}
	in, _ := json.Marshal(map[string]any{"name": "news", "preview": true})
	call := tools.Call{Input: in}
	if r := install.(tools.CallRisker).RiskFor(context.Background(), call); r != tools.RiskRead {
		t.Fatalf("a preview is %v", r)
	}
	out, err := install.Run(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"news digest", "0 7 * * *", "Needs: web", "Read the front pages and send the three stories that matter.", "Nothing is installed"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	if installed != 0 {
		t.Fatal("the preview installed the pack")
	}

	in, _ = json.Marshal(map[string]any{"name": "news"})
	if r := install.(tools.CallRisker).RiskFor(context.Background(), tools.Call{Input: in}); r != tools.RiskWrite {
		t.Fatalf("installing is %v", r)
	}
	in, _ = json.Marshal(map[string]any{"name": repo, "preview": true})
	if r := install.(tools.CallRisker).RiskFor(context.Background(), tools.Call{Input: in}); r != tools.RiskWrite {
		t.Fatalf("previewing a folder on this computer is %v", r)
	}
	for _, src := range []string{"git@evil.example:x/y.git", "ssh://git@evil.example/x/y"} {
		in, _ := json.Marshal(map[string]any{"name": src, "preview": true})
		if r := install.(tools.CallRisker).RiskFor(context.Background(), tools.Call{Input: in}); r != tools.RiskWrite {
			t.Fatalf("previewing %s (the owner's SSH keys) is %v", src, r)
		}
	}
	in2, _ := json.Marshal(map[string]any{"name": "https://example.com/x/y", "preview": true})
	if r := install.(tools.CallRisker).RiskFor(context.Background(), tools.Call{Input: in2}); r != tools.RiskRead {
		t.Fatalf("previewing an https address is %v", r)
	}
	if _, err := install.Run(context.Background(), tools.Call{Input: in}); err == nil {
		t.Fatal("a local folder was previewed")
	}
}
