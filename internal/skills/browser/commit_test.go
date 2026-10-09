package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// The last button of a web chore always asks: pressing Send or Submit
// order, sending its form from a field, or pressing Enter in one, is
// dangerous; searching, a cookie banner and Next stay ordinary writes.
func TestTheLastButtonAlwaysAsks(t *testing.T) {
	if !reCommit.MatchString("Submit order") || !reCommit.MatchString("Send message ›") || !reCommit.MatchString("Book this table") {
		t.Fatal("the send words aren't seen")
	}
	for _, l := range []string{"Search", "Next", "Accept all cookies", "Sending tips and tricks for your next trip"} {
		if reCommit.MatchString(l) {
			t.Fatalf("%q looks like a send button", l)
		}
	}

	needChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body>
<form action="/search"><input id="q" name="q"><button id="find">Search</button></form>
<button id="cookies">Accept all cookies</button><button id="next">Next</button>
<form action="/sent" method="post"><input id="name" name="name"><textarea id="msg" name="msg"></textarea><button id="send" type="submit"><span id="sendword">Send message</span></button></form>
</body></html>`))
	}))
	defer srv.Close()
	s := newTestSession(t, "127.0.0.1")
	reg := tools.NewRegistry()
	reg.Register(s.Tools()...)
	ctx := context.Background()
	if _, err := reg.Run(ctx, "browser_inspect", tools.Call{Input: json.RawMessage(`{"url":"` + srv.URL + `"}`)}); err != nil {
		t.Fatal(err)
	}
	act, _ := reg.Get("browser_act")
	cr := act.(tools.CallRisker)
	risk := func(steps string) tools.Risk {
		in, _ := json.Marshal(map[string]string{"steps": steps})
		return cr.RiskFor(ctx, tools.Call{Input: in})
	}
	for steps, want := range map[string]tools.Risk{
		`[{"type":"click","selector":"#send"}]`:                          tools.RiskDangerous,
		`[{"type":"click","selector":"#sendword"}]`:                      tools.RiskDangerous,
		`[{"type":"submit","selector":"#name"}]`:                         tools.RiskDangerous,
		`[{"type":"type","selector":"#name","text":"Sam","enter":true}]`: tools.RiskDangerous,
		`[{"type":"type","selector":"#name","text":"Sam"}]`:              tools.RiskWrite,
		`[{"type":"click","selector":"#find"}]`:                          tools.RiskWrite,
		`[{"type":"submit","selector":"#q"}]`:                            tools.RiskWrite,
		`[{"type":"type","selector":"#q","text":"trains","enter":true}]`: tools.RiskWrite,
		`[{"type":"click","selector":"#cookies"}]`:                       tools.RiskWrite,
		`[{"type":"click","selector":"#next"}]`:                          tools.RiskWrite,
		// Enter after typing goes to the field just typed in, not to
		// whatever has the focus before the steps run.
		`[{"type":"type","selector":"#name","text":"Sam"},{"type":"press","key":"Enter"}]`: tools.RiskDangerous,
		`[{"type":"type","selector":"#q","text":"trains"},{"type":"press","key":"Enter"}]`: tools.RiskWrite,
	} {
		if got := risk(steps); got != want {
			t.Errorf("%s: %v, want %v", steps, got, want)
		}
	}

	// A call that opens another page first can't be read in advance, so
	// any press there asks; on the page already open, it's read as usual,
	// and filling in alone stays a write.
	riskAt := func(url, steps string) tools.Risk {
		in, _ := json.Marshal(map[string]string{"url": url, "steps": steps})
		return cr.RiskFor(ctx, tools.Call{Input: in})
	}
	for _, c := range []struct {
		url, steps string
		want       tools.Risk
	}{
		{srv.URL + "/contact", `[{"type":"click","selector":"#send"}]`, tools.RiskDangerous},
		{srv.URL + "/contact", `[{"type":"type","selector":"#name","text":"Sam"},{"type":"press","key":"Enter"}]`, tools.RiskDangerous},
		{srv.URL + "/contact", `[{"type":"type","selector":"#name","text":"Sam"}]`, tools.RiskWrite},
		{srv.URL + "/", `[{"type":"click","selector":"#next"}]`, tools.RiskWrite},
		{srv.URL + "/", `[{"type":"click","selector":"#send"}]`, tools.RiskDangerous},
	} {
		if got := riskAt(c.url, c.steps); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.url, c.steps, got, c.want)
		}
	}
}

// Enter is read against the field the steps last worked in; with none, the
// field that has the focus now.
func TestEnterGoesToTheLastField(t *testing.T) {
	steps, err := parseSteps(`[{"type":"type","selector":"#name","text":"Sam"},{"type":"select","selector":"#topic","value":"a"},{"type":"press","key":"Enter"}]`)
	if err != nil {
		t.Fatal(err)
	}
	ps := stepProbes(steps)
	if len(ps) != 1 || ps[0].Sel != "#topic" || !ps[0].ViaForm || !ps[0].Commit {
		t.Fatalf("Enter after a select: %+v", ps)
	}
	steps, _ = parseSteps(`[{"type":"press","key":"Enter"}]`)
	if ps := stepProbes(steps); len(ps) != 1 || ps[0].Sel != "" {
		t.Fatalf("Enter on its own: %+v", ps)
	}
	steps, _ = parseSteps(`[{"type":"type","selector":"#name","text":"Sam"},{"type":"press","key":"Tab"}]`)
	if ps := stepProbes(steps); len(ps) != 0 {
		t.Fatalf("Tab sends nothing: %+v", ps)
	}
	// With no tab open, steps for a new page that press anything ask, and
	// filling in alone doesn't.
	click, _ := parseSteps(`[{"type":"click","selector":"#send"}]`)
	fill, _ := parseSteps(`[{"type":"type","selector":"#name","text":"Sam"}]`)
	var noTab context.Context
	if !stepsCommit(noTab, "https://example.test/contact", click) || stepsCommit(noTab, "https://example.test/contact", fill) || stepsCommit(noTab, "", click) {
		t.Fatal("no open tab: a press on a new page should ask, filling in shouldn't")
	}
}
