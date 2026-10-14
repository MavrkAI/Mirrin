package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/llm"
	gauth "github.com/MavrkAI/Mirrin/internal/skills/google"
	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
)

// The first hello's inbox line names the newest unread email from a
// person: never a no-reply, never one already read, and a health, money or
// relationship matter only by its kind. It says how long it has waited and
// how many more people are waiting; with no one waiting it says so, but
// only when it saw every unread email (complete): a full window of ten
// says "at least", and with no person among a full window says nothing.
func TestWaitingMailNamesTheNewestUnreadFromAPerson(t *testing.T) {
	loc := time.FixedZone("AEST", 10*3600)
	now := time.Date(2026, 10, 14, 18, 0, 0, 0, loc)
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC1123Z) }
	unread := func(from, subject, date string) gauth.InboxMail {
		return gauth.InboxMail{ID: subject, From: from, Subject: subject, Date: date, Snippet: "…", Unread: true}
	}
	priya := unread("Priya Sharma <priya@example.com>", "Dinner Saturday?", ago(2*time.Hour))
	rao := unread("Dr Rao <rao@clinic.example>", "Your test results", ago(10*time.Minute))
	github := unread("GitHub <noreply@github.com>", "[repo] CI failed", ago(5*time.Minute))
	apple := unread("Apple <no_reply@email.apple.com>", "Your receipt", ago(5*time.Minute))
	sam := unread("Sam <sam@example.com>", "Lunch?", ago(30*time.Hour))
	read := priya
	read.Unread = false

	for name, c := range map[string]struct {
		mails    []gauth.InboxMail // newest first, as Gmail lists them
		complete bool              // fewer than the window: every unread email was seen
		want     string
	}{
		"nothing":                     {nil, true, "Inbox: nothing unread from a person."},
		"only read":                   {[]gauth.InboxMail{read}, true, "Inbox: nothing unread from a person."},
		"only no-reply":               {[]gauth.InboxMail{github}, true, "Inbox: nothing unread from a person."},
		"only an underscore no-reply": {[]gauth.InboxMail{apple}, true, "Inbox: nothing unread from a person."},
		"a full window with no person says nothing": {[]gauth.InboxMail{github, apple}, false, ""},
		"one person": {[]gauth.InboxMail{priya}, true, "Waiting in the inbox: an unread email from Priya Sharma, “Dinner Saturday?”, 2 hours ago."},
		"no-reply skipped, newest person named": {[]gauth.InboxMail{github, priya, sam}, true,
			"Waiting in the inbox: an unread email from Priya Sharma, “Dinner Saturday?”, 2 hours ago, and 1 more unread from people."},
		"a full window with one person says just that": {[]gauth.InboxMail{github, apple, priya}, false,
			"Waiting in the inbox: an unread email from Priya Sharma, “Dinner Saturday?”, 2 hours ago."},
		"a full window counts at least": {[]gauth.InboxMail{github, priya, sam}, false,
			"Waiting in the inbox: an unread email from Priya Sharma, “Dinner Saturday?”, 2 hours ago, and at least 1 more unread from people."},
		"a health matter waits behind a plain one, and still counts": {[]gauth.InboxMail{rao, priya}, true,
			"Waiting in the inbox: an unread email from Priya Sharma, “Dinner Saturday?”, 2 hours ago, and 1 more unread from people."},
		"only a health matter, named by its kind": {[]gauth.InboxMail{rao}, true,
			"Waiting in the inbox: an unread email from Dr Rao about a health matter (details left out), 10 minutes ago."},
		"yesterday": {[]gauth.InboxMail{sam}, true, "Waiting in the inbox: an unread email from Sam, “Lunch?”, yesterday."},
		"two calendar days back is not yesterday": {[]gauth.InboxMail{unread("Sam <sam@example.com>", "Lunch?", ago(47*time.Hour))}, true,
			"Waiting in the inbox: an unread email from Sam, “Lunch?”, 2 days ago."},
		"an hour ago": {[]gauth.InboxMail{unread("Sam <sam@example.com>", "Lunch?", ago(time.Hour))}, true,
			"Waiting in the inbox: an unread email from Sam, “Lunch?”, an hour ago."},
		"a minute ago": {[]gauth.InboxMail{unread("Sam <sam@example.com>", "Lunch?", ago(90*time.Second))}, true,
			"Waiting in the inbox: an unread email from Sam, “Lunch?”, a minute ago."},
		"just now": {[]gauth.InboxMail{unread("Sam <sam@example.com>", "Lunch?", ago(20*time.Second))}, true,
			"Waiting in the inbox: an unread email from Sam, “Lunch?”, just now."},
		"days ago": {[]gauth.InboxMail{unread("Sam <sam@example.com>", "Lunch?", ago(3*24*time.Hour))}, true,
			"Waiting in the inbox: an unread email from Sam, “Lunch?”, 3 days ago."},
		"no date, no subject": {[]gauth.InboxMail{unread("sam@example.com", "", "not a date")}, true,
			"Waiting in the inbox: an unread email from sam@example.com, (no subject)."},
		"a long subject is cut at eighty characters": {[]gauth.InboxMail{unread("Sam <sam@example.com>", "Re: the plan for the offsite in November and everything we still need to decide about it before Friday", ago(time.Hour))}, true,
			"Waiting in the inbox: an unread email from Sam, “Re: the plan for the offsite in November and everything we still need to decide …”, an hour ago."},
	} {
		if got := waitingMail(c.mails, c.complete, now); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}

// addDatedMail is addMail with a Date header and Gmail's received time, so
// the hello can say how long the email has waited and the fake lists it in
// order. read makes it an email the owner has already opened.
func addDatedMail(fake *googletest.Fake, id, from, subject, snippet string, date time.Time, read bool) {
	labels := []string{"INBOX", "UNREAD"}
	if read {
		labels = []string{"INBOX"}
	}
	fake.AddMessage(googletest.Message{ID: id, ThreadID: "t-" + id, Labels: labels, Snippet: snippet, Received: date.UnixMilli(),
		Payload: googletest.Headers(googletest.Part("text/plain", "text/plain", "", []byte(snippet), 0),
			"From", from, "Subject", subject, "Date", date.Format(time.RFC1123Z), "Message-ID", "<"+id+"@example.com>")})
}

// With Google connected before the hello, the first hello is handed what
// is waiting in the inbox: the person, never the no-reply, and a health
// matter only as that, after the calendar and before the weather; the page
// says it is looking at the inbox. A dozen newer emails the owner has read
// don't hide the one person still waiting.
func TestHelloNamesTheEmailWaiting(t *testing.T) {
	helloWeather(t) // never the real Open-Meteo
	td, fake := googleDaemon(t)
	var asked []llm.Request
	td.llm.brain = func(_ string, req llm.Request) llm.Response {
		asked = append(asked, req)
		return say("Evening, Akshay. Priya's asking about Saturday. Type to me whenever you like.")
	}
	connectWithoutWaiting(t, td)
	// The weather is for the zone's city, and UTC, as on CI, has none. Saved,
	// not only set in memory: SetNames below starts from the file the connect
	// just wrote.
	if err := td.UpdateConfig(func(c *config.Config) { c.User.Timezone = "Australia/Melbourne" }); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := range 12 {
		addDatedMail(fake, fmt.Sprintf("read%d", i), "Colleague <colleague@example.com>", fmt.Sprintf("Minutes %d", i), "Read already.", now.Add(-time.Duration(i+1)*time.Minute), true)
	}
	addDatedMail(fake, "m1", "GitHub <noreply@github.com>", "[repo] CI failed", "Run 42 failed.", now.Add(-20*time.Minute), false)
	addDatedMail(fake, "m2", "Dr Rao <rao@clinic.example>", "Your test results", "Please call the clinic.", now.Add(-30*time.Minute), false)
	addDatedMail(fake, "m3", "Priya Sharma <priya@example.com>", "Dinner Saturday?", "Are you free on Saturday?", now.Add(-2*time.Hour), false)
	ctx := context.Background()
	if err := td.SetNames(ctx, "Akshay Kumar", "Nyra", "nyra", "name"); err != nil {
		t.Fatal(err)
	}

	var status []string
	if _, err := td.Hello(ctx, func(string) {}, func(s string) { status = append(status, s) }); err != nil {
		t.Fatal(err)
	}
	var hello *llm.Request
	for i := range asked {
		if strings.Contains(asked[i].System, "This is your very first moment") {
			hello = &asked[i]
		}
	}
	if hello == nil {
		t.Fatalf("no hello among %d model calls", len(asked))
	}
	facts := lastUserText(*hello)
	calendar, inbox, weather := strings.Index(facts, "Calendar:"), strings.Index(facts, "Waiting in the inbox:"), strings.Index(facts, "Weather:")
	for what, ok := range map[string]bool{
		"the email waiting":           strings.Contains(facts, "Waiting in the inbox: an unread email from Priya Sharma, “Dinner Saturday?”, 2 hours ago, and 1 more unread from people."),
		"not the no-reply":            !strings.Contains(facts, "GitHub"),
		"the health matter's details": !strings.Contains(facts, "test results") && !strings.Contains(facts, "clinic"),
		"calendar, inbox, weather":    calendar >= 0 && calendar < inbox && inbox < weather,
	} {
		if !ok {
			t.Errorf("hello facts: %s\n%s", what, facts)
		}
	}
	if want := lookingAt(dayPart(now.In(td.location())), true); len(status) == 0 || status[0] != want {
		t.Errorf("status %q, want %q", status, want)
	}
}

// Gmail not answering (the API turned off, a timeout) leaves the inbox out
// of the hello altogether: nothing about it is guessed or claimed.
func TestHelloSaysNothingOfAnInboxItCouldNotRead(t *testing.T) {
	helloWeather(t)
	td, fake := googleDaemon(t)
	var asked []llm.Request
	td.llm.brain = func(_ string, req llm.Request) llm.Response {
		asked = append(asked, req)
		return say("Evening, Akshay. Type to me whenever you like.")
	}
	connectWithoutWaiting(t, td)
	addDatedMail(fake, "m1", "Priya Sharma <priya@example.com>", "Dinner Saturday?", "Are you free?", time.Now().Add(-time.Hour), false)
	fake.Disable("gmail")
	ctx := context.Background()
	if err := td.SetNames(ctx, "Akshay Kumar", "Nyra", "nyra", "name"); err != nil {
		t.Fatal(err)
	}
	if _, err := td.Hello(ctx, func(string) {}, nil); err != nil {
		t.Fatal(err)
	}
	facts := lastUserText(asked[len(asked)-1])
	if strings.Contains(strings.ToLower(facts), "inbox") || strings.Contains(facts, "Priya") {
		t.Fatalf("the hello speaks of an inbox it couldn't read:\n%s", facts)
	}
}
