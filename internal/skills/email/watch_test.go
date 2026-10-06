package email

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/MavrkAI/Mirrin/internal/memory"
	"github.com/MavrkAI/Mirrin/internal/watch"
)

func plainMail(i int) string {
	return fmt.Sprintf("From: Sender %d <s%d@example.com>\r\nTo: sam@example.com\r\nSubject: Message %d\r\nDate: Mon, 03 Oct 2026 10:00:00 +1100\r\nMessage-ID: <m%d@example.com>\r\n\r\nhi\r\n", i, i, i, i)
}

// Most inboxes have more unread mail than the watcher lists (the newest
// 100). Reading one of the newest brings an older one into view; that is
// not news, and the model must not be woken for it.
func TestReadingMailDoesNotMakeOldMailNews(t *testing.T) {
	var msgs []string
	for i := 1; i <= snapshotMax+1; i++ {
		msgs = append(msgs, plainMail(i))
	}
	m := newMailbox(t, msgs...)
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var mu sync.Mutex
	var tasks []string
	w := watch.New(store, []watch.Source{m.client},
		func(_ context.Context, _, task string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			tasks = append(tasks, task)
			return "NOTHING_TO_REPORT", nil
		},
		func(context.Context, string, string) error { return nil },
		func() string { return "test:owner" }, time.Minute, nil, nil)
	ctx := context.Background()

	w.Poll(ctx) // baseline: uids 2…101
	if err := m.client.MarkRead(uint32(snapshotMax + 1)); err != nil {
		t.Fatal(err)
	}
	w.Poll(ctx) // uid 1 comes into view
	if len(tasks) != 0 {
		t.Fatalf("an old email coming into view woke the model: %v", tasks)
	}
	if audit, _ := store.RecentAuditOfKind(ctx, "watch.change", 5); len(audit) != 0 {
		t.Fatalf("old mail recorded as a change: %+v", audit)
	}

	if _, err := m.user.Append("INBOX", bytes.NewReader([]byte(plainMail(500))), &imap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w.Poll(ctx)
	if len(tasks) != 1 || !strings.Contains(tasks[0], "NEW: unread from Sender 500: Message 500") || strings.Contains(tasks[0], "Message 1\n") {
		t.Fatalf("new mail not reported alone: %v", tasks)
	}
}
