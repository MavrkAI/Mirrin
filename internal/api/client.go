package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/channels/whatsapp"
	"github.com/MavrkAI/Mirrin/internal/devices"
	"github.com/MavrkAI/Mirrin/internal/health"
)

// Client talks to a running daemon.
type Client struct {
	base  string // http://host:port or https://host:port
	token string
	http  *http.Client
}

// Target is a twin to connect to: its address (host:port for plain HTTP, or
// a URL), a key, and for TLS the certificate pins to trust instead of a CA.
type Target struct {
	Address string
	Token   string
	Pins    []string
}

// Connect returns a client for the twin running from this computer's
// dataDir, if one answers, else nil. It uses the loopback address (the
// master key works nowhere else), falling back to listen itself.
func Connect(addr, dataDir string) *Client {
	tok, err := os.ReadFile(TokenPath(dataDir))
	if err != nil {
		return nil
	}
	t := strings.TrimSpace(string(tok))
	if lo := LoopbackAddr(addr); lo != addr {
		if c := ConnectWithToken(lo, t); c != nil {
			return c
		}
	}
	return ConnectWithToken(addr, t)
}

// ConnectWithToken returns a client if a daemon answers at addr with this token, else nil.
func ConnectWithToken(addr, token string) *Client {
	return Dial(Target{Address: addr, Token: token})
}

// Dial returns a client if the twin at t answers with t's key, else nil.
func Dial(t Target) *Client {
	c, _ := DialErr(t)
	return c
}

// DialErr is Dial saying why not: a network error, or the twin's *Error (a
// 401 means it doesn't know this key, for example because it was revoked).
func DialErr(t Target) (*Client, error) {
	c := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.Status(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func newClient(t Target) *Client {
	return &Client{base: BaseURL(t.Address), token: t.Token, http: &http.Client{Timeout: 16 * time.Minute, Transport: transport(t.Pins)}}
}

// BaseURL turns host:port into http://host:port; a URL stays as it is.
func BaseURL(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimRight(addr, "/")
	}
	return "http://" + addr
}

// transport trusts, when pins are given, exactly the certificates whose key
// matches one of them (a twin's own certificate needn't come from a CA).
func transport(pins []string) http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if len(pins) == 0 {
		return tr
	}
	pins = slices.Clone(pins)
	tr.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // replaced by the pin check below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the twin sent no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if slices.Contains(pins, base64.RawURLEncoding.EncodeToString(sum[:])) {
				return nil
			}
			return verifyTailscaleRenewal(cs, tr.TLSClientConfig.RootCAs)
		},
	}
	return tr
}

// Address returns the address the client talks to.
func (c *Client) Address() string {
	return strings.TrimPrefix(c.base, "http://")
}

// Token is the key the client uses.
func (c *Client) Token() string { return c.token }

// Error is a refusal from the twin, in words.
type Error struct {
	Status  int
	Code    string
	Message string
	Fix     string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Message + " " + e.Fix)
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return "daemon: " + msg
}

// readError turns a non-200 response into an error: the twin's own words
// when it sent them.
func readError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e apiError
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") && json.Unmarshal(b, &e) == nil && e.Message != "" {
		return &Error{Status: resp.StatusCode, Code: e.Error, Message: e.Message, Fix: e.Fix}
	}
	return &Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return readError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Status fetches the daemon's status.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	if err := c.do(ctx, http.MethodGet, "/status", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Message sends a user message and returns the reply.
func (c *Client) Message(ctx context.Context, channel, chatID, text string) (string, error) {
	var r MessageResponse
	err := c.do(ctx, http.MethodPost, "/message", MessageRequest{Channel: channel, ChatID: chatID, Text: text}, &r)
	return r.Reply, err
}

// MessageStream sends a user message and streams the reply's text fragments to
// onDelta; it returns the full reply.
func (c *Client) MessageStream(ctx context.Context, channel, chatID, text string, onDelta func(string)) (string, error) {
	return c.MessageEvents(ctx, channel, chatID, text, onDelta, nil)
}

// MessageEvents is MessageStream with tool narration notes.
func (c *Client) MessageEvents(ctx context.Context, channel, chatID, text string, onDelta func(string), onNote func(string)) (string, error) {
	b, _ := json.Marshal(MessageRequest{Channel: channel, ChatID: chatID, Text: text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/message/stream", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", readError(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	event := ""
	var reply string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			switch event {
			case "delta":
				var d struct{ Text string }
				if json.Unmarshal([]byte(data), &d) == nil && d.Text != "" {
					onDelta(d.Text)
				}
			case "note":
				var d struct{ Text string }
				if json.Unmarshal([]byte(data), &d) == nil && d.Text != "" && onNote != nil {
					onNote(d.Text)
				}
			case "done":
				var r MessageResponse
				_ = json.Unmarshal([]byte(data), &r)
				reply = r.Reply
			case "error":
				var e struct{ Error string }
				_ = json.Unmarshal([]byte(data), &e)
				return "", errors.New(e.Error)
			}
		}
	}
	return reply, sc.Err()
}

// RunHealth asks the daemon to run its self-checks.
func (c *Client) RunHealth(ctx context.Context) (health.Report, error) {
	var rep health.Report
	err := c.do(ctx, http.MethodPost, "/health/run", nil, &rep)
	return rep, err
}

// SetPaused pauses or resumes the daemon.
func (c *Client) SetPaused(ctx context.Context, paused bool) error {
	return c.do(ctx, http.MethodPost, "/pause", map[string]bool{"paused": paused}, nil)
}

// RunProtocol triggers a protocol now.
func (c *Client) RunProtocol(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/protocols/run", map[string]string{"name": name}, nil)
}

// RunJob asks the daemon to run a built-in job now (POST /jobs/run: the
// voice pipeline rebuilt, the portrait refreshed). A refusal is an *Error in
// the twin's own words.
func (c *Client) RunJob(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/jobs/run", map[string]string{"name": name}, nil)
}

// PairWhatsApp asks the daemon to start linking WhatsApp.
func (c *Client) PairWhatsApp(ctx context.Context, byPhone bool) error {
	return c.do(ctx, http.MethodPost, "/channels/whatsapp/pair", map[string]any{"by_phone": byPhone}, nil)
}

// WhatsAppPairing polls the pairing state.
func (c *Client) WhatsAppPairing(ctx context.Context) (*whatsapp.Snapshot, error) {
	var s whatsapp.Snapshot
	if err := c.do(ctx, http.MethodGet, "/channels/whatsapp/pair", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// NewOffer asks the twin (on this computer) for a pairing offer.
func (c *Client) NewOffer(ctx context.Context, kind string, scopes []devices.Scope) (OfferResponse, error) {
	var out OfferResponse
	err := c.do(ctx, http.MethodPost, "/devices/offers", map[string]any{"kind": kind, "scopes": scopes}, &out)
	return out, err
}

// Devices lists the paired devices.
func (c *Client) Devices(ctx context.Context) ([]devices.Device, error) {
	var out struct {
		Devices []devices.Device `json:"devices"`
	}
	err := c.do(ctx, http.MethodGet, "/devices", nil, &out)
	return out.Devices, err
}

// RevokeDevice cuts a device off.
func (c *Client) RevokeDevice(ctx context.Context, id string) (RevokeResult, error) {
	var out RevokeResult
	err := c.do(ctx, http.MethodPost, "/devices/"+id+"/revoke", nil, &out)
	return out, err
}

// RenameDevice renames a device.
func (c *Client) RenameDevice(ctx context.Context, id, name string) (devices.Device, error) {
	var out struct {
		Device devices.Device `json:"device"`
	}
	err := c.do(ctx, http.MethodPost, "/devices/"+id+"/rename", map[string]string{"name": name}, &out)
	return out.Device, err
}

// UpgradeLegacy swaps an old-style code's master key for a key of this
// terminal's own. The client then uses the new key.
func (c *Client) UpgradeLegacy(ctx context.Context, name string) (ClaimResponse, error) {
	var out ClaimResponse
	if err := c.do(ctx, http.MethodPost, "/pair/upgrade", map[string]string{"name": name}, &out); err != nil {
		return out, err
	}
	if out.Token == "" {
		return out, errors.New("the twin didn't send a key")
	}
	c.token = out.Token
	return out, nil
}

// Claim spends a pairing offer at the twin at base (trusting pins, when
// given, for TLS), for a device of kind called name.
func Claim(ctx context.Context, base string, pins []string, offerID, secret, name, kind string) (ClaimResponse, error) {
	c := newClient(Target{Address: base, Pins: pins})
	c.http.Timeout = 20 * time.Second
	var out ClaimResponse
	err := c.do(ctx, http.MethodPost, "/pair/claim", ClaimRequest{Offer: offerID, Secret: secret, Name: name, Kind: kind}, &out)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return out, &Error{Status: e.Status, Code: "offer_unknown", Message: "That twin doesn't know this pairing code: it may have restarted since the code was made.",
			Fix: "Make a new one: type mirrin pair in Terminal on the computer it runs on."}
	}
	if err == nil && out.Token == "" {
		err = fmt.Errorf("the twin paired this device but sent no key; pair again with `mirrin pair`")
	}
	return out, err
}
