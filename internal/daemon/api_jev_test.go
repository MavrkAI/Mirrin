package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/jev"
	"github.com/MavrkAI/Mirrin/internal/jev/jevtest"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/memory"
)

// newJevDaemon is a test daemon that sends Jev questions to a fake.
func newJevDaemon(t *testing.T) (*testDaemon, *jevtest.Server) {
	t.Helper()
	t.Setenv("TYPESAFE_API_KEY", "")
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("hi") })
	srv := jevtest.New(t)
	td.jev.url = srv.URL
	return td, srv
}

// syncBuffer is a log the daemon can write while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var pingQuestion = map[string]jev.Question{"q": jev.Noul{Instructions: "Is `owner_replied` a yes?"}}

// Jev is asked only when switched on with a key: a key exported or saved
// alone sends nothing.
func TestJudgeOffSendsNothing(t *testing.T) {
	td, srv := newJevDaemon(t)
	ctx := context.Background()
	t.Setenv("TYPESAFE_API_KEY", srv.Key())
	if _, ok := td.judge(ctx, "test", "x", pingQuestion); ok {
		t.Fatal("judged with the key exported but Jev not switched on")
	}
	if err := config.SaveSecrets(map[string]string{"TYPESAFE_API_KEY": srv.Key()}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "")
	if _, ok := td.judge(ctx, "test", "x", pingQuestion); ok {
		t.Fatal("judged with the key saved but Jev not switched on")
	}
	if err := config.ForgetSecrets("TYPESAFE_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if err := td.UpdateConfig(func(c *config.Config) { c.Jev.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	if _, ok := td.judge(ctx, "test", "x", pingQuestion); ok {
		t.Fatal("judged with no key")
	}
	if srv.Calls() != 0 {
		t.Fatalf("%d requests to Jev", srv.Calls())
	}
	if td.Trust(ctx).Outbound[jevIndex(t)].On {
		t.Fatal("Trust shows Jev on without a key")
	}
}

func jevIndex(t *testing.T) int {
	for i, o := range api.OutboundCatalog() {
		if o.ID == "jev" {
			return i
		}
	}
	t.Fatal("no jev line on the Trust page")
	return -1
}

// Switched on with a key, judge asks, returns the answers and adds the
// call to the usage ledger; a key saved while running is used at once.
func TestJudgeAsksAndCountsUsage(t *testing.T) {
	td, srv := newJevDaemon(t)
	ctx := context.Background()
	if err := td.UpdateConfig(func(c *config.Config) { c.Jev.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveSecrets(map[string]string{"TYPESAFE_API_KEY": srv.Key()}); err != nil {
		t.Fatal(err)
	}
	srv.Answer("q", jev.Answer{Type: "noul", Noul: 0.93})
	res, ok := td.judge(ctx, "test", map[string]string{"owner_replied": "yep"}, pingQuestion)
	if !ok || res.Answers["q"].Noul != 0.93 || srv.Calls() != 1 {
		t.Fatalf("judge: %v %+v after %d calls", ok, res, srv.Calls())
	}
	if r := srv.Requests()[0]; r.Model != "jev-latest" || r.Auth != "Bearer "+srv.Key() {
		t.Fatalf("request %+v", r)
	}
	day := clock().In(td.location()).Format(memory.DayFormat)
	rows, err := td.store.Usage(ctx, day, day)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Kind == "jev.test" && r.Model == "jev-latest" && r.Calls == 1 && r.Input > 0 && r.Output > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("usage %+v", rows)
	}
	if o := td.Trust(ctx).Outbound[jevIndex(t)]; !o.On || o.Where != "api.typesafe.ai" {
		t.Fatalf("Trust: %+v", o)
	}
}

// A failure falls back quietly: false, one log line per kind per 10
// minutes with nothing that was asked in it, and the card says why.
func TestJudgeFailsQuietly(t *testing.T) {
	td, srv := newJevDaemon(t)
	ctx := context.Background()
	var logs syncBuffer
	td.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := td.UpdateConfig(func(c *config.Config) { c.Jev = config.Jev{Enabled: true, TypeSafeAPIKey: srv.Key()} }); err != nil {
		t.Fatal(err)
	}
	state := map[string]string{"owner_replied": "SECRET-REPLY"}
	srv.Fail(500, 3)
	for range 3 {
		if _, ok := td.judge(ctx, "test", state, pingQuestion); ok {
			t.Fatal("a 500 was taken as an answer")
		}
	}
	if n := strings.Count(logs.String(), "jev unavailable"); n != 1 {
		t.Fatalf("%d log lines for three failures:\n%s", n, logs.String())
	}
	if st := td.JevState(ctx); st.Problem != "Couldn't reach TypeSafe just now. Your twin carries on as usual; try again later." {
		t.Fatalf("card: %+v", st)
	}
	srv.Fail(401, 1)
	if _, ok := td.judge(ctx, "test", state, pingQuestion); ok {
		t.Fatal("a 401 was taken as an answer")
	}
	if st := td.JevState(ctx); st.Problem != "That key wasn't accepted. Check it at typesafe.ai and paste it again." {
		t.Fatalf("card after a 401: %+v", st)
	}
	if n := strings.Count(logs.String(), "jev unavailable"); n != 2 {
		t.Fatalf("a new kind of failure wasn't logged:\n%s", logs.String())
	}
	// Ten minutes on, the same kind is logged again.
	withClock(t, 11*time.Minute)
	srv.Fail(500, 1)
	td.judge(ctx, "test", state, pingQuestion)
	if n := strings.Count(logs.String(), "jev unavailable"); n != 3 {
		t.Fatalf("not logged again after ten minutes:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "SECRET") || strings.Contains(logs.String(), srv.Key()) {
		t.Fatalf("the log carries content:\n%s", logs.String())
	}
	// A slow TypeSafe costs at most the timeout.
	srv.Delay(3 * time.Second)
	start := time.Now()
	if _, ok := td.judge(ctx, "test", state, pingQuestion); ok || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("slow: %v after %v", ok, time.Since(start))
	}
	srv.Delay(0)
	// A call that works clears the card's problem.
	if _, ok := td.judge(ctx, "test", state, pingQuestion); !ok {
		t.Fatal("no answer once TypeSafe is back")
	}
	if st := td.JevState(ctx); st.Problem != "" {
		t.Fatalf("card after it worked: %+v", st)
	}
}

// The Accounts card: Check tests a key without saving it; Check and save
// keeps it in secrets.env and writes only the switch to config.yaml; Remove
// key forgets it and switches off.
func TestJevCard(t *testing.T) {
	td, srv := newJevDaemon(t)
	ctx := context.Background()
	const bad = "ts-wrong-0123456789abcdef"
	good := srv.Key()
	var he *api.HumanError

	if err := td.CheckJev(ctx, bad); !errors.As(err, &he) || he.Sentence != "That key wasn't accepted." || strings.Contains(err.Error(), bad) {
		t.Fatalf("a bad key: %v", err)
	}
	if err := td.CheckJev(ctx, good); err != nil {
		t.Fatalf("a good key: %v", err)
	}
	if r := srv.Requests(); len(r) != 2 || string(r[1].State) != `"ping"` {
		t.Fatalf("the check sent %+v", r)
	}
	if s, _ := config.ReadSecrets(); s["TYPESAFE_API_KEY"] != "" {
		t.Fatal("Check saved the key")
	}
	if err := td.CheckJev(ctx, ""); !errors.As(err, &he) {
		t.Fatalf("no key to check: %v", err)
	}
	if err := td.CheckJev(ctx, "ts key"); !errors.As(err, &he) || !strings.Contains(he.Sentence, "spaces") {
		t.Fatalf("a key with a space: %v", err)
	}

	if err := td.SaveJev(ctx, "", true, false); !errors.As(err, &he) || !strings.Contains(he.Sentence, "key first") {
		t.Fatalf("on without a key: %v", err)
	}
	if err := td.SaveJev(ctx, bad, true, false); !errors.As(err, &he) {
		t.Fatalf("a bad key saved: %v", err)
	}
	if s, _ := config.ReadSecrets(); s["TYPESAFE_API_KEY"] != "" || td.Config().Jev.Enabled {
		t.Fatal("a refused key changed something")
	}
	if err := td.SaveJev(ctx, good, true, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := config.ReadSecrets(); s["TYPESAFE_API_KEY"] != good {
		t.Fatal("the key isn't in secrets.env")
	}
	if c := td.Config(); !c.Jev.Enabled || c.Jev.TypeSafeAPIKey != "" || !c.JevOn() {
		t.Fatalf("config: %+v", c.Jev)
	}
	b, _ := os.ReadFile(config.Path())
	if !regexp.MustCompile(`(?m)^jev:\n\s+enabled: true$`).Match(b) || strings.Contains(string(b), "typesafe_api_key") {
		t.Fatalf("config.yaml:\n%s", b)
	}
	st := td.JevState(ctx)
	if !st.On || !st.HasKey || st.Masked != api.MaskKey(good) || st.Env != "TYPESAFE_API_KEY" || st.Problem != "" {
		t.Fatalf("card: %+v", st)
	}
	// Off and on again keeps the key.
	if err := td.SaveJev(ctx, "", false, false); err != nil || td.JevState(ctx).On {
		t.Fatalf("off: %v", err)
	}
	if err := td.SaveJev(ctx, "", true, false); err != nil || !td.JevState(ctx).On {
		t.Fatalf("on again: %v", err)
	}
	// An exported key wins over a saved one, so a different one is refused.
	t.Setenv("TYPESAFE_API_KEY", "ts-exported-0123456789")
	if err := td.SaveJev(ctx, good, true, false); !errors.As(err, &he) || !strings.Contains(he.Fix, "TYPESAFE_API_KEY") {
		t.Fatalf("with another key exported: %v", err)
	}
	t.Setenv("TYPESAFE_API_KEY", "")

	if err := td.SaveJev(ctx, "", false, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := config.ReadSecrets(); s["TYPESAFE_API_KEY"] != "" {
		t.Fatal("Remove key left it in secrets.env")
	}
	if c := td.Config(); c.Jev.Enabled || c.JevOn() {
		t.Fatalf("still on after Remove key: %+v", c.Jev)
	}
	if st := td.JevState(ctx); st.On || st.HasKey || st.Masked != "" {
		t.Fatalf("card after Remove key: %+v", st)
	}
}

// Remove key can't forget a key exported to the twin: it switches off and
// says where the key still is, instead of "Removed".
func TestJevForgetWithAnExportedKey(t *testing.T) {
	td, srv := newJevDaemon(t)
	ctx := context.Background()
	if err := td.SaveJev(ctx, srv.Key(), true, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", srv.Key())
	var he *api.HumanError
	err := td.SaveJev(ctx, "", false, true)
	if !errors.As(err, &he) || !strings.Contains(he.Sentence, "off") || !strings.Contains(he.Fix, "TYPESAFE_API_KEY") ||
		strings.Contains(he.Sentence+he.Fix, srv.Key()) {
		t.Fatalf("forget with the key exported: %v", err)
	}
	if s, _ := config.ReadSecrets(); s["TYPESAFE_API_KEY"] != "" {
		t.Fatal("the saved copy wasn't forgotten")
	}
	if st := td.JevState(ctx); st.On || td.Config().Jev.Enabled {
		t.Fatalf("still on: %+v", st)
	}
}

// TypeSafe being down gives the card's friendly message, not the error.
func TestJevCardWhenTypeSafeIsDown(t *testing.T) {
	td, srv := newJevDaemon(t)
	srv.Fail(503, 1)
	var he *api.HumanError
	err := td.SaveJev(context.Background(), srv.Key(), true, false)
	if !errors.As(err, &he) || he.Sentence != "Couldn't reach TypeSafe just now." || !strings.Contains(he.Fix, "carries on as usual") {
		t.Fatalf("down: %v", err)
	}
	if s, _ := config.ReadSecrets(); s["TYPESAFE_API_KEY"] != "" {
		t.Fatal("a key that couldn't be checked was saved")
	}
}
