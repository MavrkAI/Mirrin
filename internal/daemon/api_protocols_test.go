package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/protocols"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// The store's update check reports each pack's move, and applying one moves
// only that pack, after checking again.
func TestPackUpdatesCheckAndApply(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	oldCheck, oldApply := packCheck, packApply
	t.Cleanup(func() { packCheck, packApply = oldCheck, oldApply })
	from, to := "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	packCheck = func(context.Context, string) ([]protocols.PackUpdate, error) {
		return []protocols.PackUpdate{
			{Dir: "news", From: from, To: to, Changes: []string{"changed protocols/news.yaml"}},
			{Dir: "pinned", Note: "pinned to commit 3333333"},
			{Dir: "broken", Err: errors.New("offline")},
		}, nil
	}
	var applied []string
	packApply = func(_ context.Context, _ string, u protocols.PackUpdate) error {
		applied = append(applied, u.Dir)
		return nil
	}
	ctx := context.Background()
	ups, err := td.CheckPackUpdates(ctx)
	if err != nil || len(ups) != 3 || !ups[0].Pending || ups[0].To != to || ups[1].Pending || ups[1].Note == "" || ups[2].Error != "offline" {
		t.Fatalf("check: %+v %v", ups, err)
	}
	if err := td.ApplyPackUpdate(ctx, "news"); err != nil || len(applied) != 1 || applied[0] != "news" {
		t.Fatalf("apply: %v %v", err, applied)
	}
	if err := td.ApplyPackUpdate(ctx, "pinned"); err == nil || len(applied) != 1 {
		t.Fatalf("a pinned pack moved: %v %v", err, applied)
	}
	if err := td.ApplyPackUpdate(ctx, "nope"); err == nil {
		t.Fatal("no error for a pack that isn't installed")
	}
}

// A preview reads a pack's protocols without installing it.
func TestPackPreviewInstallsNothing(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "protocols"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "protocols", "news.yaml"), []byte("name: news digest\nschedule: \"0 7 * * *\"\nrequires: [web]\nprompt: Read the front pages.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "one"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	oldPreview := packPreview
	t.Cleanup(func() { packPreview = oldPreview })
	const addr = "https://example.com/owner/news"
	packPreview = func(ctx context.Context, src string) (string, []protocols.Protocol, error) {
		if src != addr {
			return "", nil, fmt.Errorf("unexpected source %q", src)
		}
		return oldPreview(ctx, fileURL(repo)) // the same pack, without the network
	}
	pv, err := td.PreviewPack(context.Background(), addr)
	if err != nil || len(pv.Protocols) != 1 {
		t.Fatalf("preview %+v %v", pv, err)
	}
	if p := pv.Protocols[0]; p.Name != "news digest" || p.Schedule != "0 7 * * *" || p.Prompt != "Read the front pages." || len(p.Requires) != 1 {
		t.Fatalf("protocol %+v", p)
	}
	if pks, _ := protocols.InstalledPacks(td.Config().ProtocolsDir); len(pks) != 0 {
		t.Fatalf("the preview installed %+v", pks)
	}
}

// The web preview, like the chat tool, never reads a folder on this
// computer: only a registry name or a git address.
func TestPackPreviewRefusesALocalFolder(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	oldPreview := packPreview
	t.Cleanup(func() { packPreview = oldPreview })
	called := false
	packPreview = func(context.Context, string) (string, []protocols.Protocol, error) {
		called = true
		return "", nil, nil
	}
	for _, src := range []string{t.TempDir(), fileURL(t.TempDir()), "./packs/news", "../x/y"} {
		if _, err := td.PreviewPack(context.Background(), src); err == nil {
			t.Errorf("%s was previewed", src)
		}
	}
	if called {
		t.Fatal("a local source reached the preview")
	}
}

// Moving a pack's routine makes the owner's own copy, which shadows the
// pack's and is what runs from then on; the pack's file is left as it came.
// Off, on and Skip next go through the same door.
func TestReschedulingAPackProtocolWritesAShadowingCopy(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	ctx := context.Background()
	dir := td.Config().ProtocolsDir
	pdir := filepath.Join(protocols.PacksDir(dir), "news", "protocols")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	packFile := filepath.Join(pdir, "headlines.yaml")
	pack := "name: nightly headlines\nschedule: \"0 21 * * *\"\nprompt: Read the front pages.\n"
	if err := os.WriteFile(packFile, []byte(pack), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := td.ReloadProtocols(); err != nil {
		t.Fatal(err)
	}
	if err := td.RescheduleProtocol(ctx, "Nightly Headlines", "30 8 * * 1-5"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(packFile); string(b) != pack {
		t.Fatalf("the pack's file changed:\n%s", b)
	}
	p, ok := protocols.Find(td.Protocols(), "nightly headlines")
	if !ok || p.Pack != "" || p.Schedule != "30 8 * * 1-5" || filepath.Dir(p.Source) != dir {
		t.Fatalf("the owner's copy should be the one loaded: %+v", p)
	}
	info := installedInfo(t, td, "nightly headlines")
	if info.When != "weekdays at 8:30" || info.Pack != "" || info.Skipping {
		t.Fatalf("the page's view: %+v", info)
	}
	if err := td.SkipNextProtocol(ctx, "nightly headlines"); err != nil {
		t.Fatal(err)
	}
	if !installedInfo(t, td, "nightly headlines").Skipping {
		t.Fatal("the page should say the next one is being skipped")
	}
	if err := td.SetProtocolEnabled(ctx, "nightly headlines", false); err != nil {
		t.Fatal(err)
	}
	if info := installedInfo(t, td, "nightly headlines"); info.Enabled || info.Skipping {
		t.Fatalf("off: %+v", info)
	}
	if err := td.SkipNextProtocol(ctx, "nightly headlines"); err == nil {
		t.Fatal("a routine that is off has no run to skip")
	}
	if err := td.RescheduleProtocol(ctx, "nightly headlines", "0 0 7 * * *"); err == nil {
		t.Fatal("a schedule that can't run was saved")
	}
}

// update_protocol is one of the twin's tools, and asks first.
func TestUpdateProtocolIsATool(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	tl, ok := td.agent.Tools().Get("update_protocol")
	if !ok || tl.Risk() != tools.RiskWrite {
		t.Fatalf("update_protocol: %v", ok)
	}
}

func installedInfo(t *testing.T, td *testDaemon, name string) api.ProtocolInfo {
	t.Helper()
	for _, p := range td.InstalledProtocols(context.Background()) {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("%q isn't listed", name)
	return api.ProtocolInfo{}
}
