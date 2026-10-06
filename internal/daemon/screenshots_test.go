package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

func TestScreenshotPathsStayInTheDataDir(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	sibling := filepath.Join(root, "data-other")
	for _, d := range []string{data, sibling, filepath.Join(data, "chrome-profile")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) string {
		if err := os.WriteFile(p, []byte("\x89PNG"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	shot := write(filepath.Join(data, "browser-1-1.png"))
	outside := write(filepath.Join(root, "passport.png"))
	write(filepath.Join(sibling, "x.png"))
	write(filepath.Join(data, "chrome-profile", "avatar.png"))
	empty := filepath.Join(data, "empty.png")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(data, "screenshot-9.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	for p, want := range map[string]bool{
		shot:                                true,
		data + "/../passport.png":           false, // climbs out
		data + "/./../data/../passport.png": false,
		sibling + "/x.png":                  false, // shares the prefix, not the directory
		data + "/chrome-profile/avatar.png": false, // not where screenshots go
		empty:                               false,
		link:                                false, // a link out of the directory
		data + "/browser-1-1.jpg":           false,
		"browser-1-1.png":                   false, // relative
		data + "/../data/browser-1-1.png":   true,  // cleans to the real one
	} {
		if _, ok := screenshotFile(data, p); ok != want {
			t.Errorf("screenshotFile(%s) = %v, want %v", strings.TrimPrefix(p, root), ok, want)
		}
	}
	reply := "Here it is: " + shot + " and also " + data + "/../passport.png"
	if got := screenshotPaths(reply, data); len(got) != 1 || got[0] != shot {
		t.Fatalf("screenshotPaths picked %v", got)
	}
}

// pictureChat is a channel that records the images it is asked to send.
type pictureChat struct {
	owner  string
	images []string // chatID|path
}

func (p *pictureChat) Name() string                                  { return "telegram" }
func (p *pictureChat) Start(context.Context, channels.Handler) error { return nil }
func (p *pictureChat) Send(context.Context, string, string) error    { return nil }
func (p *pictureChat) OwnerChatID() string                           { return p.owner }
func (p *pictureChat) SendImage(_ context.Context, chatID, path, _ string) error {
	p.images = append(p.images, chatID+"|"+path)
	return nil
}

func TestScreenshotsGoOnlyToTheOwnersChat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MIRRIN_HOME", home)
	cfg := config.Default()
	cfg.Channels.WhatsApp.Enabled = false
	cfg.Skills.Browser.Enabled = false
	cfg.DataDir = filepath.Join(home, "data")
	cfg.ProtocolsDir = filepath.Join(home, "protocols")
	cfg.LLM.APIKey = "test-key"
	d, err := New(cfg, Options{Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.store.Close()
	shot := filepath.Join(cfg.DataDir, "browser-1-1.png")
	if err := os.WriteFile(shot, []byte("\x89PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	ch := &pictureChat{owner: "111"}
	d.channels["telegram"] = ch
	ctx := context.Background()
	// A stranger's chat: neither a named screenshot nor the "yes N" page shot.
	_ = d.Send(ctx, "telegram:999", "Look: "+shot)
	_ = d.Send(ctx, "telegram:999", "I've asked. Reply yes 4 when ready.")
	if len(ch.images) != 0 {
		t.Fatalf("owner's screenshots went to a stranger: %v", ch.images)
	}
	_ = d.Send(ctx, "telegram:111", "Shall I submit? Reply yes 4.")
	if len(ch.images) != 1 || ch.images[0] != "111|"+shot {
		t.Fatalf("owner should get the page before approving: %v", ch.images)
	}
}

// /screen/shot serves only what a waiting approval shows (the page a
// browser action will submit), not every screenshot of a signed-in page
// kept for the model: a device paired to view the screen sees what it may
// be asked about, and nothing else (browser merged with devices' scopes).
func TestTheScreenServesOnlyAnApprovalsScreenshot(t *testing.T) {
	td := newTestDaemon(t, butler)
	data := td.Config().DataDir
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	shot := func(name string) string {
		p := filepath.Join(data, name)
		if err := os.WriteFile(p, []byte("\x89PNG"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	asked, other := shot("browser-1-1.png"), shot("browser-2-1.png")
	ctx := context.Background()
	key := "telegram:owner#task-1"
	if err := td.store.AppendMessage(ctx, key, llm.Text(llm.RoleUser, "Screenshot of the form: "+asked)); err != nil {
		t.Fatal(err)
	}
	if _, err := td.store.CreateApproval(ctx, key, "browser_act", []byte(`{"ref":"submit"}`), "click Submit", tools.RiskWrite); err != nil {
		t.Fatal(err)
	}
	if p, ok := td.ScreenshotPath(asked); !ok || p != asked {
		t.Fatalf("the approval's own screenshot: %q %v", p, ok)
	}
	if _, ok := td.ScreenshotPath(other); ok {
		t.Fatal("a screenshot no approval shows was served")
	}
	sd := td.screenData(ctx)
	if len(sd.Approvals) != 1 || !strings.Contains(sd.Approvals[0].Screenshot, "browser-1-1.png") {
		t.Fatalf("the card links %+v", sd.Approvals)
	}
}

// /forget in one chat removes the screenshots that chat showed, and leaves
// another chat's alone.
func TestForgetRemovesOnlyThatChatsScreenshots(t *testing.T) {
	td := newTestDaemon(t, butler)
	data := td.Config().DataDir
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	shot := func(name string) string {
		p := filepath.Join(data, name)
		if err := os.WriteFile(p, []byte("\x89PNG"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mine, theirs := shot("browser-1-1.png"), shot("browser-2-1.png")
	ctx := context.Background()
	chatA, chatB := "telegram:111", "telegram:222"
	if err := td.store.AppendMessage(ctx, chatA, llm.Text(llm.RoleAssistant, "Here's the page: "+mine)); err != nil {
		t.Fatal(err)
	}
	if err := td.store.AppendMessage(ctx, chatB, llm.Text(llm.RoleAssistant, "Here's yours: "+theirs)); err != nil {
		t.Fatal(err)
	}
	if _, err := td.command(ctx, chatA, "/forget"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Fatalf("chat A's screenshot outlived /forget: %v", err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Fatalf("chat B's screenshot went with chat A's /forget: %v", err)
	}
}

// fetch_url and the browser share skills.web.allow_hosts (security merged
// with browser). A change saved from the settings reaches both at once,
// without a restart: before, both kept the list they started with and
// nothing said a restart was needed.
func TestAllowHostsChangesReachFetchAndTheBrowser(t *testing.T) {
	td := newTestDaemon(t, butler)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("hello from the NAS")) }))
	defer srv.Close()
	cfg := td.Config()
	cfg.Skills.Web.Enabled = true
	*td.cfg = cfg
	td.applyAllowHosts(cfg.Skills.Web)
	fetch := func() (string, error) {
		in, _ := json.Marshal(map[string]string{"url": srv.URL})
		return td.agent.Tools().Run(context.Background(), "fetch_url", tools.Call{ChatKey: ownerKey, Input: in})
	}
	if out, err := fetch(); err == nil && strings.Contains(out, "hello from the NAS") {
		t.Fatal("setup: fetch_url reached this computer without an allow_hosts entry")
	}
	if err := td.UpdateConfig(func(c *config.Config) { c.Skills.Web.Enabled, c.Skills.Web.AllowHosts = true, []string{"127.0.0.1"} }); err != nil {
		t.Fatal(err)
	}
	if out, err := fetch(); err != nil || !strings.Contains(out, "hello from the NAS") {
		t.Fatalf("after allowing 127.0.0.1 from the settings: %q %v", out, err)
	}
}
