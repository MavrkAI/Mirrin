package phone

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/tools"
)

func TestCallAndTwoWayLoop(t *testing.T) {
	var got url.Values
	tw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		u, p, _ := r.BasicAuth()
		if u != "AC123" || p != "tok" {
			t.Errorf("auth %s %s", u, p)
		}
		_, _ = io.WriteString(w, `{"sid":"CA1"}`)
	}))
	defer tw.Close()
	cfg := config.Phone{AccountSID: "AC123", AuthToken: "tok", From: "+61111", PublicURL: "https://twin.example", Voice: "Polly.Olivia-Neural"}
	c := New(func() config.Phone { return cfg })
	c.base = tw.URL
	c.Turn = func(_ context.Context, id, purpose, heard string) (string, error) {
		if strings.Contains(strings.ToLower(heard), "seven") {
			return "Seven is perfect. Thank you, goodbye. [HANGUP]", nil
		}
		return "Do you have anything at seven?", nil
	}
	sid, err := c.Call(context.Background(), "+61222", "Hi, calling for Akshay to book a table.", "book a table for two Friday")
	if err != nil || sid != "CA1" || !strings.Contains(got.Get("Url"), "/phone/turn?call=") {
		t.Fatalf("call: %v %s %v", err, sid, got)
	}
	id := strings.TrimPrefix(got.Get("Url"), "https://twin.example/phone/turn?call=")

	post := func(path string, form url.Values) string {
		full := "https://twin.example" + path
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Twilio-Signature", sign("tok", full, form))
		rec := httptest.NewRecorder()
		c.Webhook(rec, req)
		return rec.Body.String()
	}
	// Answered: opening line, then listen.
	out := post("/phone/turn?call="+id, url.Values{"CallStatus": {"in-progress"}})
	if !strings.Contains(out, "calling for Akshay") || !strings.Contains(out, "<Gather") {
		t.Fatalf("opening: %s", out)
	}
	out = post("/phone/turn?call="+id, url.Values{"SpeechResult": {"We're full at six"}})
	if !strings.Contains(out, "anything at seven") || !strings.Contains(out, "<Gather") {
		t.Fatalf("turn: %s", out)
	}
	out = post("/phone/turn?call="+id, url.Values{"SpeechResult": {"Seven is fine"}})
	if !strings.Contains(out, "Seven is perfect") || strings.Contains(out, "<Gather") || !strings.Contains(out, "<Hangup/>") {
		t.Fatalf("hangup: %s", out)
	}
	if tr := c.Transcript(id); len(tr) != 5 || !strings.HasPrefix(tr[1], "them: We're full") {
		t.Fatalf("transcript: %v", tr)
	}
	// A forged request is refused.
	req := httptest.NewRequest(http.MethodPost, "/phone/turn?call="+id, strings.NewReader("SpeechResult=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", "nope")
	rec := httptest.NewRecorder()
	c.Webhook(rec, req)
	if rec.Code != 403 {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	// One-way when no public URL.
	cfg.PublicURL = ""
	if _, err := c.Call(context.Background(), "+61222", "Just a message.", ""); err != nil || !strings.Contains(got.Get("Twiml"), "Just a message.") || got.Get("Url") != "" {
		t.Fatalf("one-way: %v %v", err, got)
	}
}

// twoWay sets up a client whose calls are two-way, with a fake Twilio that
// answers every placed call with sid.
func twoWay(t *testing.T, sid string) (*Client, func(path string, form url.Values) string) {
	t.Helper()
	tw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"sid":"`+sid+`"}`)
	}))
	t.Cleanup(tw.Close)
	cfg := config.Phone{AccountSID: "AC123", AuthToken: "tok", From: "+61111", PublicURL: "https://twin.example"}
	c := New(func() config.Phone { return cfg })
	c.base = tw.URL
	post := func(path string, form url.Values) string {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Twilio-Signature", sign("tok", "https://twin.example"+path, form))
		rec := httptest.NewRecorder()
		c.Webhook(rec, req)
		return rec.Body.String()
	}
	return c, post
}

func callID(c *Client) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.calls {
		return id
	}
	return ""
}

// The model is given Twilio's SID, so that is what it asks about, and the
// answer must still be there once the call is over.
func TestCallTranscriptBySIDAfterTheCallEnds(t *testing.T) {
	const sid = "CA0123456789abcdef0123456789abcdef"
	c, post := twoWay(t, sid)
	c.Turn = func(context.Context, string, string, string) (string, error) {
		return "Friday at seven then. Thank you, goodbye. [HANGUP]", nil
	}
	ended := make(chan Ended, 2)
	c.OnEnd = func(e Ended) { ended <- e }
	tool := func(name, in string) string {
		for _, tl := range c.Tools() {
			if tl.Spec().Name == name {
				out, err := tl.Run(context.Background(), tools.Call{ChatKey: "telegram:42", Input: json.RawMessage(in)})
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
		}
		t.Fatalf("no tool %s", name)
		return ""
	}
	if placed := tool("phone_call", `{"to":"+61222","opening":"Hi, calling to book a table for two.","purpose":"table for two, Friday"}`); !strings.Contains(placed, sid) {
		t.Fatalf("placed: %s", placed)
	}
	id := callID(c)
	post("/phone/turn?call="+id, url.Values{"CallStatus": {"in-progress"}})
	post("/phone/turn?call="+id, url.Values{"SpeechResult": {"We can do Friday at seven"}})
	post("/phone/status?call="+id, url.Values{"CallStatus": {"completed"}})

	out := tool("call_transcript", `{"id":"`+sid+`"}`)
	if !strings.Contains(out, "ended (completed)") || !strings.Contains(out, "them: We can do Friday at seven") {
		t.Fatalf("transcript: %s", out)
	}
	select {
	case e := <-ended:
		if e.ChatKey != "telegram:42" || e.Status != "completed" || e.SID != sid || len(e.Transcript) != 3 {
			t.Fatalf("ended: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the owner was never told the call ended")
	}
	// A late status callback doesn't report the call twice.
	post("/phone/status?call="+id, url.Values{"CallStatus": {"completed"}})
	select {
	case e := <-ended:
		t.Fatalf("reported twice: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

// They hang up while the twin is still working out its reply: the report
// to the owner waits for that last line instead of leaving it out.
func TestHangUpMidTurnKeepsTheLastLine(t *testing.T) {
	c, post := twoWay(t, "CA2")
	thinking, answer := make(chan struct{}), make(chan struct{})
	c.Turn = func(context.Context, string, string, string) (string, error) {
		close(thinking)
		<-answer
		return "Seven it is. Goodbye. [HANGUP]", nil
	}
	ended := make(chan Ended, 1)
	c.OnEnd = func(e Ended) { ended <- e }
	if _, err := c.Call(context.Background(), "+61222", "Hello.", "book"); err != nil {
		t.Fatal(err)
	}
	id := callID(c)
	post("/phone/turn?call="+id, url.Values{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		post("/phone/turn?call="+id, url.Values{"SpeechResult": {"Seven works, bye"}})
	}()
	<-thinking
	post("/phone/status?call="+id, url.Values{"CallStatus": {"completed"}})
	select {
	case e := <-ended:
		t.Fatalf("reported before the last line: %v", e.Transcript)
	case <-time.After(50 * time.Millisecond):
	}
	close(answer)
	<-done
	select {
	case e := <-ended:
		if n := len(e.Transcript); n != 3 || e.Transcript[n-1] != "twin: Seven it is. Goodbye." {
			t.Fatalf("transcript: %v", e.Transcript)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the owner was never told the call ended")
	}
}

// Twilio's callbacks, the model reading the transcript and a turn in flight
// all touch the same call; run with -race.
func TestCallStateIsSafeUnderConcurrentUse(t *testing.T) {
	c, post := twoWay(t, "CA1")
	c.Turn = func(context.Context, string, string, string) (string, error) {
		_ = c.Transcript("CA1")
		return "Could you repeat that?", nil
	}
	if _, err := c.Call(context.Background(), "+61222", "Hello.", "ask"); err != nil {
		t.Fatal(err)
	}
	id := callID(c)
	post("/phone/turn?call="+id, url.Values{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); post("/phone/turn?call="+id, url.Values{"SpeechResult": {"sorry?"}}) }()
		go func() { defer wg.Done(); post("/phone/status?call="+id, url.Values{"CallStatus": {"in-progress"}}) }()
		go func() { defer wg.Done(); _ = c.Transcript(id) }()
	}
	wg.Wait()
	if len(c.Transcript(id)) < 2 {
		t.Fatal("transcript lost lines")
	}
}

func sign(token, full string, form url.Values) string {
	// Mirror Validate to produce a valid signature for tests.
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sortStrings(keys)
	s := full
	for _, k := range keys {
		s += k + form.Get(k)
	}
	return hmacB64(token, s)
}

// With phone.public_url empty, a call uses the address reach serves now:
// Twilio is told to call back there, and its signatures are checked
// against it. With no address the call is one-way, and a set
// phone.public_url still wins.
func TestPublicURLFallsBackToReach(t *testing.T) {
	var got url.Values
	tw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		_, _ = io.WriteString(w, `{"sid":"CA9"}`)
	}))
	defer tw.Close()
	cfg := config.Phone{AccountSID: "AC123", AuthToken: "tok", From: "+61111"}
	c := New(func() config.Phone { return cfg })
	c.base = tw.URL
	c.Turn = func(context.Context, string, string, string) (string, error) { return "Hello. [HANGUP]", nil }
	handle := "https://ember-otter-42.mirrin.link"
	live := handle + "/"
	c.PublicURL = func() string { return live }

	if _, err := c.Call(context.Background(), "+61222", "Hi.", "say hi"); err != nil {
		t.Fatal(err)
	}
	turn, status := got.Get("Url"), got.Get("StatusCallback")
	if !strings.HasPrefix(turn, handle+"/phone/turn?call=") || !strings.HasPrefix(status, handle+"/phone/status?call=") {
		t.Fatalf("registered %q and %q", turn, status)
	}
	path := strings.TrimPrefix(turn, handle)
	form := url.Values{"CallStatus": {"in-progress"}}
	post := func(base string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Twilio-Signature", sign("tok", base+path, form))
		rec := httptest.NewRecorder()
		c.Webhook(rec, req)
		return rec.Code
	}
	if code := post(handle); code != 200 {
		t.Fatalf("signed for the handle: %d", code)
	}
	if code := post("https://elsewhere.example"); code != 403 {
		t.Fatalf("signed for another address: %d", code)
	}

	live = ""
	if _, err := c.Call(context.Background(), "+61222", "Just a message.", ""); err != nil || got.Get("Url") != "" || !strings.Contains(got.Get("Twiml"), "Just a message.") {
		t.Fatalf("offline: %v %v", err, got)
	}
	cfg.PublicURL = "https://twin.example"
	live = handle
	if _, err := c.Call(context.Background(), "+61222", "Hi.", "say hi"); err != nil || !strings.HasPrefix(got.Get("Url"), "https://twin.example/phone/turn") {
		t.Fatalf("phone.public_url set: %v %v", err, got)
	}
}

// With no voice or language set, a call speaks with the defaults for the
// owner's country, as a new config would have.
func TestEmptyVoiceUsesTheDefaults(t *testing.T) {
	if !inUS(t, "TestEmptyVoiceUsesTheDefaults") {
		return
	}
	got := twiml("", "Hello", "/next", "", true)
	if !strings.Contains(got, `voice="Polly.Joanna-Neural"`) || !strings.Contains(got, `language="en-US"`) ||
		strings.Contains(got, "Olivia") || strings.Contains(got, "en-AU") {
		t.Fatalf("want the US voice and language in %s", got)
	}
}

// inUS runs the calling test again in a child process whose clock and
// locale say United States, so the defaults can't happen to be Australia's
// (the region is read once per process). It reports whether this is the
// child, which does the checks.
func inUS(t *testing.T, name string) bool {
	t.Helper()
	if os.Getenv("MIRRIN_TEST_IN_US") != "" {
		return true
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), "MIRRIN_TEST_IN_US=1", "TZ=America/New_York", "LC_ALL=en_US.UTF-8", "LC_MONETARY=", "LANG=en_US.UTF-8", "MIRRIN_HOME="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return false
}
