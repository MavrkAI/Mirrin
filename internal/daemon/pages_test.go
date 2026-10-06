package daemon

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/tailscale"
)

// The menu reads these from the twin (internal/tray's pagesBackend).
var _ interface {
	PageURL(string) string
	DeviceCount() int
	ReachLine() string
	RestoreReview(context.Context) api.RestoreReview
} = (*Daemon)(nil)

// The pages are the twin's.
var _ api.LocalPages = (*Daemon)(nil)

func noTailscale(t *testing.T) {
	t.Helper()
	prev := tailscaleStatus
	tailscaleStatus = func(context.Context) (tailscale.Status, error) {
		return tailscale.Status{}, &tailscale.Problem{Message: "could not find the Tailscale command; install its CLI"}
	}
	t.Cleanup(func() { tailscaleStatus = prev })
}

// Revoking a device from the Devices page (or "That wasn't me") cuts it
// off at once, and asks for a backup soon so a restore can't bring it back.
func TestRevokeFromThePageCutsOffAndBacksUp(t *testing.T) {
	noTailscale(t)
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	// Everything is mounted before the server starts, as in Run.
	srv := api.New("127.0.0.1:0", testMaster, d)
	d.attachDevices(t.Context(), srv)
	d.attachPages(t.Context(), srv)
	s := &backup.Scheduler{DataDir: t.TempDir(), Engine: func() (*backup.Engine, error) { return nil, nil }}
	phone, tok, err := d.deviceStore().Add("Phone", devices.KindPWA, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	d.backups = s
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(sctx, ln, api.LoopbackOnly, "loopback"); close(done) }()
	t.Cleanup(func() { stop(); <-done })
	do := func(method, path, auth string) int {
		r, _ := http.NewRequest(method, "http://"+ln.Addr().String()+path, nil)
		r.Header.Set("Authorization", "Bearer "+auth)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := do("GET", "/status", tok); code != 200 {
		t.Fatalf("before: %d", code)
	}
	if code := do("POST", "/devices/"+phone.ID+"/revoke", testMaster); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if code := do("GET", "/status", tok); code != http.StatusUnauthorized {
		t.Fatalf("the phone's next request: %d, want 401", code)
	}
	if !s.Pending() {
		t.Fatal("no backup asked for after the revoke")
	}
	if n := d.DeviceCount(); n != 0 {
		t.Fatalf("Devices (%d) after the revoke", n)
	}
	if line := d.ReachLine(); line != "Only on this Mac" {
		t.Fatalf("reach line %q", line)
	}
	if u := d.PageURL("/backup"); !strings.HasSuffix(u, "/backup?token="+testMaster) {
		t.Fatalf("PageURL %q", u)
	}
}

// The Backup page's checks: a folder inside the twin's own is refused
// before any words; one outside is set up with the words' public keys only,
// and moving keeps them.
func TestBackupPageSetsUpAndMoves(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	ctx := context.Background()
	if _, err := d.BackupCheck(ctx, api.BackupWhere{Target: "folder", Path: filepath.Join(config.Home(), "backups")}); err == nil || !strings.Contains(err.Error(), "own folder") {
		t.Fatalf("inside the twin: %v", err)
	}
	var h *api.HumanError
	if _, err := d.BackupCheck(ctx, api.BackupWhere{Target: "folder", Path: "backups"}); !errors.As(err, &h) || !strings.Contains(h.Sentence, "whole path") {
		t.Fatalf("relative path: %v", err)
	}
	if _, err := d.BackupCheck(ctx, api.BackupWhere{Target: "s3", S3URL: "s3://bucket/mirrin", AccessKey: "only-half"}); !errors.As(err, &h) || !strings.Contains(h.Sentence, "both halves") {
		t.Fatalf("half a key: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "Backup")
	where, err := d.BackupCheck(ctx, api.BackupWhere{Target: "folder", Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := backup.NewPhrase()
	if err := d.BackupSetup(ctx, p, where); err != nil {
		t.Fatal(err)
	}
	st := d.BackupStatus(ctx)
	if !st.On || st.Target != "folder" || st.Path != dir || st.KitID != backup.KitID(p) {
		t.Fatalf("status %+v", st)
	}
	// The words are in no file the twin or its backup wrote: not the whole
	// phrase, and not any three in a row, however they are joined.
	words := p.Words()
	var needles []string
	for _, sep := range []string{" ", "-", ",", "", "\n"} {
		needles = append(needles, strings.Join(words, sep))
		for i := 0; i+3 <= len(words); i++ {
			needles = append(needles, strings.Join(words[i:i+3], sep))
		}
	}
	for _, root := range []string{config.Home(), dir} {
		err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, n := range needles {
				if strings.Contains(strings.ToLower(string(raw)), n) {
					t.Errorf("the words are in %s", path)
					break
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	dir2 := filepath.Join(t.TempDir(), "Other")
	where, _ = d.BackupCheck(ctx, api.BackupWhere{Target: "folder", Path: dir2})
	if err := d.BackupMove(ctx, where); err != nil {
		t.Fatal(err)
	}
	if st := d.BackupStatus(ctx); st.Path != dir2 || st.KitID != backup.KitID(p) {
		t.Fatalf("after moving %+v", st)
	}
}

// After a restore the review is pending until the owner finishes it.
func TestRestoreReviewState(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	ctx := context.Background()
	if d.RestoreReview(ctx).Pending {
		t.Fatal("pending without a restore")
	}
	_ = backup.UpdateState(d.Config().DataDir, func(s *backup.State) { s.RestoredAt = time.Now() })
	if !d.RestoreReview(ctx).Pending {
		t.Fatal("not pending after a restore")
	}
	if err := d.FinishRestoreReview(ctx); err != nil || d.RestoreReview(ctx).Pending {
		t.Fatalf("still pending after the review: %v", err)
	}
}

// Trust marks what this twin uses, and never names the paid service.
func TestTrustMarksWhatIsInUse(t *testing.T) {
	d, _ := newFirstRunDaemon(t, func(c *config.Config) {
		c.Reach = config.Reach{Mode: "relay", RelayURL: "wss://relay.example.com/v1/tunnel", Hostname: "twin.example.com"}
	})
	defer d.Close()
	info := d.Trust(context.Background())
	on := map[string]string{}
	for _, o := range info.Outbound {
		if o.On {
			on[o.ID] = o.Where
		}
	}
	if on["relay"] != "relay.example.com" || on["certificates"] != "twin.example.com" {
		t.Fatalf("relay lines: %v", on)
	}
	if _, ok := on["tailscale"]; ok {
		t.Fatal("Tailscale marked in relay mode")
	}
	if _, ok := on["linked"]; ok {
		t.Fatal("a never-linked twin shows a linked service")
	}
	if _, ok := on["model"]; !ok {
		t.Fatal("the model isn't marked")
	}
}

// Looking at the Trust page changes nothing: with no Google sign-in the
// google line is off and no Google skill gets switched on.
func TestTrustLeavesGoogleAloneWhenNotConnected(t *testing.T) {
	d, _ := newFirstRunDaemon(t, nil)
	defer d.Close()
	before := d.Config().Skills
	for _, o := range d.Trust(context.Background()).Outbound {
		if o.ID == "google" && o.On {
			t.Fatalf("google marked in use with no sign-in: %q", o.Where)
		}
	}
	after := d.Config().Skills
	if after.Calendar.Enabled != before.Calendar.Enabled || after.Gmail.Enabled != before.Gmail.Enabled || after.Drive.Enabled != before.Drive.Enabled {
		t.Fatalf("the Trust page switched Google skills: before %v/%v/%v, after %v/%v/%v",
			before.Calendar.Enabled, before.Gmail.Enabled, before.Drive.Enabled,
			after.Calendar.Enabled, after.Gmail.Enabled, after.Drive.Enabled)
	}
	if raw, _ := d.store.Get(context.Background(), googleConnectedKey); raw != "" {
		t.Fatalf("the Trust page recorded a Google connection at %s", raw)
	}
}

// A Tailscale that never answers is asked again in minutes, not seconds:
// each ask hangs until it is killed.
func TestAHungTailscaleIsLeftAlone(t *testing.T) {
	prev := tailscaleStatus
	t.Cleanup(func() { tailscaleStatus = prev })
	asks := 0
	tailscaleStatus = func(ctx context.Context) (tailscale.Status, error) {
		asks++
		<-ctx.Done()
		return tailscale.Status{}, ctx.Err()
	}
	d := &Daemon{}
	t.Cleanup(func() { tsCaches.Delete(d) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already over, so the ask "hangs" at once
	_, _ = d.tailnet(ctx)
	_, _ = d.tailnet(context.Background())
	if asks != 1 {
		t.Fatalf("asked %d times", asks)
	}
	v, _ := tsCaches.Load(d)
	if c := v.(*tsCache); c.ttl != tsHungFor {
		t.Fatalf("kept for %v", c.ttl)
	}
}
