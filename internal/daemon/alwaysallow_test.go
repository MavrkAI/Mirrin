package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/agent"
	"github.com/MavrkAI/Mirrin/internal/approvals"
	"github.com/MavrkAI/Mirrin/internal/channels"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	"github.com/MavrkAI/Mirrin/internal/tools"
	"gopkg.in/yaml.v3"
)

// "Yes, always" from the chat: one line to check, then the request goes
// ahead and the twin stops asking about that tool, saying how to undo it.
func TestYesAlwaysStopsAskingAfterOneCheck(t *testing.T) {
	td := newTestDaemon(t, butler)
	ctx := context.Background()
	td.owner(t, "email the boss")
	got := td.owner(t, "Yes, always")
	if got != `Before I stop asking: from now on I'll go ahead with "send" without checking with you first. Reply "yes" to confirm, or "yes 1" for just this once.` {
		t.Fatalf("check %q", got)
	}
	if len(td.ran()) != 0 {
		t.Fatalf("ran before the check was answered: %v", td.ran())
	}
	got = td.owner(t, "yes")
	if !strings.HasPrefix(got, "Sent.") || !strings.Contains(got, `say "ask me first about send"`) || !slices.Equal(td.ran(), []string{"boss"}) {
		t.Fatalf("reply %q, sent %v", got, td.ran())
	}
	if allow := td.Config().Autonomy.AlwaysAllow; !slices.Contains(allow, "send") {
		t.Fatalf("always_allow %v", allow)
	}
	saved, err := config.Load()
	if err != nil || !slices.Contains(saved.Autonomy.AlwaysAllow, "send") {
		t.Fatalf("not saved to config.yaml: %v %v", saved.Autonomy.AlwaysAllow, err)
	}
	td.owner(t, "email the bank") // no longer asks
	if !slices.Equal(td.ran(), []string{"boss", "bank"}) {
		t.Fatalf("still asked: sent %v", td.ran())
	}
	if ps, _ := td.store.AllPendingApprovals(ctx); len(ps) != 0 {
		t.Fatalf("pending %+v", ps)
	}
	// And back again.
	if got := td.owner(t, "Ask me first about send."); got != `Done: I'll ask you before "send" again.` {
		t.Fatalf("undo %q", got)
	}
	td.owner(t, "email the landlord")
	if slices.Contains(td.ran(), "landlord") {
		t.Fatal("sent without asking after the owner took the always back")
	}
}

// Once the twin has said something else, a yes answers that, not the check.
func TestTheAlwaysCheckClosesWhenTheTwinMovesOn(t *testing.T) {
	td := newTestDaemon(t, butler)
	td.owner(t, "email the boss")
	td.owner(t, "yes, always")
	if err := td.Notify(context.Background(), ownerKey, "Reminder: call mum"); err != nil {
		t.Fatal(err)
	}
	got := td.owner(t, "yes")
	if !strings.HasPrefix(got, "Just to be sure") || len(td.Config().Autonomy.AlwaysAllow) != 0 || len(td.ran()) != 0 {
		t.Fatalf("reply %q, always %v, sent %v", got, td.Config().Autonomy.AlwaysAllow, td.ran())
	}
}

func TestNoToTheCheckKeepsAsking(t *testing.T) {
	td := newTestDaemon(t, butler)
	td.owner(t, "email the boss")
	td.owner(t, "yes always")
	if got := td.owner(t, "no"); got != `OK, I'll keep asking before "send". #1 is still waiting: reply "yes 1" to go ahead just this once.` {
		t.Fatalf("reply %q", got)
	}
	if len(td.ran()) != 0 || len(td.Config().Autonomy.AlwaysAllow) != 0 {
		t.Fatalf("sent %v, always %v", td.ran(), td.Config().Autonomy.AlwaysAllow)
	}
	if got := td.owner(t, "yes 1"); got != "Sent." {
		t.Fatalf("reply %q", got)
	}
}

// Some things always need asking: what can't easily be undone, what the
// owner listed under always_ask, and what changes the twin's own powers. A
// "yes, always" to them is a yes to this one, and the twin says why it will
// keep asking.
func TestAlwaysIsRefusedWhereItMustAsk(t *testing.T) {
	for _, c := range []struct {
		name, tool string
		risk       tools.Risk
		alwaysAsk  []string
		why        string
	}{
		{"can't be undone", "pay", tools.RiskDangerous, nil, "it can't easily be undone"},
		{"always_ask", "post", tools.RiskWrite, []string{"post"}, "always_ask"},
		{"its own powers", "create_protocol", tools.RiskWrite, nil, "changes what I can do on my own"},
		// Moving or turning on a routine is a standing order too.
		{"its routines", "update_protocol", tools.RiskWrite, nil, "changes what I can do on my own"},
		// A click on a signed-in site: allowed for good, no click would be
		// held to the page it was asked on again (browser merged with approvals).
		{"browser clicks", "browser_act", tools.RiskWrite, nil, "changes what I can do on my own"},
		// Nothing private is kept without a yes (site's promise, memory skill).
		{"private memories", "remember_sensitive", tools.RiskWrite, nil, "changes what I can do on my own"},
	} {
		t.Run(c.name, func(t *testing.T) {
			td := newTestDaemon(t, func(last string, req llm.Request) llm.Response {
				if last == "do it for me" {
					return call("x1", c.tool, `{}`)
				}
				return butler(last, req)
			})
			ran := 0
			td.agent.Tools().Register(tools.New(c.tool, c.tool, nil, c.risk, func(context.Context, tools.Call) (string, error) { ran++; return "done", nil }))
			if c.alwaysAsk != nil {
				cfg := td.Config()
				cfg.Autonomy.AlwaysAsk = c.alwaysAsk
				td.agent.SetPolicy(approvals.New(cfg.Autonomy))
				*td.cfg = cfg
			}
			td.owner(t, "do it for me")
			got := td.owner(t, "yes, and don't ask me again")
			if ran != 1 || !strings.Contains(got, c.why) || !strings.Contains(got, "I'll still check with you") {
				t.Fatalf("reply %q, ran %d", got, ran)
			}
			if allow := td.Config().Autonomy.AlwaysAllow; len(allow) != 0 {
				t.Fatalf("always_allow %v", allow)
			}
		})
	}
}

// "Yes always N" for a request that can't be always'd from here is taken
// as "yes N", with what that says.
func TestYesAlwaysNForSomethingElse(t *testing.T) {
	td := newTestDaemon(t, butler)
	if got := td.owner(t, "yes always 9"); !strings.HasPrefix(got, "I can't find approval #9.") {
		t.Fatalf("reply %q", got)
	}
	id := raise(t, td, "telegram:family", "send", `{"to":"plumber"}`)
	if got := td.owner(t, "yes, always "+strconv.FormatInt(id, 10)); !strings.Contains(got, "another conversation") || len(td.ran()) != 0 {
		t.Fatalf("reply %q, sent %v", got, td.ran())
	}
}

func TestParseAlways(t *testing.T) {
	for text, want := range map[string]int64{
		"yes, always":                  0,
		"Always":                       0,
		"always allow":                 0,
		"yes always 12":                12,
		"yes 12, and don't ask again":  12,
		"Yes from now on":              0,
		"ok, you don't need to ask me": -1, // "need to ask" isn't the phrase; not taken
		"no, never":                    -1,
		"always ask me first":          -1,
		"yes":                          -1,
	} {
		r, ok := parseAlways(text, nil)
		if (want >= 0) != ok || ok && r.ID != want {
			t.Errorf("parseAlways(%q) = %+v %v, want id %d", text, r, ok, want)
		}
	}
}

// "Yes, always" changes the twin for good, so it is taken only where it is
// surely the owner on their own: not on IRC or by email, whose sender anyone
// can claim to be, and not in a chat others talk in. The yes still goes
// for this one request.
func TestAlwaysOnlyFromTheOwnersOwnChat(t *testing.T) {
	for _, c := range []struct{ name, channel, chat, why string }{
		{"irc", "irc", "owner", "on IRC I can't be sure it's you"},
		{"email", "mail", "owner", "by email I can't be sure it's you"},
		{"a shared chat", "telegram", "family", "others talk in this chat"},
	} {
		t.Run(c.name, func(t *testing.T) {
			td := newTestDaemon(t, butler)
			if c.channel != "telegram" {
				td.channels[c.channel] = &fakeChannel{name: c.channel, owner: "owner", out: make(chan string, 8)}
			}
			say := func(text string) string {
				t.Helper()
				reply, err := td.message(context.Background(), channels.Inbound{Channel: c.channel, ChatID: c.chat, Sender: "owner", Text: text, IsOwner: true}, agent.Events{})
				if err != nil {
					t.Fatalf("%q: %v", text, err)
				}
				return reply
			}
			say("email the boss")
			if got := say("yes, always"); !strings.HasPrefix(got, "Sent.") || !strings.Contains(got, c.why) || !strings.Contains(got, `say "yes, always" from your own chat`) {
				t.Fatalf("reply %q", got)
			}
			say("yes")
			if allow := td.Config().Autonomy.AlwaysAllow; len(allow) != 0 {
				t.Fatalf("always_allow %v", allow)
			}
			if saved, err := config.Load(); err == nil && len(saved.Autonomy.AlwaysAllow) != 0 {
				t.Fatalf("saved always_allow %v", saved.Autonomy.AlwaysAllow)
			}
			if !slices.Equal(td.ran(), []string{"boss"}) {
				t.Fatalf("sent %v, want the boss once", td.ran())
			}
		})
	}
}

// "Yes, always" and "ask me first" change what the twin asks about and
// nothing else: no voice restart (here the microphone can't start), and the
// reply says what was actually saved.
func TestAlwaysChangesOnlyWhatIsAsked(t *testing.T) {
	td := newTestDaemon(t, butler)
	td.cmu.Lock()
	td.cfg.Channels.Voice.Enabled = true
	td.cfg.Channels.Voice.Mode = "wake"
	td.cfg.Channels.Voice.WhisperBin = filepath.Join(t.TempDir(), "no-whisper") // listening can't start
	td.cmu.Unlock()
	td.owner(t, "email the boss")
	td.owner(t, "yes, always")
	got := td.owner(t, "yes")
	if strings.Contains(got, "couldn't save") || !strings.Contains(got, "From now on I'll go ahead with \"send\"") {
		t.Fatalf("reply %q", got)
	}
	saved, err := config.Load()
	if err != nil || !slices.Equal(saved.Autonomy.AlwaysAllow, []string{"send"}) || !slices.Equal(td.Config().Autonomy.AlwaysAllow, []string{"send"}) {
		t.Fatalf("saved %v (%v), live %v", saved.Autonomy.AlwaysAllow, err, td.Config().Autonomy.AlwaysAllow)
	}
	if !saved.Channels.Voice.Enabled || td.Listening() {
		t.Fatalf("voice: saved enabled %v, listening %v", saved.Channels.Voice.Enabled, td.Listening())
	}
	if got := td.owner(t, "ask me first about send"); got != `Done: I'll ask you before "send" again.` {
		t.Fatalf("undo %q", got)
	}
	if saved, err := config.Load(); err != nil || len(saved.Autonomy.AlwaysAllow) != 0 || len(td.Config().Autonomy.AlwaysAllow) != 0 {
		t.Fatalf("after undo: saved %v (%v), live %v", saved.Autonomy.AlwaysAllow, err, td.Config().Autonomy.AlwaysAllow)
	}
}

// Regression (voice merged with approvals' "yes, always"): voice:local
// counts as the owner's own chat, so anyone in the room could say "yes,
// always" and "yes" and widen what the twin does unasked, for good. Out
// loud it is a yes to this one request only.
func TestSpokenYesAlwaysIsNotTakenForGood(t *testing.T) {
	td := newTestDaemon(t, butler)
	id := raise(t, td, voiceChat, "send", `{"to":"boss"}`)
	td.conv(voiceChat).noteReply(voiceChat, "Shall I send it?", true)
	got := td.spoken(t, "Yes, always")
	if !strings.Contains(got, "anyone nearby") || !strings.Contains(got, "I'll still check with you") {
		t.Fatalf("reply %q", got)
	}
	if ap, _ := td.store.GetApproval(context.Background(), id); ap.Status != "approved" {
		t.Fatalf("the yes itself: #%d %s", id, ap.Status)
	}
	td.spoken(t, "yes")
	if allow := td.Config().Autonomy.AlwaysAllow; len(allow) != 0 {
		t.Fatalf("a voice in the room set always_allow %v", allow)
	}
}

// "Yes, always" writes config.yaml from a chat; the layered save (time's)
// decides what the file keeps. The tool stays allowed through a later save
// from the tray and a restart, and through the upgrade of a full dump.
func TestYesAlwaysSurvivesLaterSavesAndUpgrades(t *testing.T) {
	td := newTestDaemon(t, butler)
	td.owner(t, "email the boss")
	td.owner(t, "yes, always")
	td.owner(t, "yes")
	if err := td.UpdateConfig(func(c *config.Config) { c.LLM.Effort = "high" }); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load()
	if err != nil || !slices.Contains(saved.Autonomy.AlwaysAllow, "send") || saved.LLM.Effort != "high" {
		t.Fatalf("after a tray save: always_allow %v, effort %q, %v", saved.Autonomy.AlwaysAllow, saved.LLM.Effort, err)
	}
	// An older release's full dump with the same list, brought up to date.
	legacy := *config.Default()
	legacy.Channels.WhatsApp.Enabled = false
	legacy.Autonomy.AlwaysAllow = []string{"send"}
	b, err := yaml.Marshal(&legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	up, err := config.Load()
	if err != nil || !slices.Contains(up.Autonomy.AlwaysAllow, "send") {
		t.Fatalf("after an upgrade: %v %v", up.Autonomy.AlwaysAllow, err)
	}
	if err := up.Save(); err != nil {
		t.Fatal(err)
	}
	if again, _ := config.Load(); !slices.Contains(again.Autonomy.AlwaysAllow, "send") {
		t.Fatalf("after saving the upgraded file: %v", again.Autonomy.AlwaysAllow)
	}
}
