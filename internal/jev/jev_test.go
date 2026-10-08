package jev_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/jev"
	"github.com/MavrkAI/Mirrin/internal/jev/jevtest"
)

func client(srv *jevtest.Server, opts ...jev.Option) *jev.Client {
	return jev.New(srv.Key(), append([]jev.Option{jev.WithURL(srv.URL)}, opts...)...)
}

var replyChoice = jev.Choice{Instructions: "What does `owner_replied` say?", Options: []jev.Opt{
	{Name: "done", Rubric: "It is done."}, {Name: "later", Rubric: nil}, {Name: "other", Rubric: map[string]any{"examples": []string{"a question"}}},
}}

// One request carries the state and every question, each in the API's
// shape: criteria in order for a choice, true/false for a noul, levels for
// a score, structured instructions passed through as given.
func TestAskSendsEveryQuestionInOneRequest(t *testing.T) {
	srv := jevtest.New(t)
	srv.Choose("reply", "done", 0.95, 0.9)
	srv.Answer("yes", jev.Answer{Type: "noul", Noul: 0.97})
	srv.Answer("rate", jev.Answer{Type: "score", Score: 1.2, Legend: map[string]string{"0": "calm", "1": "cross", "2": "furious"},
		Probabilities: map[string]float64{"0": 0.1, "1": 0.6, "2": 0.3}, Confidence: 0.7})
	state := map[string]string{"twin_said": "Reminder: pay the window cleaner", "owner_replied": "paid him"}
	res, err := client(srv, jev.WithModel("jev-pinned"), jev.WithUserAgent("mirrin/test")).Ask(t.Context(), state, map[string]jev.Question{
		"reply": replyChoice,
		"yes":   jev.Noul{Instructions: map[string]any{"question": "Is `x` paid?", "x": "the bill"}, True: "Paid", False: "Not paid"},
		"rate":  &jev.Score{Instructions: "How cross?", Levels: []any{"calm", "cross", "furious"}},
		"plain": jev.Noul{Instructions: "Is this a test?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Calls() != 1 {
		t.Fatalf("%d requests, want 1", srv.Calls())
	}
	r := srv.Requests()[0]
	if r.Auth != "Bearer test-key" || r.Model != "jev-pinned" || len(r.Questions) != 4 {
		t.Fatalf("request: %+v", r)
	}
	var gotState map[string]string
	if json.Unmarshal(r.State, &gotState) != nil || gotState["owner_replied"] != "paid him" {
		t.Fatalf("state %s", r.State)
	}
	if q := r.Questions["reply"]; q.Type != "choice" || strings.Join(q.Options(), ",") != "done,later,other" ||
		string(q.Criteria) != `{"done":"It is done.","later":null,"other":{"examples":["a question"]}}` {
		t.Fatalf("choice sent as %+v (%s)", q, q.Criteria)
	}
	if q := r.Questions["yes"]; q.Type != "noul" || string(q.Instructions) != `{"question":"Is `+"`x`"+` paid?","x":"the bill"}` || string(q.Criteria) != `{"false":"Not paid","true":"Paid"}` {
		t.Fatalf("noul sent as %s / %s", q.Instructions, q.Criteria)
	}
	if q := r.Questions["plain"]; len(q.Criteria) != 0 {
		t.Fatalf("a noul without criteria sent %s", q.Criteria)
	}
	if q := r.Questions["rate"]; q.Type != "score" || string(q.Criteria) != `["calm","cross","furious"]` {
		t.Fatalf("score sent as %s", q.Criteria)
	}
	if a := res.Answers["reply"]; a.Type != "choice" || a.Choice != "done" || a.P("done") != 0.95 || a.P("nope") != 0 || a.Confidence != 0.9 {
		t.Fatalf("choice answer %+v", a)
	}
	if a := res.Answers["yes"]; a.Noul != 0.97 {
		t.Fatalf("noul answer %+v", a)
	}
	if a := res.Answers["rate"]; a.Score != 1.2 || a.Legend["2"] != "furious" || a.P("1") != 0.6 {
		t.Fatalf("score answer %+v", a)
	}
	if a := res.Answers["plain"]; a.Noul != 0.5 {
		t.Fatalf("an unscripted noul should be neutral: %+v", a)
	}
	if res.Model != jevtest.Model || res.Usage.InputTokens <= 0 || res.Usage.OutputTokens != 40 {
		t.Fatalf("result %+v", res)
	}
}

func TestHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"model":"m","answers":{"q":{"type":"noul","noul":1}},"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	if _, err := jev.New("k", jev.WithURL(srv.URL), jev.WithUserAgent("mirrin/1.0")).Ask(t.Context(), "s", map[string]jev.Question{"q": jev.Noul{Instructions: "q?"}}); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer k" || got.Get("Content-Type") != "application/json" || got.Get("User-Agent") != "mirrin/1.0" {
		t.Fatalf("headers %v", got)
	}
}

// An unscripted choice is even odds with no confidence, so callers fall back.
func TestFakeAnswersNeutrally(t *testing.T) {
	srv := jevtest.New(t)
	res, err := client(srv).Ask(t.Context(), "x", map[string]jev.Question{
		"c": replyChoice, "s": jev.Score{Instructions: "rate", Levels: []any{"low", "high"}}})
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Answers["c"]; a.Confidence != 0 || a.P("done") > 0.34 || a.P("done") < 0.33 {
		t.Fatalf("neutral choice %+v", a)
	}
	if a := res.Answers["s"]; a.Confidence != 0 || a.Score != 0.5 || a.Legend["1"] != "high" {
		t.Fatalf("neutral score %+v", a)
	}
}

func ask(c *jev.Client, ctx context.Context) error {
	_, err := c.Ask(ctx, "x", map[string]jev.Question{"q": jev.Noul{Instructions: "q?"}})
	return err
}

func TestRetriesOnceWhenBusy(t *testing.T) {
	for _, status := range []int{429, 529} {
		srv := jevtest.New(t)
		srv.Fail(status, 1)
		if err := ask(client(srv), t.Context()); err != nil {
			t.Fatalf("%d then 200: %v", status, err)
		}
		if srv.Calls() != 2 {
			t.Fatalf("%d then 200: %d requests", status, srv.Calls())
		}
	}
	srv := jevtest.New(t)
	srv.Fail(529, 2)
	err := ask(client(srv), t.Context())
	var se *jev.StatusError
	if !errors.As(err, &se) || se.Code != 529 || !se.Retryable || srv.Calls() != 2 {
		t.Fatalf("529 twice: %v after %d requests", err, srv.Calls())
	}
}

func TestNoRetryOnOtherFailures(t *testing.T) {
	for _, status := range []int{401, 422, 500, 503} {
		srv := jevtest.New(t)
		srv.Fail(status, 3)
		err := ask(client(srv), t.Context())
		var se *jev.StatusError
		if !errors.As(err, &se) || se.Code != status || se.Retryable || srv.Calls() != 1 {
			t.Errorf("%d: %v after %d requests", status, err, srv.Calls())
		}
	}
	// A wrong key is the fake's own 401.
	srv := jevtest.New(t)
	err := ask(jev.New("wrong-key", jev.WithURL(srv.URL)), t.Context())
	var se *jev.StatusError
	if !errors.As(err, &se) || se.Code != 401 || srv.Calls() != 1 {
		t.Fatalf("wrong key: %v", err)
	}
	// Nor on a transport error.
	var n atomic.Int32
	hc := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		n.Add(1)
		return nil, errors.New("connection refused")
	})}
	if err := ask(jev.New("k", jev.WithHTTPClient(hc)), t.Context()); err == nil || n.Load() != 1 {
		t.Fatalf("transport error: %v after %d tries", err, n.Load())
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRetryAfter(t *testing.T) {
	serve := func(retryAfter string) (*httptest.Server, *atomic.Int32) {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				w.Header().Set("Retry-After", retryAfter)
				w.WriteHeader(429)
				return
			}
			_, _ = w.Write([]byte(`{"model":"m","answers":{"q":{"type":"noul","noul":0.2}},"usage":{}}`))
		}))
		t.Cleanup(srv.Close)
		return srv, &n
	}
	// Honoured when it fits.
	srv, n := serve("1")
	start := time.Now()
	if err := ask(jev.New("k", jev.WithURL(srv.URL), jev.WithTimeout(3*time.Second)), t.Context()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < time.Second || n.Load() != 2 {
		t.Fatalf("waited %v, %d requests", took, n.Load())
	}
	// Skipped when it would pass the deadline: the 429 comes back at once.
	srv, n = serve("5")
	start = time.Now()
	err := ask(jev.New("k", jev.WithURL(srv.URL)), t.Context())
	var se *jev.StatusError
	if !errors.As(err, &se) || se.Code != 429 || n.Load() != 1 || time.Since(start) > time.Second {
		t.Fatalf("%v after %d requests in %v", err, n.Load(), time.Since(start))
	}
}

func TestDeadlines(t *testing.T) {
	srv := jevtest.New(t)
	srv.Delay(3 * time.Second)
	start := time.Now()
	err := ask(client(srv), t.Context())
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("slow server: %v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	start = time.Now()
	if err := ask(client(srv), ctx); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("cancelled: %v after %v", err, time.Since(start))
	}
	// The caller's sooner deadline wins.
	ctx, cancel2 := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel2()
	start = time.Now()
	if err := ask(client(srv), ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("caller's deadline: %v after %v", err, time.Since(start))
	}
}

func TestNoKeyOrBadQuestionSendsNothing(t *testing.T) {
	srv := jevtest.New(t)
	if err := ask(jev.New("", jev.WithURL(srv.URL)), t.Context()); !errors.Is(err, jev.ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
	var nilClient *jev.Client
	if err := ask(nilClient, t.Context()); !errors.Is(err, jev.ErrNoKey) {
		t.Fatalf("nil client: %v", err)
	}
	opts := func(n int) []jev.Opt {
		var o []jev.Opt
		for i := range n {
			o = append(o, jev.Opt{Name: "o" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
		}
		return o
	}
	levels := func(n int) []any {
		l := make([]any, n)
		for i := range l {
			l[i] = "level"
		}
		return l
	}
	var nilNoul *jev.Noul
	for name, qs := range map[string]map[string]jev.Question{
		"no questions":      {},
		"empty id":          {"": jev.Noul{Instructions: "q?"}},
		"nil question":      {"q": nil},
		"nil pointer":       {"q": nilNoul},
		"no instructions":   {"q": jev.Noul{}},
		"blank":             {"q": jev.Noul{Instructions: "  "}},
		"one option":        {"q": jev.Choice{Instructions: "q?", Options: opts(1)}},
		"256 options":       {"q": jev.Choice{Instructions: "q?", Options: opts(256)}},
		"same option twice": {"q": jev.Choice{Instructions: "q?", Options: []jev.Opt{{Name: "a"}, {Name: "a"}}}},
		"unnamed option":    {"q": jev.Choice{Instructions: "q?", Options: []jev.Opt{{Name: "a"}, {Name: ""}}}},
		"one level":         {"q": jev.Score{Instructions: "q?", Levels: levels(1)}},
		"11 levels":         {"q": jev.Score{Instructions: "q?", Levels: levels(11)}},
		"empty level":       {"q": jev.Score{Instructions: "q?", Levels: []any{"a", nil}}},
	} {
		if _, err := client(srv).Ask(t.Context(), "x", qs); !errors.Is(err, jev.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := client(srv).Ask(t.Context(), nil, map[string]jev.Question{"q": jev.Noul{Instructions: "q?"}}); !errors.Is(err, jev.ErrInvalid) {
		t.Errorf("no state: %v", err)
	}
	if _, err := client(srv).Ask(t.Context(), map[string]any{"f": func() {}}, map[string]jev.Question{"q": jev.Noul{Instructions: "q?"}}); !errors.Is(err, jev.ErrInvalid) {
		t.Errorf("state that isn't JSON: %v", err)
	}
	// The limits themselves are fine.
	if _, err := client(srv).Ask(t.Context(), "x", map[string]jev.Question{
		"a": jev.Choice{Instructions: "q?", Options: opts(255)}, "b": jev.Score{Instructions: "q?", Levels: levels(10)},
		"c": jev.Choice{Instructions: "q?", Options: opts(2)}, "d": jev.Score{Instructions: "q?", Levels: levels(2)}}); err != nil {
		t.Fatalf("at the limits: %v", err)
	}
	if srv.Calls() != 1 {
		t.Fatalf("%d requests, want only the one at the limits", srv.Calls())
	}
}

func TestMalformedAnswers(t *testing.T) {
	qs := map[string]jev.Question{"c": replyChoice, "n": jev.Noul{Instructions: "q?"}, "s": jev.Score{Instructions: "q?", Levels: []any{"a", "b", "c"}}}
	goodC := `{"type":"choice","choice":"done","probabilities":{"done":0.9,"later":0.05,"other":0.05},"confidence":0.8}`
	goodN := `{"type":"noul","noul":0.4}`
	goodS := `{"type":"score","score":1.5,"legend":{"0":"a","1":"b","2":"c"},"probabilities":{"0":0,"1":0.5,"2":0.5},"confidence":0.5}`
	for name, tc := range map[string][3]string{
		"missing id":         {goodC, "", goodS},
		"wrong type":         {goodC, `{"type":"choice","noul":0.4}`, goodS},
		"no noul":            {goodC, `{"type":"noul"}`, goodS},
		"noul over 1":        {goodC, `{"type":"noul","noul":1.2}`, goodS},
		"negative noul":      {goodC, `{"type":"noul","noul":-0.1}`, goodS},
		"NaN":                {goodC, `{"type":"noul","noul":NaN}`, goodS},
		"huge":               {goodC, `{"type":"noul","noul":1e999}`, goodS},
		"p over 1":           {`{"type":"choice","choice":"done","probabilities":{"done":1.5},"confidence":0.8}`, goodN, goodS},
		"unknown option":     {`{"type":"choice","choice":"done","probabilities":{"done":0.9,"pay":0.1},"confidence":0.8}`, goodN, goodS},
		"unknown choice":     {`{"type":"choice","choice":"pay","probabilities":{"done":1},"confidence":0.8}`, goodN, goodS},
		"no probabilities":   {`{"type":"choice","choice":"done","confidence":0.8}`, goodN, goodS},
		"odds don't add up":  {`{"type":"choice","choice":"done","probabilities":{"done":0.5},"confidence":0.8}`, goodN, goodS},
		"no confidence":      {`{"type":"choice","choice":"done","probabilities":{"done":1}}`, goodN, goodS},
		"confidence over 1":  {`{"type":"choice","choice":"done","probabilities":{"done":1},"confidence":2}`, goodN, goodS},
		"score off the end":  {goodC, goodN, `{"type":"score","score":3,"probabilities":{"2":1},"confidence":0.5}`},
		"unknown level":      {goodC, goodN, `{"type":"score","score":1,"probabilities":{"1":0.5,"7":0.5},"confidence":0.5}`},
		"unknown legend":     {goodC, goodN, `{"type":"score","score":1,"legend":{"9":"x"},"probabilities":{"1":1},"confidence":0.5}`},
		"answer is a string": {goodC, `"yes"`, goodS},
	} {
		answers := []string{}
		for i, id := range []string{"c", "n", "s"} {
			if tc[i] != "" {
				answers = append(answers, `"`+id+`":`+tc[i])
			}
		}
		body := `{"model":"m","answers":{` + strings.Join(answers, ",") + `},"usage":{"input_tokens":1,"output_tokens":1}}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		if _, err := jev.New("k", jev.WithURL(srv.URL)).Ask(t.Context(), "x", qs); !errors.Is(err, jev.ErrBadAnswer) {
			t.Errorf("%s: %v", name, err)
		}
		srv.Close()
	}
	// The good ones, with an extra answer nobody asked for, are read.
	body := `{"model":"m","answers":{"c":` + goodC + `,"n":` + goodN + `,"s":` + goodS + `,"extra":{"type":"noul","noul":9}},"usage":{"input_tokens":1,"output_tokens":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	res, err := jev.New("k", jev.WithURL(srv.URL)).Ask(t.Context(), "x", qs)
	if err != nil || len(res.Answers) != 3 || res.Answers["s"].Score != 1.5 {
		t.Fatalf("good answers: %v %+v", err, res)
	}
	// So is a scripted wrong option from the fake, and refused.
	fake := jevtest.New(t)
	fake.Choose("c", "pay", 0.9, 0.9)
	if _, err := client(fake).Ask(t.Context(), "x", map[string]jev.Question{"c": replyChoice}); !errors.Is(err, jev.ErrBadAnswer) {
		t.Fatalf("an option nobody offered: %v", err)
	}
	fake.RawAnswer("c", `{"type":"choice"}`)
	if _, err := client(fake).Ask(t.Context(), "x", map[string]jev.Question{"c": replyChoice}); !errors.Is(err, jev.ErrBadAnswer) {
		t.Fatalf("a raw bad answer: %v", err)
	}
}

func TestOversizedResponseIsRefused(t *testing.T) {
	pad := strings.Repeat("x", 70<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"` + pad + `","answers":{"q":{"type":"noul","noul":1}},"usage":{}}`))
	}))
	defer srv.Close()
	if err := ask(jev.New("k", jev.WithURL(srv.URL)), t.Context()); !errors.Is(err, jev.ErrBadAnswer) {
		t.Fatalf("70 KiB: %v", err)
	}
}

// No error ever repeats the key, the state or what the server said.
func TestErrorsCarryNoSecrets(t *testing.T) {
	const key, secretState, echo = "ts-SECRET-KEY-123", "SECRET-STATE-abc", "SECRET-ECHO"
	for _, status := range []int{200, 401, 422, 429, 500, 529} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"` + echo + ` ` + key + ` ` + secretState + `"}`))
		}))
		_, err := jev.New(key, jev.WithURL(srv.URL)).Ask(t.Context(), secretState, map[string]jev.Question{"q": jev.Noul{Instructions: "q?"}})
		srv.Close()
		if err == nil {
			t.Fatalf("%d: no error", status)
		}
		if s := err.Error(); strings.Contains(s, "SECRET") {
			t.Errorf("%d: %q", status, s)
		}
	}
	// A transport error names neither.
	hc := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed for " + r.Header.Get("Authorization"))
	})}
	_, err := jev.New(key, jev.WithHTTPClient(hc)).Ask(t.Context(), secretState, map[string]jev.Question{"q": jev.Noul{Instructions: "q?"}})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("transport error: %v", err)
	}
}

// The default client sends through http.DefaultTransport, where an egress
// guard can see it.
func TestDefaultTransport(t *testing.T) {
	srv := jevtest.New(t)
	var saw atomic.Int32
	orig := http.DefaultTransport
	http.DefaultTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		saw.Add(1)
		return orig.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = orig })
	if err := ask(client(srv), t.Context()); err != nil || saw.Load() != 1 {
		t.Fatalf("%v; the default transport saw %d requests", err, saw.Load())
	}
}

func TestConcurrentUse(t *testing.T) {
	srv := jevtest.New(t)
	srv.Answer("q", jev.Answer{Type: "noul", Noul: 0.9})
	c := client(srv)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if err := ask(c, t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if srv.Calls() != 16 {
		t.Fatalf("%d requests", srv.Calls())
	}
}

// TypeSafe rounds each probability to two decimals, so a choice with many
// options can add up to well short of 1 (30 options at 0.03 each is 0.9).
// That is still a good answer; a sum far from 1 still isn't.
func TestRoundedProbabilitiesAreRead(t *testing.T) {
	var opts []jev.Opt
	probs := map[string]float64{}
	for i := range 30 {
		name := "o" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		opts = append(opts, jev.Opt{Name: name})
		probs[name] = 0.03
	}
	q := map[string]jev.Question{"c": jev.Choice{Instructions: "Which?", Options: opts}}
	answer := func(p map[string]float64) error {
		b, _ := json.Marshal(map[string]any{"model": "m", "usage": map[string]int{},
			"answers": map[string]any{"c": map[string]any{"type": "choice", "choice": "oaa", "probabilities": p, "confidence": 0.1}}})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(b) }))
		defer srv.Close()
		_, err := jev.New("k", jev.WithURL(srv.URL)).Ask(t.Context(), "x", q)
		return err
	}
	if err := answer(probs); err != nil {
		t.Fatalf("30 options at 0.03: %v", err)
	}
	for k := range probs {
		probs[k] = 0.01
	}
	if err := answer(probs); !errors.Is(err, jev.ErrBadAnswer) {
		t.Fatalf("30 options at 0.01 (0.3 in all): %v", err)
	}
}

// A score's levels may be objects, and its legend may repeat them as
// objects: the answer is still read, each description kept as JSON text.
func TestStructuredScoreLevels(t *testing.T) {
	srv := jevtest.New(t)
	srv.RawAnswer("s", `{"type":"score","score":0.8,"legend":{"0":{"name":"calm"},"1":"cross"},"probabilities":{"0":0.2,"1":0.8},"confidence":0.6}`)
	res, err := client(srv).Ask(t.Context(), "x", map[string]jev.Question{
		"s": jev.Score{Instructions: "How cross?", Levels: []any{map[string]string{"name": "calm"}, "cross"}}})
	if err != nil {
		t.Fatal(err)
	}
	if a := res.Answers["s"]; a.Score != 0.8 || a.Legend["0"] != `{"name":"calm"}` || a.Legend["1"] != "cross" {
		t.Fatalf("answer %+v", a)
	}
}
