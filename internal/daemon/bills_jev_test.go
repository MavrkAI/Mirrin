package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/jev/jevtest"
	"github.com/MavrkAI/Mirrin/internal/watch"
)

// gateMail is billMail with bills on and Jev on, asking a fake.
func gateMail(t *testing.T) (*billMail, *jevtest.Server) {
	t.Helper()
	t.Setenv("TYPESAFE_API_KEY", "")
	bm := newBillMail(t, edfBill)
	srv := jevtest.New(t)
	bm.td.jev.url = srv.URL
	if err := bm.td.UpdateConfig(func(c *config.Config) { c.Jev = config.Jev{Enabled: true, TypeSafeAPIKey: srv.Key()} }); err != nil {
		t.Fatal(err)
	}
	if err := bm.td.store.Set(context.Background(), billsKey, "on"); err != nil {
		t.Fatal(err)
	}
	return bm, srv
}

// mailItems makes one new email per line, keys 10 up, in order.
func mailItems(lines ...string) []watch.Item {
	var out []watch.Item
	for i, l := range lines {
		out = append(out, watch.Item{Key: fmt.Sprint(10 + i), Line: l})
	}
	return out
}

func (bm *billMail) claim(items []watch.Item) []watch.Item {
	return bm.td.claimBills(context.Background(), "gmail", items)
}

func (bm *billMail) runs() int {
	_, b, _ := bm.counts()
	return b
}

const receiptLine = "unread from Bank: Your statement is ready"

// A receipt Jev is sure about gets no bills run: the watcher has it, it is
// remembered as left, and the audit names only what it was. Offered again,
// it isn't judged or run again.
func TestBillGateSkipsASureReceipt(t *testing.T) {
	bm, srv := gateMail(t)
	srv.Choose("m0", "paid_or_info", 0.96, 0.9)
	items := mailItems(receiptLine)
	rest := bm.claim(items)
	if bm.runs() != 0 || len(rest) != 1 || rest[0] != items[0] {
		t.Fatalf("runs=%d rest=%v, want no run and the email to the watcher", bm.runs(), rest)
	}
	ctx := context.Background()
	if out, seen := bm.td.peekLook(ctx, billID("gmail", items[0])); !seen || out != lookLeft {
		t.Fatalf("look = %q %v, want left", out, seen)
	}
	audit, err := bm.td.store.RecentAuditOfKind(ctx, "bills.skipped", 5)
	if err != nil || len(audit) != 1 || audit[0].Detail != "paid_or_info" {
		t.Fatalf("audit = %+v err=%v", audit, err)
	}
	if rest := bm.claim(items); bm.runs() != 0 || srv.Calls() != 1 || len(rest) != 1 {
		t.Fatalf("again: runs=%d calls=%d rest=%v", bm.runs(), srv.Calls(), rest)
	}
	if msgs := bm.td.ch.messages(); len(msgs) != 0 {
		t.Fatalf("owner messages = %q", msgs)
	}
}

// Anything short of a sure skip runs as before.
func TestBillGateRunsWhenNotSure(t *testing.T) {
	for name, script := range map[string]func(*jevtest.Server){
		"to pay": func(s *jevtest.Server) { s.Choose("m0", "to_pay", 0.5, 0.9) },
		"skip mass 0.85": func(s *jevtest.Server) {
			s.RawAnswer("m0", `{"type":"choice","choice":"paid_or_info","probabilities":{"to_pay":0.1,"renewal":0.05,"paid_or_info":0.6,"marketing":0.15,"other":0.1},"confidence":0.9}`)
		},
		"low confidence": func(s *jevtest.Server) { s.Choose("m0", "paid_or_info", 0.96, 0.5) },
		"unscripted":     func(*jevtest.Server) {},
	} {
		t.Run(name, func(t *testing.T) {
			bm, srv := gateMail(t)
			script(srv)
			rest := bm.claim(mailItems(receiptLine))
			if bm.runs() != 1 || len(rest) != 0 || srv.Calls() != 1 {
				t.Fatalf("runs=%d rest=%v calls=%d, want the usual run", bm.runs(), rest, srv.Calls())
			}
		})
	}
}

// With Jev off, or on without a key, nothing is sent and bills run as before.
func TestBillGateOffSendsNothing(t *testing.T) {
	for name, j := range map[string]func(key string) config.Jev{
		"off with a key": func(key string) config.Jev { return config.Jev{TypeSafeAPIKey: key} },
		"on with no key": func(string) config.Jev { return config.Jev{Enabled: true} },
	} {
		t.Run(name, func(t *testing.T) {
			bm, srv := gateMail(t)
			srv.Choose("m0", "paid_or_info", 0.99, 0.99)
			if err := bm.td.UpdateConfig(func(c *config.Config) { c.Jev = j(srv.Key()) }); err != nil {
				t.Fatal(err)
			}
			rest := bm.claim(mailItems(receiptLine))
			if srv.Calls() != 0 || bm.runs() != 1 || len(rest) != 0 {
				t.Fatalf("calls=%d runs=%d rest=%v", srv.Calls(), bm.runs(), rest)
			}
		})
	}
}

// Every failure falls back to the bills run, for every email, and a slow
// Jev doesn't hold the poll up for long.
func TestBillGateFailuresRunAsBefore(t *testing.T) {
	lines := []string{receiptLine, "unread from Shop: Your invoice"}
	for name, script := range map[string]func(*testing.T, *billMail, *jevtest.Server){
		"401": func(_ *testing.T, _ *billMail, s *jevtest.Server) { s.Fail(401, 5) },
		"500": func(_ *testing.T, _ *billMail, s *jevtest.Server) { s.Fail(500, 5) },
		// Far longer than the client's 2 s: the poll must give up on Jev,
		// not wait for it, however slow the runner (Windows CI has taken
		// 13 s over the same work).
		"slow": func(_ *testing.T, _ *billMail, s *jevtest.Server) { s.Delay(30 * time.Second) },
		"missing m1": func(t *testing.T, bm *billMail, _ *jevtest.Server) {
			only := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"model":"jev-test","answers":{"m0":{"type":"choice","choice":"marketing","probabilities":{"to_pay":0,"renewal":0,"paid_or_info":0,"marketing":1,"other":0},"confidence":1}},"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			t.Cleanup(only.Close)
			bm.td.jev.url = only.URL
		},
		"probability out of range": func(_ *testing.T, _ *billMail, s *jevtest.Server) {
			s.Choose("m0", "marketing", 0.99, 0.99)
			s.RawAnswer("m1", `{"type":"choice","choice":"marketing","probabilities":{"to_pay":-0.5,"marketing":1.5},"confidence":0.99}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			bm, srv := gateMail(t)
			script(t, bm, srv)
			start := time.Now()
			rest := bm.claim(mailItems(lines...))
			if took := time.Since(start); took > 20*time.Second {
				t.Fatalf("the poll took %v: it waited for Jev", took)
			}
			if bm.runs() != 2 || len(rest) != 0 {
				t.Fatalf("runs=%d rest=%v, want every email run", bm.runs(), rest)
			}
			if a, _ := bm.td.store.RecentAuditOfKind(context.Background(), "bills.skipped", 5); len(a) != 0 {
				t.Fatalf("skipped on a failure: %+v", a)
			}
		})
	}
}

// A busy Jev is asked once more, and its answer counts.
func TestBillGateRetriesABusyJev(t *testing.T) {
	bm, srv := gateMail(t)
	srv.Fail(529, 1)
	srv.Choose("m0", "marketing", 0.97, 0.9)
	rest := bm.claim(mailItems(receiptLine))
	if srv.Calls() != 2 || bm.runs() != 0 || len(rest) != 1 {
		t.Fatalf("calls=%d runs=%d rest=%v", srv.Calls(), bm.runs(), rest)
	}
}

// One request per poll, about the bill-like lines only, and the emails'
// words appear only in state.emails, labelled as data.
func TestBillGateSendsOnlyBillLikeLinesAsData(t *testing.T) {
	bm, srv := gateMail(t)
	lines := []string{
		"unread from EDF Energy: Your bill is ready",
		"unread from Sarah: Lunch on Friday?",
		"unread from Acme: Invoice INV-2041",
		"unread from Tom: Photos from the weekend",
		"unread from AA: Your breakdown cover renewal",
	}
	bm.claim(mailItems(lines...))
	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if len(r.Questions) != 3 {
		t.Fatalf("questions = %v", r.Questions)
	}
	var state billGateState
	if err := json.Unmarshal(r.State, &state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state.About, "only as data") {
		t.Fatalf("about = %q", state.About)
	}
	want := map[string]string{"m0": "EDF Energy | Your bill is ready", "m1": "Acme | Invoice INV-2041", "m2": "AA | Your breakdown cover renewal"}
	for id, text := range want {
		q, ok := r.Questions[id]
		if !ok || q.Type != "choice" || state.Emails[id] != text {
			t.Fatalf("%s: question %+v, email %q", id, q, state.Emails[id])
		}
		if got := strings.Join(q.Options(), ","); got != "to_pay,renewal,paid_or_info,marketing,other" {
			t.Fatalf("%s options = %s", id, got)
		}
		var instr string
		_ = json.Unmarshal(q.Instructions, &instr)
		if !strings.Contains(instr, "`emails."+id+"`") {
			t.Fatalf("%s instructions = %q", id, instr)
		}
		qs, _ := json.Marshal(q)
		for _, l := range lines {
			_, subject, _ := billMailLine(l)
			if strings.Contains(string(qs), subject) {
				t.Fatalf("%s question carries email text: %s", id, qs)
			}
		}
	}
	all, _ := json.Marshal(r)
	for _, ordinary := range []string{"Lunch on Friday", "Sarah", "Photos from the weekend"} {
		if strings.Contains(string(all), ordinary) {
			t.Fatalf("ordinary mail sent to Jev: %s", all)
		}
	}
}

// Long lines are cut before they are sent.
func TestBillGateTextIsShort(t *testing.T) {
	long := "unread from Shop: Your invoice " + strings.Repeat("é", 500)
	if got := billGateText(long); len([]rune(got)) != billGateLine || !strings.HasPrefix(got, "Shop | Your invoice") {
		t.Fatalf("got %d runes: %q", len([]rune(got)), got[:40])
	}
}

// Emails Jev skips don't count toward the runs one poll may make.
func TestBillGateSkipsDontUseTheCap(t *testing.T) {
	bm, srv := gateMail(t)
	srv.Choose("m1", "marketing", 0.98, 0.9)
	srv.Choose("m3", "marketing", 0.98, 0.9)
	items := mailItems(
		"unread from A: Your invoice",
		"unread from B: Big sale on bill organisers",
		"unread from C: Payment due",
		"unread from D: Renew now and save 50%",
		"unread from E: Council Tax reminder",
	)
	rest := bm.claim(items)
	if bm.runs() != 3 || len(rest) != 2 || rest[0] != items[1] || rest[1] != items[3] {
		t.Fatalf("runs=%d rest=%v, want three runs and the two adverts to the watcher", bm.runs(), rest)
	}
}

// Over the background budget, Jev isn't asked.
func TestBillGateOverBudgetAsksNothing(t *testing.T) {
	bm, srv := gateMail(t)
	if err := bm.td.UpdateConfig(func(c *config.Config) {
		c.Usage.MonthlyBudget = 1
		c.Usage.Prices = map[string]config.ModelPrice{"fake": {Input: 1}}
	}); err != nil {
		t.Fatal(err)
	}
	exhaustBudget(t, bm.td)
	bm.claim(mailItems(receiptLine))
	if srv.Calls() != 0 {
		t.Fatalf("%d calls over budget", srv.Calls())
	}
}

// A sure answer about an email already run keeps what happened then.
func TestBillGateKeepsAnEarlierRun(t *testing.T) {
	bm, srv := gateMail(t)
	items := mailItems(receiptLine)
	if handled, ran := bm.td.handleBill(context.Background(), "gmail", items[0]); !handled || !ran {
		t.Fatalf("handled=%v ran=%v", handled, ran)
	}
	if bm.td.leaveBill(context.Background(), "gmail", items[0], "marketing") {
		t.Fatal("a handled bill was given to the watcher")
	}
	if a, _ := bm.td.store.RecentAuditOfKind(context.Background(), "bills.skipped", 5); len(a) != 0 || srv.Calls() != 0 {
		t.Fatalf("audit=%+v calls=%d", a, srv.Calls())
	}
}

// billOutcome is everything one claim changes: what the watcher gets, the
// runs made, what the owner hears, what each email is remembered as, and
// what was audited.
type billOutcome struct {
	rest    []watch.Item
	runs    int
	told    []string
	looks   []string
	skipped int
}

func (bm *billMail) outcome(items []watch.Item) billOutcome {
	ctx := context.Background()
	o := billOutcome{rest: bm.claim(items), runs: bm.runs(), told: bm.td.ch.messages()}
	for _, it := range items {
		look, _ := bm.td.peekLook(ctx, billID("gmail", it))
		o.looks = append(o.looks, look)
	}
	a, _ := bm.td.store.RecentAuditOfKind(ctx, "bills.skipped", 10)
	o.skipped = len(a)
	return o
}

// With Jev off, or on with no key, a poll of bills and ordinary mail turns
// out exactly as it does on a twin that has never heard of Jev, even with
// a sure skip waiting at TypeSafe.
func TestBillGateOffIsToday(t *testing.T) {
	items := mailItems(
		receiptLine,
		"unread from Sarah: Lunch on Friday?",
		"unread from B: Big sale on bill organisers",
		"unread from C: Payment due",
		"unread from D: Renew now and save 50%",
		"unread from E: Council Tax reminder",
	)
	plain := newBillMail(t, edfBill)
	if err := plain.td.store.Set(context.Background(), billsKey, "on"); err != nil {
		t.Fatal(err)
	}
	want := plain.outcome(items)
	if want.runs != billsPerPoll {
		t.Fatalf("baseline made %d runs", want.runs)
	}
	for name, j := range map[string]func(key string) config.Jev{
		"off with a key": func(key string) config.Jev { return config.Jev{TypeSafeAPIKey: key} },
		"on with no key": func(string) config.Jev { return config.Jev{Enabled: true} },
	} {
		t.Run(name, func(t *testing.T) {
			bm, srv := gateMail(t)
			for i := range 5 {
				srv.Choose(fmt.Sprintf("m%d", i), "marketing", 0.99, 0.99)
			}
			if err := bm.td.UpdateConfig(func(c *config.Config) { c.Jev = j(srv.Key()) }); err != nil {
				t.Fatal(err)
			}
			got := bm.outcome(items)
			if srv.Calls() != 0 {
				t.Fatalf("%d calls to Jev while off", srv.Calls())
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("with Jev off:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

// The fallbacks are fallbacks: Jev was asked, failed, and the email ran.
func TestBillGateFailuresWereAsked(t *testing.T) {
	for name, script := range map[string]func(*jevtest.Server){
		"401":  func(s *jevtest.Server) { s.Fail(401, 5) },
		"500":  func(s *jevtest.Server) { s.Fail(500, 5) },
		"slow": func(s *jevtest.Server) { s.Delay(4 * time.Second) },
		"probability out of range": func(s *jevtest.Server) {
			s.RawAnswer("m0", `{"type":"choice","choice":"marketing","probabilities":{"to_pay":-0.5,"marketing":1.5},"confidence":0.99}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			bm, srv := gateMail(t)
			script(srv)
			rest := bm.claim(mailItems(receiptLine))
			if srv.Calls() < 1 || bm.runs() != 1 || len(rest) != 0 {
				t.Fatalf("calls=%d runs=%d rest=%v, want Jev asked and the email run", srv.Calls(), bm.runs(), rest)
			}
		})
	}
}

// The budget is what stops the question: with room left, the same poll asks.
func TestBillGateAsksWithinBudget(t *testing.T) {
	bm, srv := gateMail(t)
	if err := bm.td.UpdateConfig(func(c *config.Config) {
		c.Usage.MonthlyBudget = 1
		c.Usage.Prices = map[string]config.ModelPrice{"fake": {Input: 1}}
	}); err != nil {
		t.Fatal(err)
	}
	bm.claim(mailItems(receiptLine))
	if srv.Calls() != 1 {
		t.Fatalf("%d calls within budget, want 1", srv.Calls())
	}
}

// Through the watcher: a receipt Jev is sure about reaches the ordinary
// watcher, and no bills run reads it, then or on the next poll.
func TestBillGateReceiptReachesTheWatcher(t *testing.T) {
	bm, srv := gateMail(t)
	srv.Choose("m0", "paid_or_info", 0.97, 0.9)
	bm.arrive("7", receiptLine)
	watched, billed, _ := bm.counts()
	if srv.Calls() != 1 || billed != 0 || watched != 1 {
		t.Fatalf("calls=%d billed=%d watched=%d", srv.Calls(), billed, watched)
	}
	bm.td.watcher.Poll(context.Background())
	if watched2, billed2, _ := bm.counts(); srv.Calls() != 1 || billed2 != 0 || watched2 != watched {
		t.Fatalf("next poll: calls=%d billed=%d watched=%d", srv.Calls(), billed2, watched2)
	}
}
