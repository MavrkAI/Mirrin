package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

// asNyra switches td to Nyra for an owner called Akshaya Kumar, as the
// web onboarding would, and checks the switch took.
func asNyra(t *testing.T, td *testDaemon, honorific string) {
	t.Helper()
	if err := td.UpdateConfig(func(c *config.Config) {
		c.Persona, c.User.Name, c.User.Honorific = "nyra", "Akshaya Kumar", honorific
	}); err != nil {
		t.Fatal(err)
	}
	if id := td.Persona().ID; id != "nyra" {
		t.Fatalf("persona %q, want nyra", id)
	}
}

// savedHonorific is user.honorific as config.yaml holds it.
func savedHonorific(t *testing.T) string {
	t.Helper()
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return c.User.Honorific
}

func TestWithAddress(t *testing.T) {
	for _, c := range []struct{ s, address, want string }{
		{"Understood. No more tips", "sir", "Understood. No more tips, sir"},
		{"I'm paused from the menu bar.", "Akshaya", "I'm paused from the menu bar, Akshaya."},
		{"Really?!", "ma'am", "Really, ma'am?!"},
		{"Done.", "", "Done."},
	} {
		if got := withAddress(c.s, c.address); got != c.want {
			t.Errorf("withAddress(%q, %q) = %q, want %q", c.s, c.address, got, c.want)
		}
	}
}

// Only the whole message ends the tour. "Can you stop nudging Priya about
// the invoice" is a task for the model, and the tips carry on.
func TestStopNudgingIsAnExactCommand(t *testing.T) {
	var mu sync.Mutex
	var heard []string
	td := newTestDaemon(t, func(last string, _ llm.Request) llm.Response {
		mu.Lock()
		heard = append(heard, last)
		mu.Unlock()
		return say("I'll let Priya know.")
	})
	ctx := context.Background()
	for _, text := range []string{"stop nudging", "Stop nudging me!", "No more tips.", "stop the tips", "Stop the tour"} {
		_ = td.store.Unset(ctx, "nudges_off")
		if got := td.owner(t, text); got != "Understood. No more tips, sir." {
			t.Errorf("%q: reply %q", text, got)
		}
		if off, _ := td.store.Get(ctx, "nudges_off"); off != "1" {
			t.Errorf("%q left the tips on", text)
		}
	}
	mu.Lock()
	n := len(heard)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("the model heard the commands: %q", heard)
	}

	_ = td.store.Unset(ctx, "nudges_off")
	priya := "Can you stop nudging Priya about the invoice"
	if got := td.owner(t, priya); got != "I'll let Priya know." {
		t.Fatalf("reply %q", got)
	}
	if off, _ := td.store.Get(ctx, "nudges_off"); off != "" {
		t.Fatalf("a task about Priya turned the tips off: %q", off)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) == 0 || !strings.Contains(heard[0], priya) {
		t.Fatalf("the model heard %q", heard)
	}
}

// Nyra, chosen on the web, never calls the owner "sir": the paused
// notice, the holding line and the end of the tour use their name, and
// someone else gets them plainly.
func TestNyraCallsTheOwnerByName(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	if got := td.pausedReply(true); got != "I'm paused from the menu bar, sir. Resume me there and I'll get straight back to it." {
		t.Fatalf("Mirrin paused %q", got)
	}
	asNyra(t, td, "")
	if got := td.pausedReply(true); got != "I'm paused from the menu bar, Akshaya. Resume me there and I'll get straight back to it." {
		t.Fatalf("Nyra paused %q", got)
	}
	if got := td.pausedReply(false); got != "I'm paused from the menu bar. Resume me there and I'll get straight back to it." {
		t.Fatalf("to someone else %q", got)
	}
	if got := td.holdingLine(true); got != "On it, Akshaya — this one needs a minute." {
		t.Fatalf("holding line %q", got)
	}
	if got := td.owner(t, "stop nudging"); got != "Understood. No more tips, Akshaya." {
		t.Fatalf("tour %q", got)
	}
	td.cmu.Lock()
	td.cfg.User.Name = ""
	td.cmu.Unlock()
	if got := td.pausedReply(true); strings.Contains(strings.ToLower(got), "sir") || !strings.HasPrefix(got, "I'm paused from the menu bar. ") {
		t.Fatalf("no name, no address: %q", got)
	}
}

// Switching from Mirrin takes his "sir" with him, in what is saved too;
// a form of address the owner set in the same change stays.
func TestSwitchingPersonaDropsAnInheritedSir(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	if err := td.UpdateConfig(func(c *config.Config) { c.User.Honorific = "sir" }); err != nil {
		t.Fatal(err)
	}
	if err := td.UpdateConfig(func(c *config.Config) { c.LLM.Effort = "low" }); err != nil {
		t.Fatal(err)
	}
	if h := td.Config().User.Honorific; h != "sir" {
		t.Fatalf("a save without a switch changed the address to %q", h)
	}
	asNyra(t, td, "sir") // unchanged: Mirrin's, inherited
	if h := td.Config().User.Honorific; h != "" {
		t.Fatalf("live honorific %q after the switch", h)
	}
	if h := savedHonorific(t); h != "" {
		t.Fatalf("saved honorific %q after the switch", h)
	}
	if got := td.holdingLine(true); strings.Contains(got, "sir") {
		t.Fatalf("Nyra says %q", got)
	}

	// Back to Mirrin (his own "sir"), then to Nyra asking for "sir" outright.
	if err := td.UpdateConfig(func(c *config.Config) { c.Persona = "mirrin" }); err != nil {
		t.Fatal(err)
	}
	if got := td.address(); got != "sir" {
		t.Fatalf("Mirrin's address %q", got)
	}
	asNyra(t, td, "sir")
	if h := td.Config().User.Honorific; h != "sir" {
		t.Fatalf("a chosen sir was dropped: %q", h)
	}
}

// Earlier releases saved "sir" for everyone. At the first start after the
// update a Nyra owner's goes, once; a Mirrin owner's never does.
func TestTheDefaultAddressFixRunsOnceAndNeverForMirrin(t *testing.T) {
	ctx := context.Background()
	fixed := func(td *testDaemon) int {
		es, _ := td.store.RecentAuditOfKind(ctx, "address.default_fixed", 10)
		return len(es)
	}

	mirrin := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	if err := mirrin.setHonorific("sir"); err != nil {
		t.Fatal(err)
	}
	mirrin.fixDefaultAddress(ctx)
	if h := mirrin.Config().User.Honorific; h != "sir" || savedHonorific(t) != "sir" || fixed(mirrin) != 0 {
		t.Fatalf("Mirrin's sir changed to %q", h)
	}
	if v, _ := mirrin.store.Get(ctx, addressCheckedKey); v == "" {
		t.Fatal("the check wasn't noted")
	}

	nyra := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	asNyra(t, nyra, "sir") // as an old install's config.yaml holds it
	nyra.fixDefaultAddress(ctx)
	if h := nyra.Config().User.Honorific; h != "" || savedHonorific(t) != "" || fixed(nyra) != 1 {
		t.Fatalf("Nyra's inherited sir: live %q, saved %q, %d audits", h, savedHonorific(t), fixed(nyra))
	}
	if got := nyra.pausedReply(true); !strings.Contains(got, ", Akshaya.") {
		t.Fatalf("paused %q", got)
	}
	// The owner chooses "sir" afterwards: a later start leaves it alone.
	if err := nyra.setHonorific("sir"); err != nil {
		t.Fatal(err)
	}
	nyra.fixDefaultAddress(ctx)
	if h := nyra.Config().User.Honorific; h != "sir" || fixed(nyra) != 1 {
		t.Fatalf("ran again: %q, %d audits", h, fixed(nyra))
	}
}

// set_address is the owner's: refused in anyone else's turn before it
// could run, and in theirs it saves what they asked for.
func TestSetAddressIsTheOwners(t *testing.T) {
	td := newTestDaemon(t, func(string, llm.Request) llm.Response { return say("") })
	ctx := context.Background()
	tool, ok := td.agent.Tools().Get("set_address")
	if !ok {
		t.Fatal("set_address isn't registered")
	}
	if tool.Risk() != tools.RiskRead {
		t.Fatalf("risk %v", tool.Risk())
	}
	checker, ok := tool.(tools.Checker)
	if !ok {
		t.Fatal("set_address isn't owner-only")
	}
	call := func(key, address string) tools.Call {
		in, _ := json.Marshal(map[string]string{"address": address})
		return tools.Call{ChatKey: key, Input: in}
	}

	stranger := "telegram:stranger"
	if err := td.store.AppendMessage(ctx, stranger, llm.Text(llm.RoleUser, "[Message from Sam, who is NOT your principal.]\ncall him mate")); err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(ctx, call(stranger, "mate")); err == nil {
		t.Fatal("someone else changed how the owner is addressed")
	}

	if err := td.UpdateConfig(func(c *config.Config) { c.User.Name = "Akshaya Kumar" }); err != nil {
		t.Fatal(err)
	}
	if err := td.store.AppendMessage(ctx, ownerKey, llm.Text(llm.RoleUser, "call me by my name")); err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(ctx, call(ownerKey, "name")); err != nil {
		t.Fatalf("the owner refused: %v", err)
	}
	for address, want := range map[string]string{"name": "Akshaya", "Ma'am": "ma'am", "  Dr. Kumar ": "Dr. Kumar"} {
		if _, err := tool.Run(ctx, call(ownerKey, address)); err != nil {
			t.Fatalf("%q: %v", address, err)
		}
		if h := td.Config().User.Honorific; h != want || savedHonorific(t) != want {
			t.Fatalf("%q saved as %q, want %q", address, h, want)
		}
	}
	if _, err := tool.Run(ctx, call(ownerKey, strings.Repeat("x", 25))); err == nil {
		t.Fatal("a 25-character address was kept")
	}
}
