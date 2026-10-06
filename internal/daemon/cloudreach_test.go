package daemon

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/backup"
	"github.com/MavrkAI/Mirrin/internal/certwatch"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/cloud"
	"github.com/MavrkAI/Mirrin/internal/cloud/cloudtest"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/reach"
)

// spyChannel counts every Start.
type spyChannel struct {
	fakeChannel
	starts atomic.Int32
}

func (s *spyChannel) Start(ctx context.Context, h channels.Handler) error {
	s.starts.Add(1)
	<-ctx.Done()
	return nil
}

// useFakeKeys makes the daemon trust the fake control plane's keys.
func useFakeKeys(t *testing.T, f *cloudtest.Fake) {
	prev := cloudKeys
	cloudKeys = func() map[string]ed25519.PublicKey { return f.Keys() }
	t.Cleanup(func() { cloudKeys = prev })
}

// linkWith links the test daemon's machine with f, as `mirrin cloud link`
// would, with the link made at linkedAt by both clocks.
func linkWith(t *testing.T, td *testDaemon, f *cloudtest.Fake, linkedAt func() time.Time) *cloud.Client {
	t.Helper()
	f.SetClock(linkedAt)
	c, err := cloud.New(td.cfg.DataDir, f.URL, f.Keys())
	if err != nil {
		t.Fatal(err)
	}
	c.Now = linkedAt
	ls, err := c.StartLink(context.Background(), "ember-otter-42", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Pay(ls.ID); err != nil {
		t.Fatal(err)
	}
	if st, _, err := c.PollLink(context.Background(), ls.ID); err != nil || st != cloud.LinkActive {
		t.Fatalf("link: %q %v", st, err)
	}
	f.SetClock(time.Now)
	return c
}

func refreshCount(f *cloudtest.Fake) int {
	n := 0
	for _, q := range f.Requests() {
		if q.Path == "/v1/entitlement/refresh" {
			n++
		}
	}
	return n
}

func twoDaysAgo() time.Time { return time.Now().Add(-48 * time.Hour) }

// A machine that stood by after a backup handover sends nothing to the
// linked service, though its daily refresh is overdue; the same machine
// without the handover refreshes at once (the positive control).
func TestBackupStandbyKeepsTheLinkedServiceQuiet(t *testing.T) {
	for _, standby := range []bool{false, true} {
		t.Run(map[bool]string{false: "not standing by", true: "standing by"}[standby], func(t *testing.T) {
			td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
			f := cloudtest.NewFake(t)
			useFakeKeys(t, f)
			stubDesktop(t)
			linkWith(t, td, f, twoDaysAgo)
			if standby {
				if err := backup.StandBy(td.cfg.DataDir, backup.Handover{Name: "handover-20260927T090000Z-0a0b0c0d.age", HostLabel: "Akshay's Mac mini", At: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			before := refreshCount(f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			td.startCloud(ctx)
			td.startBackup(ctx)
			if !standby {
				waitUntil(t, 10*time.Second, "the overdue refresh", func() bool { return refreshCount(f) > before })
				// Let the loop save what it got before the folder is deleted.
				run := td.cloudRun()
				run.mu.Lock()
				kept := run.kept
				run.mu.Unlock()
				cancel()
				select {
				case <-kept:
				case <-time.After(10 * time.Second):
					t.Fatal("the refresh loop didn't stop")
				}
				return
			}
			// The standby pauses the twin in the background; wait for it
			// rather than a fixed sleep, which a busy machine can outrun.
			waitUntil(t, 10*time.Second, "the standby pause", func() bool { return td.paused.Load() })
			time.Sleep(300 * time.Millisecond) // room for a wrong refresh to show; it can only fail on a real one
			if n := refreshCount(f) - before; n != 0 {
				t.Fatalf("a machine standing by sent %d refreshes", n)
			}
		})
	}
}

// A handover found while running quiets the link at once.
func TestAFreshHandoverQuietsTheLink(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	f := cloudtest.NewFake(t)
	useFakeKeys(t, f)
	stubDesktop(t)
	linkWith(t, td, f, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	td.startCloud(ctx)
	run := td.cloudRun()
	run.mu.Lock()
	linkCtx := run.ctx
	run.mu.Unlock()
	if linkCtx == nil || linkCtx.Err() != nil {
		t.Fatal("the link isn't running")
	}
	td.standBy(&backup.Handover{HostLabel: "Akshay's Mac mini", At: time.Now()}, true)
	if linkCtx.Err() == nil {
		t.Fatal("the link still runs on a machine standing by")
	}
}

// Another machine took the handle (a higher generation): this machine
// stands by from the start, pauses, keeps its chat apps off for good, and
// sends nothing more; the menu says where it went.
func TestAMovedHandleStandsByAndChannelsStayOff(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	f := cloudtest.NewFake(t)
	useFakeKeys(t, f)
	stubDesktop(t)
	c := linkWith(t, td, f, time.Now)

	spy := &spyChannel{fakeChannel: fakeChannel{name: "telegram", owner: "owner", out: make(chan string, 8)}}
	td.launch("telegram", spy)
	waitUntil(t, 5*time.Second, "the channel", func() bool { return spy.starts.Load() == 1 })

	if _, err := f.Supersede("ember-otter-42"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Refresh(context.Background()); err == nil {
		t.Fatal("refresh after supersede succeeded")
	}
	before := len(f.Requests())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	td.startCloud(ctx)
	if !td.paused.Load() {
		t.Fatal("not paused")
	}
	if _, ok := td.channel("telegram"); ok {
		t.Fatal("the chat app is still running")
	}
	if err := td.StartChannel("telegram"); err == nil || !strings.Contains(err.Error(), "standing by") {
		t.Fatalf("start while standing by: %v", err)
	}
	td.launch("telegram", spy)
	time.Sleep(300 * time.Millisecond)
	if n := spy.starts.Load(); n != 1 {
		t.Fatalf("the chat app started %d times", n)
	}
	if n := len(f.Requests()) - before; n != 0 {
		t.Fatalf("%d requests from a machine standing by", n)
	}
	if line := td.ReachLine(); !strings.HasPrefix(line, td.cfg.Name+" moved to another machine on ") {
		t.Fatalf("menu line %q", line)
	}
	if st, detail, _ := td.cloudHealth(); !strings.Contains(detail, "moved to another machine") || st == "" {
		t.Fatalf("health %q", detail)
	}
}

// The Reach page: Cloud is last, says it is coming in a build that trusts
// no signing keys, and one click links and switches, keeping the free mode
// as the fallback.
func TestReachPageLinksAndSwitches(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	stubDesktop(t)
	noTailscale(t)
	var _ api.ReachBackend = td.Daemon // the page is the twin's
	prev := cloudKeys
	cloudKeys = func() map[string]ed25519.PublicKey { return nil }
	t.Cleanup(func() { cloudKeys = prev })
	info := td.ReachInfo(context.Background())
	var kinds []string
	for _, c := range info.Cards {
		kinds = append(kinds, c.Kind)
	}
	if strings.Join(kinds, ",") != "tailscale,relay,cloud" || info.Cards[2].Available {
		t.Fatalf("cards %+v", info.Cards)
	}
	if _, err := td.ReachUseCloud(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "coming") {
		t.Fatalf("before launch: %v", err)
	}

	f := cloudtest.NewFake(t)
	useFakeKeys(t, f)
	prevPoll := linkPollEvery
	linkPollEvery = 10 * time.Millisecond
	t.Cleanup(func() { linkPollEvery = prevPoll })
	if err := td.UpdateConfig(func(c *config.Config) { c.Cloud.API = f.URL; c.Reach.Mode = "tailscale" }); err != nil {
		t.Fatal(err)
	}
	if _, err := td.ReachUseCloud(context.Background(), "Bad Name"); err == nil {
		t.Fatal("a bad name was sent")
	}
	checkout, err := td.ReachUseCloud(context.Background(), "ember-otter-42")
	if err != nil || checkout == "" {
		t.Fatalf("use cloud: %q %v", checkout, err)
	}
	if res, err := http.Get(checkout); err != nil || res.StatusCode != 200 { // the fake checkout pays
		t.Fatalf("checkout: %v %v", res, err)
	}
	waitUntil(t, 10*time.Second, "the switch", func() bool { return td.Config().Reach.Mode == "cloud" })
	if fb := td.Config().Reach.Fallback; fb != "tailscale" {
		t.Fatalf("fallback %q", fb)
	}
	info = td.ReachInfo(context.Background())
	cc := info.Cards[len(info.Cards)-1]
	if cc.Kind != api.ReachCloud || !cc.Linked || !cc.Using || cc.State != "active" {
		t.Fatalf("cloud card %+v", cc)
	}
	td.cloudRun().mu.Lock()
	cancel := td.cloudRun().cancel
	td.cloudRun().mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Expiry ends the handle with one notice and the free fallback; health
// says so without naming the service.
func TestCloudExpiryFallsBackWithOneNotice(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	shown := stubDesktop(t)
	f := cloudtest.NewFake(t)
	useFakeKeys(t, f)
	linkWith(t, td, f, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	td.startCloud(ctx)
	srv := api.New("127.0.0.1:0", "t", nil)
	for range 2 {
		td.cloudEnded(context.Background(), srv, reach.CloudExpired, time.Time{})
	}
	if n := len(*shown); n != 1 {
		t.Fatalf("%d notices, want one: %v", n, *shown)
	}
	if st, detail, _ := td.cloudHealth(); st == "" || !strings.Contains(detail, "isn't active") {
		t.Fatalf("health %q", detail)
	}
}

func stubDesktop(t *testing.T) *[]string {
	var shown []string
	prev := desktopNotify
	desktopNotify = func(_, body string) error { shown = append(shown, body); return nil }
	t.Cleanup(func() { desktopNotify = prev })
	return &shown
}

func waitUntil(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cloudRig runs the daemon's cloud reach loop against the fake control
// plane, with relays nobody answers at (the tunnels keep retrying; the loop
// is what's under test), and no CT, CAA or CA traffic.
func cloudReachRig(t *testing.T) (*testDaemon, *cloudtest.Fake, *cloud.Client, *spyChannel, context.Context) {
	t.Helper()
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	f := cloudtest.NewFake(t)
	useFakeKeys(t, f)
	stubDesktop(t)
	f.SetRelays([]entitle.Relay{{ID: "r1", URL: "wss://127.0.0.1:1/v1/tunnel", IPs: []string{"127.0.0.1"}}})
	c := linkWith(t, td, f, time.Now)
	prevDeps, prevCalm := cloudDeps, cloudRestartCalm
	cloudDeps = func(d *reach.Deps) {
		d.CTSources = []certwatch.CTSource{}
		d.CAA = func(context.Context, string) ([]certwatch.CAA, error) { return nil, nil }
		d.Reach.ACMEDirectory = "https://127.0.0.1:1/directory"
		d.CloudCheck = 20 * time.Millisecond
	}
	cloudRestartCalm = 0
	t.Cleanup(func() { cloudDeps, cloudRestartCalm = prevDeps, prevCalm })

	spy := &spyChannel{fakeChannel: fakeChannel{name: "telegram", owner: "owner", out: make(chan string, 8)}}
	td.launch("telegram", spy)
	waitUntil(t, 5*time.Second, "the channel", func() bool { return spy.starts.Load() == 1 })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	td.startCloud(ctx)
	td.startCloudReach(ctx, api.New("127.0.0.1:0", "t", nil))
	waitUntil(t, 10*time.Second, "the cloud endpoint", func() bool { return td.cloudEndpoint() != nil })
	return td, f, c, spy, ctx
}

func (td *testDaemon) reaching() bool {
	run := td.cloudRun()
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.reaching
}

// Another machine takes the handle while this one serves it: the running
// endpoint ends, the daemon stands by (paused, the chat app stopped and
// never started again), and nothing more is sent.
func TestCloudReachLoopStandsByAtRuntime(t *testing.T) {
	td, f, c, spy, _ := cloudReachRig(t)
	if _, err := f.Supersede("ember-otter-42"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Refresh(context.Background()); err == nil {
		t.Fatal("refresh after supersede succeeded")
	}
	waitUntil(t, 10*time.Second, "standby", func() bool { _, ok := td.cloudStandingBy(); return ok && td.paused.Load() })
	waitUntil(t, 5*time.Second, "the loop to end", func() bool { return !td.reaching() })
	if _, ok := td.channel("telegram"); ok {
		t.Fatal("the chat app is still running")
	}
	before := len(f.Requests())
	if err := td.StartChannel("telegram"); err == nil || !strings.Contains(err.Error(), "standing by") {
		t.Fatalf("start while standing by: %v", err)
	}
	td.launch("telegram", spy)
	time.Sleep(500 * time.Millisecond)
	if n := spy.starts.Load(); n != 1 {
		t.Fatalf("the chat app started %d times", n)
	}
	if n := len(f.Requests()) - before; n != 0 {
		t.Fatalf("%d requests from a machine standing by", n)
	}
	if td.cloudEndpoint() != nil {
		t.Fatal("an endpoint still runs")
	}
}

// A refreshed entitlement naming other relays starts the endpoint again.
func TestCloudReachLoopRestarts(t *testing.T) {
	td, f, c, _, _ := cloudReachRig(t)
	first := td.cloudEndpoint()
	f.SetRelays([]entitle.Relay{{ID: "r2", URL: "wss://127.0.0.1:2/v1/tunnel", IPs: []string{"127.0.0.1"}}})
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, "a new endpoint", func() bool {
		e := td.cloudEndpoint()
		return e != nil && e != first
	})
	if end, _ := first.Ended(); end != reach.CloudRestart {
		t.Fatalf("the first ended %v", end)
	}
	if !strings.Contains(td.cloudEndpoint().RelayURL, "127.0.0.1:2") {
		t.Fatalf("relays %q", td.cloudEndpoint().RelayURL)
	}
}

// A backup handover while the endpoint runs: the loop goes quiet (no
// fallback, no restart) and nothing more is sent.
func TestCloudReachLoopQuietAfterHandover(t *testing.T) {
	td, f, _, _, _ := cloudReachRig(t)
	h := backup.Handover{Name: "handover-20260927T090000Z-0a0b0c0d.age", HostLabel: "Akshay's Mac mini", At: time.Now()}
	if err := backup.StandBy(td.cfg.DataDir, h); err != nil {
		t.Fatal(err)
	}
	td.standBy(&h, true)
	waitUntil(t, 10*time.Second, "the loop to end", func() bool { return !td.reaching() })
	before := len(f.Requests())
	time.Sleep(500 * time.Millisecond)
	if n := len(f.Requests()) - before; n != 0 {
		t.Fatalf("%d requests after the handover", n)
	}
	run := td.cloudRun()
	run.mu.Lock()
	expired := run.expired
	run.mu.Unlock()
	if expired || td.cloudEndpoint() != nil {
		t.Fatal("the loop fell back or kept an endpoint on a machine standing by")
	}
}

// The Reach page's Cloud card on a copy standing by after a handover sends
// nothing, not even a checkout.
func TestReachUseCloudRefusesWhileStandingBy(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("ok") })
	stubDesktop(t)
	f := cloudtest.NewFake(t)
	useFakeKeys(t, f)
	if err := td.UpdateConfig(func(c *config.Config) { c.Cloud.API = f.URL }); err != nil {
		t.Fatal(err)
	}
	if err := backup.StandBy(td.cfg.DataDir, backup.Handover{Name: "handover-20260927T090000Z-0a0b0c0d.age", HostLabel: "Akshay's Mac mini", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := td.ReachUseCloud(context.Background(), "ember-otter-42"); err == nil || !strings.Contains(err.Error(), "standing by") {
		t.Fatalf("use cloud while standing by: %v", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("%d requests", n)
	}
}
