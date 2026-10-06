package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Enabled    *bool    `yaml:"enabled,omitempty"`
	Subject    string   `yaml:"subject,omitempty"`
	Preview    string   `yaml:"preview,omitempty"`
	QuietHours string   `yaml:"quiet_hours,omitempty"` // local HH:MM-HH:MM
	Kinds      []string `yaml:"kinds,omitempty"`
}

func (c Config) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }
func (c Config) permits(kind string, now time.Time) bool {
	if !c.IsEnabled() {
		return false
	}
	if kind == "resolved" || kind == "security" || kind == "test" {
		return true
	}
	if len(c.Kinds) > 0 {
		found := false
		for _, k := range c.Kinds {
			if k == kind {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	// An approval, or a page handed over in the browser, is something the
	// owner is waiting on now: quiet hours don't hold it back.
	if kind == "approval" || kind == "handover" {
		return true
	}
	return !InQuiet(c.QuietHours, now)
}

// InQuiet reports whether now's clock time falls in spec, quiet hours as
// local HH:MM-HH:MM that may run past midnight. A spec that can't be read,
// or one that starts and ends at the same time, is never quiet.
func InQuiet(spec string, now time.Time) bool {
	from, to, ok := strings.Cut(spec, "-")
	if !ok {
		return false
	}
	a, e1 := time.Parse("15:04", from)
	b, e2 := time.Parse("15:04", to)
	if e1 != nil || e2 != nil {
		return false
	}
	n := now.Hour()*60 + now.Minute()
	start := a.Hour()*60 + a.Minute()
	end := b.Hour()*60 + b.Minute()
	if start < end {
		return n >= start && n < end
	}
	if start > end {
		return n >= start || n < end
	}
	return false
}

type Notification struct {
	Kind                  string
	ID                    int64
	Title, Body, URL, Tag string
	Origin                string
	Badge                 int
}

func (n Notification) Payload(name, preview string, now time.Time) ([]byte, error) {
	preview = normalizedPreview(preview)
	body := n.Body
	switch n.Kind {
	case "resolved":
		n.Title, body, n.URL = name+": handled", "This request no longer needs you.", "/ui"
	case "test":
		n.Title, body = name, "Notifications are ready."
	default:
		if preview == "private" {
			body = name + " needs you"
			n.Title = body
		} else if preview != "full" {
			r := []rune(body)
			if len(r) > 60 {
				body = string(r[:60])
			}
		}
	}

	p := map[string]any{"v": 1, "k": n.Kind, "id": n.ID, "t": n.Title, "b": body, "u": n.URL, "tag": n.Tag, "badge": max(0, n.Badge), "ts": now.Unix()}
	// Declarative clients replace the original tagged notification with its
	// resolved state; legacy workers show a quiet replacement.
	p["web_push"] = 8030
	p["app_badge"] = max(0, n.Badge)
	p["notification"] = map[string]any{"title": n.Title, "body": body, "navigate": n.Origin + n.URL, "tag": n.Tag, "app_badge": max(0, n.Badge)}
	// Bound the encoded size, including escaping and both body copies.
	runes := []rune(body)
	for {
		p["b"] = body
		p["notification"].(map[string]any)["body"] = body
		b, err := json.Marshal(p)
		if err != nil || len(b) <= 3993 {
			return b, err
		}
		if len(runes) == 0 {
			return nil, errors.New("notification metadata is too long")
		}
		runes = runes[:len(runes)-1]
		body = string(runes) + "…"
	}

}

type delivery struct {
	sub      Subscription
	n        Notification
	due      time.Time
	attempts int
}
type Dispatcher struct {
	Log               *slog.Logger
	configMu          sync.Mutex
	lastConfigWarning string
	Store             *Store
	Send              func(context.Context, Subscription, []byte, string, string) error
	// Active is checked again at delivery, so revoke wins over queued work.
	Active func(string) bool
	// Approves says whether a device may act for the owner (Approve scope).
	// Only such a device hears of an approval, a task's question or a
	// hand-over: a wall screen paired only to look has nothing to do with
	// one, and its lock screen is for whoever walks past.
	Approves func(string) bool
	Settings func() (string, Config)
	mu       sync.Mutex
	pending  map[string]*delivery
	wake     chan struct{}
}

func NewDispatcher(store *Store, send func(context.Context, Subscription, []byte, string, string) error) *Dispatcher {
	return &Dispatcher{Log: slog.Default(), Store: store, Send: send, pending: map[string]*delivery{}, wake: make(chan struct{}, 1)}
}

// Enqueue queues n for every subscribed device but exclude, and says how
// many it was queued for: none when the settings hold it back.
func (d *Dispatcher) Enqueue(n Notification, exclude string) int { return d.enqueue(n, exclude, "") }
func (d *Dispatcher) Test(device string, name string) {
	d.enqueue(Notification{Kind: "test", Title: name, Body: "Notifications are ready.", URL: "/ui", Tag: "test"}, "", device)
}
func (d *Dispatcher) enqueue(n Notification, exclude, only string) int {
	_, c := d.settings()
	if !c.permits(n.Kind, time.Now()) {
		d.warn("push filtered", "kind", n.Kind)
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if n.Kind == "resolved" {
		for key, item := range d.pending {
			if item.n.Tag == n.Tag {
				delete(d.pending, key)
			}
		}
	}
	queued := 0
	for _, s := range d.Store.List() {
		if s.DeviceID == exclude || only != "" && s.DeviceID != only || !d.reaches(s.DeviceID, n.Kind) {
			continue
		}
		key := s.Endpoint + "\x00" + n.Tag
		if len(d.pending) >= 256 && d.pending[key] == nil {
			d.warn("push dropped: queue full", "kind", n.Kind)
			continue
		}
		d.pending[key] = &delivery{sub: s, n: n, due: time.Now()}
		queued++
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
	return queued
}

// reaches says whether a device may be sent this kind of notification. A
// security alarm and the test go to every subscribed device; everything
// else says what the owner is being asked, so it goes only to a device that
// can act for them, including kinds added later.
func (d *Dispatcher) reaches(device, kind string) bool {
	switch kind {
	case "security", "test":
		return true
	}
	return d.Approves != nil && d.Approves(device)
}
func (d *Dispatcher) settings() (string, Config) {
	if d.Settings != nil {
		name, c := d.Settings()
		d.validateConfig(&c)
		return name, c
	}
	return "Your twin", Config{}
}
func (d *Dispatcher) Run(ctx context.Context) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-d.wake:
		}
		for {
			key, item := d.next()
			if item == nil {
				break
			}
			err := d.deliver(ctx, item)
			d.finish(key, item, err)
			if ctx.Err() != nil {
				return
			}
		}
	}
}
func (d *Dispatcher) next() (string, *delivery) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range d.pending {
		if !v.due.After(time.Now()) {
			return k, v
		}
	}
	return "", nil
}
func (d *Dispatcher) deliver(ctx context.Context, item *delivery) error {
	if !d.Store.contains(item.sub) || d.Active != nil && !d.Active(item.sub.DeviceID) || !d.reaches(item.sub.DeviceID, item.n.Kind) {
		d.warn("push dropped: subscription inactive", "kind", item.n.Kind)
		return nil
	}
	name, c := d.settings()
	if !c.permits(item.n.Kind, time.Now()) {
		d.warn("push filtered", "kind", item.n.Kind)
		return nil
	}
	n := item.n
	n.Origin = item.sub.Origin
	b, err := n.Payload(name, c.Preview, time.Now())
	if err != nil {
		d.warn("push payload could not be encoded", "kind", n.Kind)
		return err
	}
	err = d.Send(ctx, item.sub, b, item.n.Kind, item.n.Tag)
	if errors.Is(err, ErrGone) {
		d.warn("push subscription removed: expired")
		return d.Store.Delete(item.sub.DeviceID, item.sub.Endpoint)
	}
	return err
}
func (d *Dispatcher) finish(key string, item *delivery, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pending[key] != item {
		return
	}
	if err == nil || item.attempts >= 5 {
		if err != nil {
			d.warn("push delivery failed: retries exhausted", "kind", item.n.Kind, "error", err.Error())
		}
		delete(d.pending, key)
		return
	}
	waits := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 45 * time.Minute}
	wait := waits[item.attempts]
	var retry *RetryError
	if errors.As(err, &retry) {
		wait = max(wait, retry.After)
	}
	d.warn("push delivery will retry", "kind", item.n.Kind, "error", err.Error(), "delay", wait)
	item.attempts++
	item.due = time.Now().Add(wait)
}
func Approval(id int64, summary, status, name string, badge int) Notification {
	kind, title := "approval", name+" needs a yes"
	if status != "pending" {
		kind = "resolved"
		title = name + ": handled"
	}
	return Notification{Kind: kind, ID: id, Title: title, Body: summary, URL: fmt.Sprintf("/approve/%d", id), Tag: fmt.Sprintf("approval-%d", id), Badge: badge}
}

func normalizedPreview(preview string) string {
	switch strings.ToLower(strings.TrimSpace(preview)) {
	case "", "brief":
		return "brief"
	case "full":
		return "full"
	default:
		return "private"
	}
}

func (d *Dispatcher) warn(msg string, args ...any) {
	if d.Log != nil {
		d.Log.Warn(msg, args...)
	}
}

// Validate at the settings boundary, including live config changes. Unknown
// preview values fail closed; warnings never include owner-provided values.
func (d *Dispatcher) validateConfig(c *Config) {
	preview := strings.ToLower(strings.TrimSpace(c.Preview))
	warning := ""
	if preview != "" && preview != "brief" && preview != "full" && preview != "private" {
		warning = "unknown push preview; using private"
	}
	if c.QuietHours != "" {
		from, to, ok := strings.Cut(c.QuietHours, "-")
		_, a := time.Parse("15:04", from)
		_, b := time.Parse("15:04", to)
		if !ok || a != nil || b != nil {
			warning += "; invalid push quiet_hours; use HH:MM-HH:MM"
		}
	}
	c.Preview = normalizedPreview(preview)
	d.configMu.Lock()
	defer d.configMu.Unlock()
	if warning != "" && warning != d.lastConfigWarning {
		d.warn(warning)
	}
	d.lastConfigWarning = warning
}
