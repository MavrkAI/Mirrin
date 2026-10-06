package google

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"sync"

	"google.golang.org/api/googleapi"
)

// watchQuery is the mail worth a look: unread, in the inbox, not in the
// Promotions or Social tabs.
const watchQuery = "in:inbox is:unread -category:promotions -category:social"

// watchMax caps how many unread messages are tracked.
const watchMax = 50

// GmailWatch is the Gmail inbox as a watch source: unread mail, keyed by
// message id, so the twin notices "the landlord replied" without being asked.
// Each message's sender, subject and arrival time are fetched once and
// remembered.
type GmailWatch struct {
	a    *Auth
	mu   sync.Mutex
	seen map[string]seenMail // by message id
}

type seenMail struct {
	desc     string
	received int64 // when Gmail received it (ms since 1970)
}

// GmailWatch returns the inbox watch source for this account.
func (a *Auth) GmailWatch() *GmailWatch {
	a.tmu.Lock()
	defer a.tmu.Unlock()
	if a.gw == nil {
		a.gw = &GmailWatch{a: a, seen: map[string]seenMail{}}
	}
	return a.gw
}

// Name identifies the watch source.
func (g *GmailWatch) Name() string { return "gmail" }

// Arrival places a message by when Gmail received it, so an older unread
// message that only now comes into view (one of the newest was read) isn't
// taken for new mail.
func (g *GmailWatch) Arrival(id string) (uint64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, ok := g.seen[id]
	if !ok || m.received <= 0 {
		return 0, false
	}
	return uint64(m.received), true
}

// Snapshot lists unread inbox mail as "unread from Sarah Smith: Lunch?".
func (g *GmailWatch) Snapshot(ctx context.Context) (map[string]string, error) {
	svc, err := g.a.gmail(ctx)
	if err != nil {
		return nil, err
	}
	list, err := svc.Users.Messages.List("me").Q(watchQuery).MaxResults(watchMax).Context(ctx).Do()
	if err != nil {
		return nil, g.a.Explain("gmail", err)
	}
	g.mu.Lock()
	known := make(map[string]seenMail, len(g.seen))
	for k, v := range g.seen {
		known[k] = v
	}
	g.mu.Unlock()
	now := map[string]seenMail{}
	var failed error
	for _, m := range list.Messages {
		if s, ok := known[m.Id]; ok {
			now[m.Id] = s
			continue
		}
		full, err := svc.Users.Messages.Get("me", m.Id).Format("metadata").MetadataHeaders("From", "Subject").Context(ctx).Do()
		if err != nil {
			var ge *googleapi.Error
			if errors.As(err, &ge) && ge.Code == 404 {
				continue // deleted since it was listed
			}
			failed = g.a.Explain("gmail", err)
			break
		}
		subject := strings.TrimSpace(header(full, "Subject"))
		if subject == "" {
			subject = "(no subject)"
		}
		now[m.Id] = seenMail{desc: fmt.Sprintf("unread from %s: %s", senderName(header(full, "From")), subject), received: full.InternalDate}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if failed != nil {
		// Keep what was fetched, so the next try doesn't ask for it again.
		for k, v := range now {
			g.seen[k] = v
		}
		return nil, failed
	}
	g.seen = now
	res := make(map[string]string, len(now))
	for k, v := range now {
		res[k] = v.desc
	}
	return res, nil
}

// senderName is how a person refers to a sender: their name, else their address.
func senderName(from string) string {
	if a, err := mail.ParseAddress(from); err == nil {
		if a.Name != "" {
			return a.Name
		}
		return a.Address
	}
	if from = strings.TrimSpace(from); from != "" {
		return from
	}
	return "?"
}
