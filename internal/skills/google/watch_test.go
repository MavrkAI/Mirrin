package google

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/skills/google/googletest"
	"github.com/MavrkAI/Mirrin/internal/watch"
)

// watching runs a real watcher over the Gmail source and records every
// turn the model would be asked to take.
func watching(t *testing.T, a *Auth) (poll func(), tasks func() []string, store *memory.Store) {
	t.Helper()
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var mu sync.Mutex
	var got []string
	w := watch.New(store, []watch.Source{a.GmailWatch()},
		func(_ context.Context, _, task string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, task)
			return "NOTHING_TO_REPORT", nil
		},
		func(context.Context, string, string) error { return nil },
		func() string { return "test:owner" }, time.Minute, nil, nil)
	return func() { w.Poll(context.Background()) }, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}, store
}

func unread(i int) googletest.Message {
	id := fmt.Sprintf("u%02d", i)
	return googletest.Message{ID: id, ThreadID: id, Labels: []string{"INBOX", "UNREAD"},
		Payload: googletest.Headers(googletest.Part("text/plain", "", "", []byte("hi"), 0), "From", fmt.Sprintf("Sender %d <s%d@example.com>", i, i), "Subject", fmt.Sprintf("Message %d", i))}
}

// Most inboxes have more unread mail than the watcher lists. Reading one of
// the newest brings the 51st into view; that weeks-old email is not news,
// and the model must not be woken for it.
func TestReadingMailDoesNotMakeOldMailNews(t *testing.T) {
	a, fake := connected(t)
	for i := 1; i <= watchMax+1; i++ {
		fake.AddMessage(unread(i))
	}
	poll, tasks, store := watching(t, a)
	poll() // baseline: the newest 50
	fake.MarkRead(fmt.Sprintf("u%02d", watchMax+1))
	poll()
	if got := tasks(); len(got) != 0 {
		t.Fatalf("an old email coming into view woke the model: %v", got)
	}
	if audit, _ := store.RecentAuditOfKind(context.Background(), "watch.change", 5); len(audit) != 0 {
		t.Fatalf("old mail recorded as a change: %+v", audit)
	}

	fake.AddMessage(unread(99))
	poll()
	if got := tasks(); len(got) != 1 || !strings.Contains(got[0], "NEW: unread from Sender 99: Message 99") || strings.Contains(got[0], "Message 1\n") {
		t.Fatalf("new mail not reported alone: %v", got)
	}
}

// A message deleted between being listed and being fetched is skipped; the
// rest of the inbox is still seen, and nothing is fetched twice.
func TestGmailWatchSkipsAMessageThatVanished(t *testing.T) {
	a, fake := connected(t)
	fake.AddMessage(unread(1))
	gone := unread(2)
	gone.GetStatus = 404
	fake.AddMessage(gone)
	w := a.GmailWatch()
	snap, err := w.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("one vanished message failed the whole inbox: %v", err)
	}
	if len(snap) != 1 || snap["u01"] == "" {
		t.Fatalf("snapshot: %v", snap)
	}

	// Another failure (Google having a bad minute) fails this look, but
	// what was already fetched isn't fetched again.
	fake.AddMessage(unread(3))
	broken := unread(4)
	broken.GetStatus = 500
	fake.AddMessage(broken)
	if _, err := w.Snapshot(context.Background()); err == nil {
		t.Fatal("a failed fetch was taken for an empty inbox")
	}
	fake.Delete("u04")
	snap, err = w.Snapshot(context.Background())
	if err != nil || len(snap) != 2 {
		t.Fatalf("after Google recovered: %v %v", snap, err)
	}
	if n := fake.Calls("/gmail/v1/users/me/messages/u01"); n != 1 {
		t.Fatalf("a message already seen was fetched %d times", n)
	}
	if n := fake.Calls("/gmail/v1/users/me/messages/u03"); n != 1 {
		t.Fatalf("a new message was fetched %d times", n)
	}
}
