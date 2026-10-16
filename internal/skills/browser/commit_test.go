package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/tools"
)

// Ordinary browsing runs unasked: searching, a cookie banner, Next and a
// link to the terms are reads. The last button of a web chore always
// asks: pressing Send or Submit order, sending its form from a field, or
// pressing Enter in one, is dangerous, and so is agreeing: a tick box for
// the terms, Create account, or a bare Accept. Typing personal details (a
// name, an email, a phone number) is a write, so it asks under the
// default autonomy; typing a search term is a read.
func TestTheLastButtonAlwaysAsks(t *testing.T) {
	for _, l := range []string{"Submit order", "Send message ›", "Book this table", "Send us a message", "Submit your application today", "Book a table now", "Book your free consultation", "Post", "Post comment", "Reply", "Publish", "Apply now", "Order now", "Confirm", "Delete account", "Delete", "Cancel subscription", "Unsubscribe", "RSVP", "Yes, delete it", "Yes, send", "Submit order\n$49.00"} {
		if !reCommit.MatchString(l) {
			t.Errorf("%q isn't seen as a send button", l)
		}
	}
	for _, l := range []string{"Search", "Next", "Add to cart", "Accept all cookies", "Cancel", "Apply", "Apply filters", "Order history", "Sending tips and tricks for your next trip", "Submit your application today and join the thousands of happy customers who already did"} {
		if reCommit.MatchString(l) {
			t.Errorf("%q looks like a send button", l)
		}
	}

	needChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(agreePage))
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
		`[{"type":"type","selector":"#email","text":"sam@x.test"}]`:      tools.RiskWrite,
		`[{"type":"type","selector":"#phone","text":"0400 000 000"}]`:    tools.RiskWrite,
		`[{"type":"click","selector":"#find"}]`:                          tools.RiskRead,
		`[{"type":"submit","selector":"#q"}]`:                            tools.RiskRead,
		`[{"type":"type","selector":"#q","text":"trains","enter":true}]`: tools.RiskRead,
		`[{"type":"type","selector":"#q","text":"trains"}]`:              tools.RiskRead,
		`[{"type":"click","selector":"#cookies"}]`:                       tools.RiskRead,
		`[{"type":"click","selector":"#next"}]`:                          tools.RiskRead,
		`[{"type":"scroll","direction":"down"}]`:                         tools.RiskRead,
		`[{"type":"click","selector":"#termslink"}]`:                     tools.RiskRead,
		`[{"type":"click","selector":"#terms"}]`:                         tools.RiskDangerous,
		`[{"type":"click","selector":"#termsword"}]`:                     tools.RiskDangerous,
		`[{"type":"click","selector":"#create"}]`:                        tools.RiskDangerous,
		`[{"type":"click","selector":"#accept"}]`:                        tools.RiskDangerous,
		// A cookie dialog's Agree or Accept all is an ordinary click even
		// when the button itself doesn't say cookies.
		`[{"type":"click","selector":"#agreeall"}]`:  tools.RiskRead,
		`[{"type":"click","selector":"#acceptall"}]`: tools.RiskRead,
		// Enter in a chat or comment composer sends, whether or not there
		// is a form; Enter in a search box searches.
		`[{"type":"type","selector":"#chat","text":"hi","enter":true}]`:                            tools.RiskDangerous,
		`[{"type":"type","selector":"#chat","text":"hi"}]`:                                         tools.RiskRead,
		`[{"type":"type","selector":"#chatline","text":"hi"},{"type":"press","key":"Enter"}]`:      tools.RiskDangerous,
		`[{"type":"type","selector":"#sitesearch","text":"flats"},{"type":"press","key":"Enter"}]`: tools.RiskRead,
		// Agreeing on the controls sites really use: a div, a link that
		// goes nowhere, a role=checkbox, a tick box whose words sit beside it.
		`[{"type":"click","selector":"#agreediv"}]`:   tools.RiskDangerous,
		`[{"type":"click","selector":"#createlink"}]`: tools.RiskDangerous,
		`[{"type":"click","selector":"#rolebox"}]`:    tools.RiskDangerous,
		`[{"type":"click","selector":"#bare"}]`:       tools.RiskDangerous,
		// Sending in more words than Send: posting, booking, cancelling a
		// subscription. A dialog's Cancel, Apply filters and a link to
		// order history are ordinary.
		`[{"type":"click","selector":"#twoline"}]`:                tools.RiskDangerous,
		`[{"type":"type","selector":"#addr","text":"1 High St"}]`: tools.RiskWrite,
		`[{"type":"type","selector":"#to","text":"sam@x.test"}]`:  tools.RiskWrite,
		`[{"type":"click","selector":"#postbtn"}]`:                tools.RiskDangerous,
		`[{"type":"click","selector":"#bookbtn"}]`:                tools.RiskDangerous,
		`[{"type":"click","selector":"#cancelsub"}]`:              tools.RiskDangerous,
		`[{"type":"click","selector":"#cancel"}]`:                 tools.RiskRead,
		`[{"type":"click","selector":"#applyfilters"}]`:           tools.RiskRead,
		`[{"type":"click","selector":"#orderhist"}]`:              tools.RiskRead,
		// Personal details by the spellings forms use; a search box for
		// emails isn't one; a date of birth chosen from a list is.
		`[{"type":"type","selector":"#first_name","text":"Sam"}]`:             tools.RiskWrite,
		`[{"type":"type","selector":"#fn","text":"Sam"}]`:                     tools.RiskWrite,
		`[{"type":"type","selector":"#pc","text":"3000"}]`:                    tools.RiskWrite,
		`[{"type":"type","selector":"#cc","text":"4111"}]`:                    tools.RiskWrite,
		`[{"type":"type","selector":"#bday","text":"1990"}]`:                  tools.RiskWrite,
		`[{"type":"type","selector":"#mailsearch","text":"invoice"}]`:         tools.RiskRead,
		`[{"type":"select","selector":"#dob_month","value":"1"}]`:             tools.RiskWrite,
		`[{"type":"select","selector":"select[name=reason]","value":"late"}]`: tools.RiskRead,
		// Something that isn't on the page yet can't be read, so a press
		// on it asks and typing into it is a write.
		`[{"type":"click","selector":"#nope"}]`:             tools.RiskDangerous,
		`[{"type":"type","selector":"#nope","text":"Sam"}]`: tools.RiskWrite,
		// Enter after typing goes to the field just typed in, not to
		// whatever has the focus before the steps run.
		`[{"type":"type","selector":"#name","text":"Sam"},{"type":"press","key":"Enter"}]`: tools.RiskDangerous,
		`[{"type":"type","selector":"#q","text":"trains"},{"type":"press","key":"Enter"}]`: tools.RiskRead,
	} {
		if got := risk(steps); got != want {
			t.Errorf("%s: %v, want %v", steps, got, want)
		}
	}

	// A call that opens another page first can't be read in advance, so
	// any press there asks, and typing there is a write; on the page
	// already open, it's read as usual.
	riskAt := func(url, steps string) tools.Risk {
		in, _ := json.Marshal(map[string]string{"url": url, "steps": steps})
		return cr.RiskFor(ctx, tools.Call{Input: in})
	}
	// When the page can't be read at all (the script fails), a press
	// asks and typing is a write, the same as a page not open yet.
	func() {
		old := evalRisk
		defer func() { evalRisk = old }()
		evalRisk = func(context.Context, string, any) error { return errors.New("page busy") }
		for steps, want := range map[string]tools.Risk{
			`[{"type":"click","selector":"#next"}]`:             tools.RiskDangerous,
			`[{"type":"type","selector":"#q","text":"trains"}]`: tools.RiskWrite,
			`[{"type":"scroll","direction":"down"}]`:            tools.RiskRead,
		} {
			if got := risk(steps); got != want {
				t.Errorf("unreadable page, %s: %v, want %v", steps, got, want)
			}
		}
	}()
	for _, c := range []struct {
		url, steps string
		want       tools.Risk
	}{
		{srv.URL + "/contact", `[{"type":"click","selector":"#send"}]`, tools.RiskDangerous},
		{srv.URL + "/contact", `[{"type":"click","selector":"#next"}]`, tools.RiskDangerous},
		{srv.URL + "/contact", `[{"type":"type","selector":"#name","text":"Sam"},{"type":"press","key":"Enter"}]`, tools.RiskDangerous},
		{srv.URL + "/contact", `[{"type":"type","selector":"#name","text":"Sam"}]`, tools.RiskWrite},
		{srv.URL + "/contact", `[{"type":"type","selector":"#q","text":"trains"}]`, tools.RiskWrite},
		{srv.URL + "/contact", `[{"type":"scroll","direction":"down"}]`, tools.RiskRead},
		{srv.URL + "/contact", ``, tools.RiskRead},
		{srv.URL + "/", `[{"type":"click","selector":"#next"}]`, tools.RiskRead},
		{srv.URL + "/", `[{"type":"click","selector":"#send"}]`, tools.RiskDangerous},
	} {
		if got := riskAt(c.url, c.steps); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.url, c.steps, got, c.want)
		}
	}
}

// agreePage is a page with a search, a cookie banner, a contact form, a
// sign-up and a link to the terms.
func agreePage(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte(`<html><body>
<form action="/search"><input id="q" name="q"><button id="find">Search</button></form>
<button id="cookies">Accept all cookies</button><button id="next">Next</button>
<form action="/sent" method="post"><input id="name" name="name"><input id="email" type="email"><input id="phone" autocomplete="tel"><select name="reason"><option value="late">late</option></select><textarea id="msg" name="msg"></textarea><button id="send" type="submit"><span id="sendword">Send message</span></button></form>
<form action="/join" method="post"><label><input id="terms" type="checkbox"> <span id="termsword">I agree to the Terms and Conditions</span></label><button id="create" type="button">Create account</button></form>
<div role="dialog"><p>We've updated our terms.</p><button id="accept">Accept</button></div>
<a id="termslink" href="/terms">Terms and conditions</a>
<div id="cmp" role="dialog"><p>We use cookies to improve your experience.</p><button id="agreeall">AGREE</button><button id="acceptall">Accept all</button></div>
<div id="chat" contenteditable="true" data-placeholder="Type a message"></div>
<input id="chatline" placeholder="Message #general"><input id="sitesearch" type="search" placeholder="Search">
<div id="agreediv" onclick="">I agree</div><a id="createlink" href="#">Create account</a>
<div id="rolebox" role="checkbox" aria-checked="false" aria-label="I agree to the terms"></div>
<div><input id="bare" type="checkbox"><span>I agree to the privacy policy</span></div>
<button id="postbtn">Post comment</button><button id="bookbtn">Book a table now</button><button id="cancelsub">Cancel subscription</button>
<button id="cancel">Cancel</button><button id="applyfilters">Apply filters</button><a id="orderhist" href="/orders">Order history</a>
<button id="twoline"><div>Submit order</div><div>$49.00</div></button>
<form action="/join2"><input id="addr" role="combobox" autocomplete="street-address" placeholder="Enter your address"><input id="to" role="combobox" name="email" aria-label="To"><input id="first_name"><input id="fn" autocomplete="given-name"><input id="pc" autocomplete="postal-code"><input id="cc" autocomplete="cc-number"><input id="bday" name="bday"><input id="mailsearch" placeholder="Search emails"><select id="dob_month" name="dob_month"><option>1</option></select></form>
</body></html>`))
}

// With ask_every_click, the old way: every click and every typed word is
// a write, so the twin asks before each; the floor stays dangerous.
func TestAskEveryClickKeepsAsking(t *testing.T) {
	needChrome(t)
	srv := httptest.NewServer(http.HandlerFunc(agreePage))
	defer srv.Close()
	s := newTestSession(t, "127.0.0.1")
	s.cfg.AskEveryClick = true
	reg := tools.NewRegistry()
	reg.Register(s.Tools()...)
	ctx := context.Background()
	if _, err := reg.Run(ctx, "browser_inspect", tools.Call{Input: json.RawMessage(`{"url":"` + srv.URL + `"}`)}); err != nil {
		t.Fatal(err)
	}
	act, _ := reg.Get("browser_act")
	if act.Risk() != tools.RiskWrite {
		t.Fatalf("browser_act declares %v", act.Risk())
	}
	cr := act.(tools.CallRisker)
	for steps, want := range map[string]tools.Risk{
		`[{"type":"click","selector":"#next"}]`:             tools.RiskWrite,
		`[{"type":"type","selector":"#q","text":"trains"}]`: tools.RiskWrite,
		`[{"type":"scroll","direction":"down"}]`:            tools.RiskWrite,
		`[{"type":"click","selector":"#send"}]`:             tools.RiskDangerous,
		`[{"type":"click","selector":"#terms"}]`:            tools.RiskDangerous,
	} {
		in, _ := json.Marshal(map[string]string{"steps": steps})
		if got := cr.RiskFor(ctx, tools.Call{Input: in}); got != want {
			t.Errorf("%s: %v, want %v", steps, got, want)
		}
	}
}

// The words that agree to something: on a button, a tick box's label or
// a goal. A cookie banner's choices, a link to read the terms, signing in
// and ordinary words don't.
func TestTheAgreeWords(t *testing.T) {
	for _, w := range []string{"I agree to the Terms and Conditions", "Accept", "Agree", "I accept", "I consent", "Accept terms", "Accept the terms and continue", "Agree and continue", "Create account", "Create an account", "Create my account", "Create free account", "Sign up", "Sign Up free", "Register", "Register now", "Join now", "Yes, I agree", "Complete registration", "We use cookies. I agree to the Terms"} {
		if !agrees(w) {
			t.Errorf("%q isn't seen as agreeing", w)
		}
	}
	for _, w := range []string{"Accept all cookies", "Accept cookies", "cookies AGREE", "cookies Accept all", "Reject all", "Terms and conditions", "Privacy policy", "Search", "Next", "Sign in", "Log in", "Registered office: 1 High St", "Agreement", "Acceptance rate 4%", "Join the waitlist for updates"} {
		if agrees(w) {
			t.Errorf("%q is seen as agreeing", w)
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
	if len(ps) != 3 || ps[0].Sel != "#name" || !ps[0].Field || ps[0].Commit || ps[1].Sel != "#topic" || !ps[1].Field || ps[2].Sel != "#topic" || !ps[2].ViaForm || !ps[2].Commit {
		t.Fatalf("Enter after a select: %+v", ps)
	}
	steps, _ = parseSteps(`[{"type":"press","key":"Enter"}]`)
	if ps := stepProbes(steps); len(ps) != 1 || ps[0].Sel != "" {
		t.Fatalf("Enter on its own: %+v", ps)
	}
	steps, _ = parseSteps(`[{"type":"type","selector":"#name","text":"Sam"},{"type":"press","key":"Tab"}]`)
	if ps := stepProbes(steps); len(ps) != 1 || !ps[0].Field || ps[0].Commit {
		t.Fatalf("Tab sends nothing, the typing is a field: %+v", ps)
	}
	// With no tab open, steps for a new page that press anything ask,
	// filling in there is a write, and scrolling is ordinary browsing.
	click, _ := parseSteps(`[{"type":"click","selector":"#send"}]`)
	fill, _ := parseSteps(`[{"type":"type","selector":"#name","text":"Sam"}]`)
	scroll, _ := parseSteps(`[{"type":"scroll","direction":"down"}]`)
	var noTab context.Context
	for _, c := range []struct {
		url   string
		steps []actStep
		want  tools.Risk
	}{
		{"https://example.test/contact", click, tools.RiskDangerous},
		{"https://example.test/contact", fill, tools.RiskWrite},
		{"https://example.test/contact", scroll, tools.RiskRead},
		{"", click, tools.RiskDangerous},
		{"", scroll, tools.RiskRead},
	} {
		if got := stepsRisk(noTab, c.url, c.steps, tools.RiskRead); got != c.want {
			t.Errorf("no open tab, %q %+v: %v, want %v", c.url, c.steps, got, c.want)
		}
	}
	if got := stepsRisk(noTab, "https://example.test/contact", scroll, tools.RiskWrite); got != tools.RiskWrite {
		t.Errorf("ask_every_click, scrolling a new page: %v", got)
	}
}
