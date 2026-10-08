package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// briefingFake is a welcome backend whose hello offers the briefing, with
// a calendar that may be connected.
type briefingFake struct {
	welcomeFake
	calendar bool
	answers  []string
}

func (b *briefingFake) Hello(ctx context.Context, delta, status func(string)) (HelloReply, error) {
	r, err := b.welcomeFake.Hello(ctx, delta, status)
	r.Offer = &BriefingOffer{ID: 7, Text: "Want me to brief you at seven tomorrow?", Time: "07:00"}
	return r, err
}
func (b *briefingFake) CalendarConnected(context.Context) bool { return b.calendar }
func (b *briefingFake) AnswerBriefing(_ context.Context, yes bool, at string) (string, error) {
	if yes && at == "nonsense" {
		return "", &HumanError{Sentence: "That time doesn't look right.", Fix: "Pick a time like 7:00."}
	}
	if !yes {
		b.answers = append(b.answers, "later")
		return "No problem.", nil
	}
	b.answers = append(b.answers, "yes "+at)
	return "Done.", nil
}

// The hello's stream carries the offer after its words; the card's answer
// reaches the twin, in plain words both ways; and the page can ask whether
// a calendar is connected. All on this computer only.
func TestWelcomeBriefingOfferAndCalendar(t *testing.T) {
	e := newEnv(t)
	b := &briefingFake{welcomeFake: welcomeFake{you: "Akshay"}}
	e.s.WithWelcome(b)
	w := e.do(onLoopback, req{method: "POST", path: "/welcome/hello", header: bearer(master)})
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "event: offer\ndata: {\"id\":7,\"text\":\"Want me to brief you at seven tomorrow?\",\"time\":\"07:00\"}") || strings.Index(body, "event: offer") < strings.Index(body, "event: done") {
		t.Fatalf("%d %s", w.Code, body)
	}

	for _, c := range []struct{ body, want string }{
		{`{"Yes":true,"Time":"07:30"}`, "Done."},
		{`{"Yes":false}`, "No problem."},
	} {
		w := e.do(onLoopback, req{method: "POST", path: "/welcome/briefing", body: c.body, header: bearer(master)})
		var got map[string]string
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got["text"] != c.want {
			t.Fatalf("%s: %d %s", c.body, w.Code, w.Body)
		}
	}
	if strings.Join(b.answers, ", ") != "yes 07:30, later" {
		t.Fatalf("answers %q", b.answers)
	}
	w = e.do(onLoopback, req{method: "POST", path: "/welcome/briefing", body: `{"Yes":true,"Time":"nonsense"}`, header: bearer(master)})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Pick a time like 7:00.") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}

	for _, connected := range []bool{false, true} {
		b.calendar = connected
		w := e.do(onLoopback, req{path: "/welcome/calendar", header: bearer(master)})
		var got map[string]bool
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got["connected"] != connected {
			t.Fatalf("connected %v: %d %s", connected, w.Code, w.Body)
		}
	}
	for _, q := range []req{{path: "/welcome/calendar"}, {method: "POST", path: "/welcome/briefing", body: `{"Yes":true,"Time":"07:00"}`}} {
		q.header = bearer(master)
		if w := e.do(onRemote, q); w.Code != 404 {
			t.Errorf("%s from another device: %d", q.path, w.Code)
		}
	}
	if len(b.answers) != 2 {
		t.Fatalf("answered from another device: %q", b.answers)
	}
}
