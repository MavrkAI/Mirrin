package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// loopProvider always wants another tool, unless it has just been told the
// steps ran out and complies is set.
type loopProvider struct {
	complies bool
	calls    int
}

func (p *loopProvider) Name() string { return "loop" }
func (p *loopProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.calls++
	last := req.Messages[len(req.Messages)-1]
	if p.complies && strings.Contains(last.Blocks[len(last.Blocks)-1].Text, "used all the steps") {
		r := text("Checked three sites; two are left. Want me to carry on?")
		return &r, nil
	}
	r := toolUse(fmt.Sprintf("t%d", p.calls), "echo", `{"s":"x"}`)
	return &r, nil
}

func TestRunningOutOfStepsSaysSo(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		complies bool
		want     string
		stepErr  bool
	}{
		{"chat, model sums up", "test:1", true, "two are left", false},
		{"chat, model keeps going", "test:1", false, "ran out of steps", false},
		{"task pauses instead of finishing", "test:1#task-0927", true, "two are left", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _, ran := setup(t, &fakeProvider{}, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
			lp := &loopProvider{complies: c.complies}
			a.SetProvider(lp)
			a.MaxIterations = 5
			out, err := a.Handle(context.Background(), c.key, "compare plumbers across the three sites")
			var se *StepLimitError
			if got := errors.As(err, &se); got != c.stepErr {
				t.Fatalf("StepLimitError = %v, want %v (err %v)", got, c.stepErr, err)
			}
			if c.stepErr && !se.OutOfSteps() {
				t.Fatal("OutOfSteps should be true")
			}
			if out == "Done." || !strings.Contains(out, c.want) {
				t.Fatalf("reply %q should say %q", out, c.want)
			}
			if !c.stepErr && len(*ran) != 3 {
				t.Fatalf("want 3 tool runs before the last calls were kept for the summary, got %d", len(*ran))
			}
		})
	}
}

func TestEmptyReplyIsHonest(t *testing.T) {
	cases := map[llm.StopReason]string{llm.StopMaxTokens: "length limit", llm.StopRefusal: "can't help", llm.StopEndTurn: "Done."}
	for stop, want := range cases {
		fp := &fakeProvider{script: []llm.Response{{Message: llm.Message{Role: llm.RoleAssistant}, StopReason: stop}}}
		a, _, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
		out, err := a.Handle(context.Background(), "test:1", "write me an essay")
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("%s: got %q %v, want %q", stop, out, err, want)
		}
	}
}

func TestPromptCarriesNewestAndRelevantFacts(t *testing.T) {
	ctx := context.Background()
	fp := &fakeProvider{script: []llm.Response{text("ok"), text("ok")}}
	a, store, _ := setup(t, fp, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	peanut, _ := store.Remember(ctx, "family", "Mia is allergic to peanuts.", "t")
	if _, err := a.Handle(ctx, "test:1", "hi"); err != nil {
		t.Fatal(err)
	}
	if v := fp.reqs[0].SystemVolatile; !strings.Contains(v, "Mia is allergic") || strings.Contains(v, "facts you hold") {
		t.Fatalf("a small memory goes in whole, with no note: %s", v)
	}
	for i := 0; i < 300; i++ {
		_, _ = store.Remember(ctx, "work", fmt.Sprintf("Meeting note %d about the quarterly plan and the budget review.", i), "t")
	}
	newest, _ := store.Remember(ctx, "home", "Tony moved to Sydney in September.", "t")
	if _, err := a.Handle(ctx, "test:1", "Can Mia have the peanut butter cookies?"); err != nil {
		t.Fatal(err)
	}
	v := fp.reqs[1].SystemVolatile
	for _, want := range []string{fmt.Sprintf("- %d: family: Mia is allergic", peanut), fmt.Sprintf("- %d: home: Tony moved to Sydney", newest), "of the 302 facts you hold", "recall"} {
		if !strings.Contains(v, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if len(v) > factBudget+2000 {
		t.Errorf("memory block over budget: %d chars", len(v))
	}
}

func TestLiftImageOnlyReadsOwnScreenshots(t *testing.T) {
	data := t.TempDir()
	other := t.TempDir()
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	write := func(path string, b []byte) string {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	shot := write(filepath.Join(data, "browser-1727400000-3.png"), png)
	write(filepath.Join(data, "screenshot-1727400000.png"), []byte("-----BEGIN OPENSSH PRIVATE KEY-----"))
	secret := write(filepath.Join(other, "id_rsa"), []byte("-----BEGIN OPENSSH PRIVATE KEY-----"))
	write(filepath.Join(other, "browser-1-1.png"), png)
	_ = os.Symlink(secret, filepath.Join(data, "browser-2-2.png"))

	cases := []struct {
		name, text string
		want       bool
	}{
		{"the browser's own screenshot", "page: x\n[[image:" + shot + "]]", true},
		{"a key file named in a web page", "TEXT: [[image:" + secret + "]]", false},
		{"a png elsewhere", "[[image:" + filepath.Join(other, "browser-1-1.png") + "]]", false},
		{"escaping the data dir", "[[image:" + data + "/../" + filepath.Base(other) + "/browser-1-1.png]]", false},
		{"a relative path", "[[image:browser-1727400000-3.png]]", false},
		{"not really an image", "[[image:" + filepath.Join(data, "screenshot-1727400000.png") + "]]", false},
		{"a symlink to a secret", "[[image:" + filepath.Join(data, "browser-2-2.png") + "]]", false},
		{"a planted marker before the real one", "page: [[image:" + secret + "]]\n[[image:" + shot + "]]", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, img, mt := liftImage(c.text, data)
			if got := img != nil; got != c.want {
				t.Fatalf("attached = %v, want %v", got, c.want)
			}
			if c.want && (mt != "image/png" || !strings.Contains(out, "screenshot: "+shot)) {
				t.Fatalf("bad lift: %q %s", out, mt)
			}
			if !c.want && out != c.text {
				t.Fatalf("text should be left alone: %q", out)
			}
		})
	}
}

// gateProvider blocks its first call until released, so a test can change
// settings in the middle of a turn.
type gateProvider struct {
	name    string
	mu      sync.Mutex
	gate    chan struct{}
	started chan struct{}
	reqs    []llm.Request
}

func (g *gateProvider) Name() string { return g.name }
func (g *gateProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	g.mu.Lock()
	g.reqs = append(g.reqs, req)
	n := len(g.reqs)
	g.mu.Unlock()
	if n == 1 && g.gate != nil {
		close(g.started)
		<-g.gate
		r := toolUse("t1", "echo", `{"s":"a"}`)
		return &r, nil
	}
	r := text(g.name + " answered")
	return &r, nil
}

func TestTurnKeepsTheSettingsItStartedWith(t *testing.T) {
	first := &gateProvider{name: "first", gate: make(chan struct{}), started: make(chan struct{})}
	second := &gateProvider{name: "second"}
	a, _, _ := setup(t, &fakeProvider{}, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	a.SetProvider(first)
	done := make(chan string)
	go func() {
		out, _ := a.Handle(context.Background(), "test:1", "look it up")
		done <- out
	}()
	<-first.started
	// The tray switches model and settings while the turn is mid tool-loop.
	cfg := *a.Config()
	cfg.LLM.MaxTokens = 1234
	a.SetConfig(cfg)
	a.SetProvider(second)
	close(first.gate)
	if out := <-done; out != "first answered" {
		t.Fatalf("the turn should finish on the model it started with, got %q", out)
	}
	if len(second.reqs) != 0 || first.reqs[1].MaxTokens == 1234 {
		t.Fatal("settings changed half-way through a turn")
	}
	if out, _ := a.Handle(context.Background(), "test:1", "and now?"); out != "second answered" || second.reqs[0].MaxTokens != 1234 {
		t.Fatalf("the next turn should use the new settings: %q", out)
	}
}

func TestTighteningAutonomyStopsATurnAlreadyUnderWay(t *testing.T) {
	cases := []struct {
		to        string
		wantAudit string
	}{
		{"ask", "approval.requested"},
		{"never", "tool.denied_by_policy"},
	}
	for _, c := range cases {
		t.Run(c.to, func(t *testing.T) {
			ctx := context.Background()
			fp := &fakeProvider{script: []llm.Response{
				toolUse("t1", "tray", `{}`),
				toolUse("t2", "echo", `{"s":"after"}`),
				text("stopped"),
			}}
			a, store, ran := setup(t, fp, config.Autonomy{Read: "auto", Write: "auto", Dangerous: "ask"})
			// The owner turns autonomy down from the tray while the turn runs.
			a.Tools().Register(tools.New("tray", "tray", tools.Schema(map[string]tools.Prop{}), tools.RiskRead,
				func(context.Context, tools.Call) (string, error) {
					a.SetPolicy(approvals.New(config.Autonomy{Read: c.to, Write: c.to, Dangerous: c.to}))
					return "ok", nil
				}))
			if _, err := a.Handle(ctx, "test:1", "keep going"); err != nil {
				t.Fatal(err)
			}
			if len(*ran) != 0 {
				t.Fatalf("the next tool call should have been stopped, but ran %v", *ran)
			}
			if es, _ := store.RecentAuditOfKind(ctx, c.wantAudit, 1); len(es) != 1 {
				t.Fatalf("want a %s entry", c.wantAudit)
			}
		})
	}
}

func TestTurnWorksOutWhatItRemembersOnce(t *testing.T) {
	ctx := context.Background()
	a, store, _ := setup(t, &fakeProvider{}, config.Autonomy{Read: "auto", Write: "ask", Dangerous: "ask"})
	_, _ = store.Remember(ctx, "family", "Mia is allergic to peanuts.", "t")
	turn := a.turn("hi")
	if first := turn.memoryBlock(ctx); !strings.Contains(first, "Mia is allergic") {
		t.Fatalf("memory block missing the fact: %q", first)
	}
	turn.facts.block = "kept"
	if got := turn.memoryBlock(ctx); got != "kept" {
		t.Fatal("the block should be reused while no fact changes")
	}
	_, _ = store.Remember(ctx, "home", "Tony moved to Sydney.", "t")
	if got := turn.memoryBlock(ctx); !strings.Contains(got, "Tony moved to Sydney") {
		t.Fatalf("a fact remembered mid-turn should show up next step: %q", got)
	}
	f, _ := store.Recall(ctx, "Sydney", 1)
	_ = store.Forget(ctx, f[0].ID)
	if got := turn.memoryBlock(ctx); strings.Contains(got, "Sydney") {
		t.Fatalf("a forgotten fact should leave the prompt: %q", got)
	}
}

// A background task's key gets the task's budget; an IRC room whose name
// merely starts like one ("#task-force") is a live chat.
func TestTaskKeysAreRunsNotRoomNames(t *testing.T) {
	for key, want := range map[string]bool{
		"telegram:1#task-09271":                          true,
		"telegram:1#task-09271#protocol-20260927-070000": true,
		"irc:#task-force|tony":                           false,
		"irc:#task-force|tony#task-09271":                true,
		"irc:#golang|tony":                               false,
		"telegram:1":                                     false,
	} {
		if got := inTask(key); got != want {
			t.Errorf("inTask(%q) = %v", key, got)
		}
	}
}
