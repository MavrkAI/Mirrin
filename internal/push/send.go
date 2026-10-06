package push

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Resolver func(string) ([]netip.Addr, error)

func Resolve(host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}
func publicIP(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, s := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001::/32", "2002::/16", "64:ff9b::/96"} {
		if netip.MustParsePrefix(s).Contains(a) {
			return false
		}
	}
	return true
}
func AllowedEndpoint(endpoint string, resolve func(string) ([]netip.Addr, error)) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return errors.New("unsupported push endpoint")
	}
	h := strings.ToLower(u.Hostname())
	allowed := h == "web.push.apple.com" || h == "fcm.googleapis.com" || strings.HasSuffix(h, ".push.services.mozilla.com") || strings.HasSuffix(h, ".notify.windows.com")
	if !allowed {
		return errors.New("unsupported push service")
	}
	if resolve == nil {
		resolve = Resolve
	}
	ips, err := resolve(h)
	if err != nil || len(ips) == 0 {
		return errors.New("push service could not be resolved")
	}
	for _, a := range ips {
		if !publicIP(a) {
			return errors.New("push service must have public addresses")
		}
	}
	return nil
}

// NewClient pins each dial to checked public addresses. No proxy or redirects
// can bypass endpoint validation; TLS still verifies the original hostname.
func NewClient(resolve Resolver) *http.Client {
	if resolve == nil {
		resolve = Resolve
	}
	tr := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		var ips []netip.Addr
		err = AllowedEndpoint("https://"+address, func(h string) ([]netip.Addr, error) { var e error; ips, e = resolve(h); return ips, e })
		if err != nil {
			return nil, err
		}
		var last error
		for _, a := range ips {
			c, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(a.String(), port))
			if e == nil {
				return c, nil
			}
			last = e
		}
		return nil, last
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var ErrGone = errors.New("push subscription expired")

type RetryError struct{ After time.Duration }

func (e *RetryError) Error() string             { return "push service asked to retry later" }
func (e *RetryError) RetryAfter() time.Duration { return e.After }

type Sender struct {
	VAPID   *VAPID
	Subject string
	Client  *http.Client
	Resolve Resolver
}

func (s *Sender) Send(ctx context.Context, sub Subscription, payload []byte, kind, topic string) error {
	if err := AllowedEndpoint(sub.Endpoint, s.Resolve); err != nil {
		return err
	}
	body, err := Encrypt(sub, payload)
	if err != nil {
		return err
	}
	auth, err := s.VAPID.Header(sub.Endpoint, s.Subject, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/notification+json")
	ttl := "3600"
	if kind == "approval" || kind == "resolved" {
		ttl = "86400"
	}
	req.Header.Set("TTL", ttl)
	if kind == "approval" || kind == "question" || kind == "security" {
		req.Header.Set("Urgency", "high")
	}
	if topic != "" {
		if len(topic) > 32 || strings.ContainsAny(topic, "\r\n") {
			return errors.New("invalid push topic")
		}
		req.Header.Set("Topic", topic)
	}
	client := s.Client
	if client == nil {
		client = NewClient(s.Resolve)
	}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("push service unavailable")
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode == 404 || res.StatusCode == 410 {
		return ErrGone
	}
	if res.StatusCode == 429 {
		wait := time.Minute
		if n, e := strconv.ParseUint(res.Header.Get("Retry-After"), 10, 32); e == nil {
			wait = time.Duration(n) * time.Second
		} else if at, e := http.ParseTime(res.Header.Get("Retry-After")); e == nil {
			wait = max(0, time.Until(at))
		}
		return &RetryError{wait}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("push service returned %d", res.StatusCode)
	}
	return nil
}
