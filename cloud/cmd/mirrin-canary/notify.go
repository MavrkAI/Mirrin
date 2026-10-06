package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// notifier delivers the watcher's events: the one page to a pager, and
// every event (pages included) by email.
type notifier struct {
	pager PageConfig
	mail  EmailConfig
	http  *http.Client
	// sendMail is smtp.SendMail; tests replace it only to watch.
	sendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
	now      func() time.Time
	host     string
}

func newNotifier(cfg WatchConfig) *notifier {
	host, _ := os.Hostname()
	return &notifier{pager: cfg.Page, mail: cfg.Email, http: &http.Client{Timeout: 15 * time.Second}, sendMail: smtp.SendMail, now: time.Now, host: host}
}

// pagerDutyURL is PagerDuty's Events API v2.
const pagerDutyURL = "https://events.pagerduty.com/v2/enqueue"

// page sends a page trigger or resolve.
func (n *notifier) page(ctx context.Context, e Event) error {
	url := n.pager.URL
	if n.pager.URLEnv != "" {
		url = os.Getenv(n.pager.URLEnv)
	}
	action := "resolve"
	if e.Raised {
		action = "trigger"
	}
	var body any
	switch n.pager.Format {
	case "pagerduty":
		if url == "" {
			url = pagerDutyURL
		}
		key := os.Getenv(n.pager.RoutingKeyEnv)
		if key == "" {
			return fmt.Errorf("page: %s is empty", n.pager.RoutingKeyEnv)
		}
		pd := map[string]any{"routing_key": key, "event_action": action, "dedup_key": e.Key}
		if e.Raised {
			pd["payload"] = map[string]any{"summary": e.Summary, "source": "mirrin-canary@" + n.host, "severity": "critical"}
		}
		body = pd
	case "webhook":
		body = map[string]any{"event": action, "key": e.Key, "summary": e.Summary, "source": "mirrin-canary@" + n.host}
	default:
		return fmt.Errorf("page: unknown format %q", n.pager.Format)
	}
	if url == "" {
		return errors.New("page: no URL")
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := n.http.Do(req)
	if err != nil {
		return shortErr(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("page: the pager answered %d", res.StatusCode)
	}
	return nil
}

// email sends one event. STARTTLS is used whenever the server offers it,
// and a password is only ever sent over TLS (or to loopback).
func (n *notifier) email(_ context.Context, e Event) error {
	m := n.mail
	var auth smtp.Auth
	if m.UsernameEnv != "" {
		host, _, _ := net.SplitHostPort(m.SMTP)
		auth = smtp.PlainAuth("", os.Getenv(m.UsernameEnv), os.Getenv(m.PasswordEnv), host)
	}
	return n.sendMail(m.SMTP, auth, m.From, m.To, n.message(e))
}

func (n *notifier) message(e Event) []byte {
	tag := "resolved"
	switch {
	case e.Page && e.Raised:
		tag = "PAGE"
	case e.Raised:
		tag = "alert"
	}
	subject := fmt.Sprintf("[mirrin ops] %s: %s", tag, clipLine(e.Summary, 120))
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", n.mail.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(n.mail.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", n.now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	fmt.Fprintf(&b, "%s\r\n\r\nalarm: %s\r\nfrom: mirrin-canary on %s\r\nrunbook: cloud/ops/ALERTS.md#%s\r\n", e.Summary, e.Key, n.host, anchor(e.Key))
	return []byte(b.String())
}

// anchor is the ALERTS.md section for an alarm key.
func anchor(key string) string {
	switch {
	case key == pageKey:
		return "page-canary-unreachable-through-every-relay"
	case strings.HasPrefix(key, "relay-"):
		return "one-relay-down"
	case strings.HasPrefix(key, "probe-"):
		return "a-probe-went-quiet"
	case key == "cert":
		return "canary-certificate-not-renewing"
	}
	return "a-check-failed"
}

// clipLine keeps a header to one short line.
func clipLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
