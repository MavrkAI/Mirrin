// Package cloud is the daemon's side of Mirrin Cloud, the optional paid
// availability layer: linking, the entitlement and its daily refresh, and
// the egress ledger. It is inert until the user runs `mirrin cloud link`. A
// machine that never linked has no data/cloud directory, and nothing here
// contacts anything for it.
//
// Only cmd/mirrin, internal/daemon and internal/reach may import this
// package (boundary_test.go), so no core feature can ask whether the user
// pays. docs/cloud-api.md is the wire contract; cloudtest holds a fake
// control plane and the contract suite a real one must pass.
package cloud

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/MavrkAI/Mirrin/internal/entitle"
	"github.com/MavrkAI/Mirrin/internal/httpsig"
)

// The hosted service's names. Nothing is deployed under them yet, and
// mirrin.link must be registered before Cloud opens.
const (
	// DefaultAPI is the control plane `mirrin cloud link` uses when the
	// config names no other.
	DefaultAPI = "https://cloud.mirrin.app"
	// OperatorZone holds the control plane and the relays. The egress
	// guard counts the apex as Cloud too, so nothing the free twin
	// fetches (install scripts, the registry, updates, docs) may ever be
	// served from mirrin.app or a name under it.
	OperatorZone = "mirrin.app"
	// TenantZone holds the users' handles. It is a separate registrable
	// domain, so a handle can never set cookies for the operator zone.
	TenantZone = "mirrin.link"
)

// Size limits on what the control plane sends back.
const (
	maxReply   = 64 << 10 // any answer but /v1/me
	maxMeReply = 1 << 20
	// requestTimeout bounds one request, answer included.
	requestTimeout = 30 * time.Second
)

var (
	// ErrNotLinked means this machine has no device key or link: run
	// `mirrin cloud link`.
	ErrNotLinked = errors.New("cloud: this machine is not linked (run mirrin cloud link)")
	// ErrLapsed is a refresh refused with 402 lapsed: payment has lapsed.
	// The stored entitlement stays good until it expires.
	ErrLapsed = errors.New("cloud: payment has lapsed")
	// ErrRevoked is 403 revoked or deleted: the control plane no longer
	// accepts this machine's key.
	ErrRevoked = errors.New("cloud: this machine's link was revoked")
	// ErrUnauthorized is 401: the control plane did not accept the request
	// signature.
	ErrUnauthorized = errors.New("cloud: request signature refused")
	// ErrWrongKey means an entitlement is bound to a key other than this
	// machine's. It is never stored.
	ErrWrongKey = errors.New("cloud: entitlement is bound to another device key")
)

// APIError is an answer outside 2xx: the status and the control plane's
// {"error","message"}. errors.Is matches 401 to ErrUnauthorized, 402 lapsed
// to ErrLapsed, and 403 revoked or deleted to ErrRevoked. A 402 or 403
// without its code, such as a proxy's HTML page, matches neither: it says
// nothing about this machine.
type APIError struct {
	Status  int
	Code    string // such as "lapsed", "handle_taken", "replay"
	Message string // a sentence for the user
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("cloud: HTTP %d", e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Is maps the statuses that carry a meaning to their errors.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized
	case ErrLapsed:
		return e.Status == http.StatusPaymentRequired && e.Code == "lapsed"
	case ErrRevoked:
		return e.Status == http.StatusForbidden && (e.Code == "revoked" || e.Code == "deleted")
	}
	return false
}

// SupersededError is a refresh refused with 409 superseded: another machine
// holds the handle at generation Gen since At. This one stands by.
type SupersededError struct {
	Gen int64
	At  time.Time
}

func (e *SupersededError) Error() string {
	return fmt.Sprintf("cloud: another machine took over this handle on %s (generation %d)", e.At.UTC().Format(time.DateOnly), e.Gen)
}

// Client talks to one control plane for this machine. Every request is
// signed with the device key (RFC 9421), goes to that one origin, follows
// no redirect, and is written to the egress ledger.
type Client struct {
	// HTTP sends the requests. Its Transport is nil, so requests go through
	// http.DefaultTransport, where the egress test watches for them.
	HTTP *http.Client
	// Now is the clock for signatures and entitlement checks.
	Now func() time.Time

	api     string // the control plane's origin
	dataDir string
	keys    map[string]ed25519.PublicKey
	state   *State
	keyMu   sync.Mutex // creating the device key
}

// New returns a client for the control plane at base, an origin such as
// https://cloud.mirrin.app (plain http only for a loopback address, as in
// tests). keys verifies entitlements; nil means the keys compiled into this
// build. New creates and contacts nothing: the device key is made by
// StartLink, in dataDir/cloud/device.key.
func New(dataDir, base string, keys map[string]ed25519.PublicKey) (*Client, error) {
	api, err := parseAPI(base)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		keys = entitle.EntitlementKeys
	}
	st, err := openState(dataDir, keys)
	if err != nil {
		return nil, err
	}
	return &Client{
		HTTP: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Now:     time.Now,
		api:     api,
		dataDir: dataDir,
		keys:    keys,
		state:   st,
	}, nil
}

// OpenLinked returns a client for the control plane this machine linked
// with, or nil and no error when it never linked. It reads one file and
// contacts nothing; the daemon calls it once at start.
func OpenLinked(dataDir string) (*Client, error) {
	st, err := OpenState(dataDir)
	if err != nil {
		return nil, err
	}
	rec, err := st.record()
	if err != nil || rec == nil {
		return nil, err
	}
	return New(dataDir, rec.API, nil)
}

// API is the control plane's origin.
func (c *Client) API() string { return c.api }

// State is this machine's link state, verified with the client's keys.
func (c *Client) State() *State { return c.state }

// PublicKey is the device key's public half. It fails with ErrNotLinked
// before StartLink has made the key.
func (c *Client) PublicKey() (ed25519.PublicKey, error) {
	priv, err := loadKey(c.state.dir)
	if err != nil {
		return nil, err
	}
	return priv.Public().(ed25519.PublicKey), nil
}

// deviceKey loads the device key, making it first when create is set.
func (c *Client) deviceKey(create bool) (ed25519.PrivateKey, error) {
	if !create {
		return loadKey(c.state.dir)
	}
	c.keyMu.Lock()
	defer c.keyMu.Unlock()
	return createKey(c.dataDir)
}

// reply is one answer: its status and body.
type reply struct {
	status int
	body   []byte
}

// call sends one signed request with in (if not nil) as its JSON body. The
// ledger is opened before anything is sent and the entry written once the
// answer is read, whatever it was. An answer over limit is an error.
func (c *Client) call(ctx context.Context, method, path string, in any, limit int64, createKey bool) (reply, error) {
	key, err := c.deviceKey(createKey)
	if err != nil {
		return reply{}, err
	}
	var body []byte
	var rd io.Reader
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return reply{}, err
		}
		rd = bytes.NewReader(body)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, rd)
	if err != nil {
		return reply{}, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mirrin")
	if err := httpsig.Sign(req, httpsig.KeyID(key.Public().(ed25519.PublicKey)), key, c.Now()); err != nil {
		return reply{}, err
	}

	led, err := openLedger(c.dataDir)
	if err != nil {
		return reply{}, err
	}
	defer led.Close()
	e := Entry{At: c.Now(), Method: method, Host: req.URL.Host, Path: req.URL.EscapedPath(), ReqBytes: int64(len(body))}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if lerr := writeEntry(led, e); lerr != nil {
			return reply{}, errors.Join(err, lerr)
		}
		return reply{}, fmt.Errorf("cloud: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	e.Status, e.RespBytes = resp.StatusCode, int64(len(b))
	if err := writeEntry(led, e); err != nil {
		return reply{}, err
	}
	if rerr != nil {
		return reply{}, fmt.Errorf("cloud: %s %s: %w", method, path, rerr)
	}
	if int64(len(b)) > limit {
		return reply{}, fmt.Errorf("cloud: %s %s: answer larger than %d bytes", method, path, limit)
	}
	return reply{status: resp.StatusCode, body: b}, nil
}

// decode reads a 2xx JSON answer into out. Unknown members are ignored, so
// the control plane can add fields; duplicate names, invalid UTF-8 and
// trailing data are refused.
func decode(r reply, out any) error {
	if r.status < 200 || r.status > 299 {
		return apiError(r)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.body, out); err != nil {
		return fmt.Errorf("cloud: malformed answer: %w", err)
	}
	return nil
}

// apiError builds the error for an answer outside 2xx. A body that is not
// the documented {"error","message"} still gives the status.
func apiError(r reply) error {
	var b struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(r.body, &b)
	return &APIError{Status: r.status, Code: clip(b.Error, 64), Message: clip(b.Message, 300)}
}

// clip bounds text from the control plane before it is shown, and drops
// every rune that is not graphic: C0 and C1 controls (U+009B is a terminal's
// CSI), format characters such as bidi overrides, line separators and
// private-use code points.
func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if !unicode.IsGraphic(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}

// parseAPI checks a control plane origin: https://host[:port] and nothing
// else, or http:// for a loopback host. It returns the origin lowercased,
// without a default port.
func parseAPI(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(s, "#") {
		return "", fmt.Errorf("cloud: API %q is not https://host[:port]", s)
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	switch scheme {
	case "https":
		host = strings.TrimSuffix(host, ":443")
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", fmt.Errorf("cloud: API %q must use https (plain http only for a loopback address)", s)
		}
		host = strings.TrimSuffix(host, ":80")
	default:
		return "", fmt.Errorf("cloud: API %q is not https://host[:port]", s)
	}
	return scheme + "://" + host, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
