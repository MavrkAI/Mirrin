package cloud

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// Link statuses, as GET /v1/link/{id} reports them.
const (
	LinkPending = "pending" // checkout not finished yet
	LinkActive  = "active"  // paid; the answer carries the entitlement
	LinkExpired = "expired" // nobody paid in time; start again
)

// LinkStart is the answer to StartLink.
type LinkStart struct {
	ID          string // the link to poll
	CheckoutURL string // the checkout page, opened in the user's browser
}

// StartLink begins linking this machine: it makes the device key if there
// is none, and sends only public things: the device's public key, the
// handle asked for ("" for a random one), and the ACME account URI the
// handle's CAA record will name ("" until the machine has one).
func (c *Client) StartLink(ctx context.Context, handle, acmeAccount string) (LinkStart, error) {
	if handle != "" {
		if err := CheckHandle(handle); err != nil {
			return LinkStart{}, err
		}
	}
	if acmeAccount != "" {
		if err := CheckACMEAccount(acmeAccount); err != nil {
			return LinkStart{}, err
		}
	}
	if info, ok, err := c.state.Info(); err != nil {
		return LinkStart{}, err
	} else if ok {
		return LinkStart{}, fmt.Errorf("cloud: this machine is already linked to %s as %s; run mirrin cloud unlink first", info.API, info.Handle)
	}
	key, err := c.deviceKey(true)
	if err != nil {
		return LinkStart{}, err
	}
	in := struct {
		DevicePub   string `json:"device_pub"`
		Handle      string `json:"handle,omitempty"`
		ACMEAccount string `json:"acme_account,omitempty"`
	}{entitle.EncodeKey(key.Public().(ed25519.PublicKey)), handle, acmeAccount}
	r, err := c.call(ctx, http.MethodPost, "/v1/link/start", in, maxReply, true)
	if err != nil {
		return LinkStart{}, err
	}
	var out struct {
		ID          string `json:"id"`
		CheckoutURL string `json:"checkout_url"`
	}
	if err := decode(r, &out); err != nil {
		return LinkStart{}, err
	}
	if err := checkID(out.ID); err != nil {
		return LinkStart{}, err
	}
	if err := c.checkBrowserURL(out.CheckoutURL); err != nil {
		return LinkStart{}, err
	}
	p := pendingLink{API: c.api, ID: out.ID, CheckoutURL: out.CheckoutURL, Started: c.Now().UTC().Truncate(time.Second)}
	if err := c.state.savePending(p); err != nil {
		return LinkStart{}, err
	}
	return LinkStart{ID: out.ID, CheckoutURL: out.CheckoutURL}, nil
}

// PendingLink returns a link this machine started with this control plane
// and has not finished, and when it started. Resuming it, rather than
// starting another, keeps a checkout from being paid twice.
func (c *Client) PendingLink() (LinkStart, time.Time, bool) {
	p, err := c.state.pending()
	if err != nil || p == nil || p.API != c.api || c.checkBrowserURL(p.CheckoutURL) != nil {
		return LinkStart{}, time.Time{}, false
	}
	return LinkStart{ID: p.ID, CheckoutURL: p.CheckoutURL}, p.Started, true
}

// PollLink asks how link id is going. When it is active, the entitlement is
// verified, checked to be bound to this machine's key, and stored; only then
// is it returned.
func (c *Client) PollLink(ctx context.Context, id string) (status, ent string, err error) {
	if err := checkID(id); err != nil {
		return "", "", err
	}
	r, err := c.call(ctx, http.MethodGet, "/v1/link/"+id, nil, maxReply, false)
	if err != nil {
		return "", "", err
	}
	var out struct {
		Status      string `json:"status"`
		Entitlement string `json:"entitlement"`
	}
	if err := decode(r, &out); err != nil {
		return "", "", err
	}
	switch out.Status {
	case LinkPending:
		return out.Status, "", nil
	case LinkExpired:
		return out.Status, "", c.state.clearPending()
	case LinkActive:
	default:
		return "", "", fmt.Errorf("cloud: unknown link status %q", clip(out.Status, 32))
	}
	cl, err := c.accept(out.Entitlement)
	if err != nil {
		return "", "", err
	}
	if err := c.state.saveEntitlement(c.api, out.Entitlement, cl, c.Now(), true); err != nil {
		return "", "", err
	}
	return LinkActive, out.Entitlement, c.state.clearPending()
}

// Refresh asks for a new entitlement, as the daemon does about once a day.
// A 402 lapsed (ErrLapsed) keeps the stored token until it expires. A 403
// revoked or deleted (ErrRevoked) and a 409 superseded naming a later
// generation (*SupersededError) are recorded, so Current reports Expired and
// Superseded. Any other answer, a 403 or 409 without those codes included,
// changes nothing and is worth retrying later.
func (c *Client) Refresh(ctx context.Context) (entitle.Claims, error) {
	rec, err := c.linked()
	if err != nil {
		return entitle.Claims{}, err
	}
	r, err := c.call(ctx, http.MethodPost, "/v1/entitlement/refresh", nil, maxReply, false)
	if err != nil {
		return entitle.Claims{}, err
	}
	now := c.Now().UTC().Truncate(time.Second)
	switch r.status {
	case http.StatusOK:
	case http.StatusPaymentRequired:
		e := apiError(r)
		if !errors.Is(e, ErrLapsed) {
			return entitle.Claims{}, e
		}
		return entitle.Claims{}, errors.Join(e, c.state.update(func(l *linkRecord) { l.CheckedAt = now }))
	case http.StatusForbidden:
		e := apiError(r)
		if !errors.Is(e, ErrRevoked) {
			return entitle.Claims{}, e
		}
		return entitle.Claims{}, errors.Join(e, c.state.update(func(l *linkRecord) { l.RevokedAt = now }))
	case http.StatusConflict:
		var b struct {
			Error string    `json:"error"`
			Gen   int64     `json:"gen"`
			At    time.Time `json:"at"`
		}
		if json.Unmarshal(r.body, &b) != nil || b.Error != "superseded" || b.Gen <= c.state.heldGen(rec) {
			// Only a later generation than this machine holds supersedes it.
			return entitle.Claims{}, apiError(r)
		}
		sup := &Supersede{Gen: b.Gen, At: b.At.UTC()}
		err := c.state.update(func(l *linkRecord) { l.Superseded = sup })
		return entitle.Claims{}, errors.Join(&SupersededError{Gen: b.Gen, At: sup.At}, err)
	default:
		return entitle.Claims{}, apiError(r)
	}
	var out struct {
		Entitlement string `json:"entitlement"`
	}
	if err := decode(r, &out); err != nil {
		return entitle.Claims{}, err
	}
	cl, err := c.accept(out.Entitlement)
	if err != nil {
		return entitle.Claims{}, err
	}
	if err := c.state.saveEntitlement(c.api, out.Entitlement, cl, now, false); err != nil {
		return entitle.Claims{}, err
	}
	return cl, nil
}

// Me returns everything the control plane stores about this account, as it
// sends it: a JSON object.
func (c *Client) Me(ctx context.Context) (jsonv1.RawMessage, error) {
	if _, err := c.linked(); err != nil {
		return nil, err
	}
	r, err := c.call(ctx, http.MethodGet, "/v1/me", nil, maxMeReply, false)
	if err != nil {
		return nil, err
	}
	if err := decode(r, nil); err != nil {
		return nil, err
	}
	v := jsontext.Value(r.body)
	if !v.IsValid() || v.Kind() != '{' {
		return nil, errors.New("cloud: /v1/me did not answer with a JSON object")
	}
	return jsonv1.RawMessage(v), nil
}

// BillingPortal returns the merchant of record's page for this account:
// receipts, card, cancelling. It is opened in the user's browser.
func (c *Client) BillingPortal(ctx context.Context) (string, error) {
	if _, err := c.linked(); err != nil {
		return "", err
	}
	r, err := c.call(ctx, http.MethodPost, "/v1/billing/portal", nil, maxReply, false)
	if err != nil {
		return "", err
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := decode(r, &out); err != nil {
		return "", err
	}
	if err := c.checkBrowserURL(out.URL); err != nil {
		return "", err
	}
	return out.URL, nil
}

// SetACMEAccount tells the control plane which ACME account may issue
// certificates for the handle; it rewrites the handle's CAA record.
func (c *Client) SetACMEAccount(ctx context.Context, uri string) error {
	if err := CheckACMEAccount(uri); err != nil {
		return err
	}
	if _, err := c.linked(); err != nil {
		return err
	}
	r, err := c.call(ctx, http.MethodPut, "/v1/acme-account", struct {
		URI string `json:"uri"`
	}{uri}, maxReply, false)
	if err != nil {
		return err
	}
	return decode(r, nil)
}

// Unlink revokes this machine's device key at the control plane, then
// forgets the key, the entitlement and the link. An answer that the key is
// already revoked or the account deleted (403) counts as done. A 401 does
// not: it is as likely a clock more than five minutes out as an unknown key,
// and the key would stay valid there. The subscription is untouched; cancel
// it in the billing portal. The egress ledger stays.
func (c *Client) Unlink(ctx context.Context) error {
	if _, err := c.linked(); err != nil {
		return err
	}
	r, err := c.call(ctx, http.MethodDelete, "/v1/device", nil, maxReply, false)
	if err != nil {
		return err
	}
	switch e := apiError(r); {
	case r.status == http.StatusUnauthorized:
		return fmt.Errorf("%w; check that this machine's clock is right (signatures allow five minutes either way)", e)
	case errors.Is(e, ErrRevoked):
		// The control plane let go of the key already.
	default:
		if err := decode(r, nil); err != nil {
			return err
		}
	}
	return c.state.forget()
}

// Forget removes this machine's link without asking the control plane: for
// when it cannot be reached. The key stays valid there until the
// entitlement expires, but no copy of it is left here.
func (c *Client) Forget() error { return c.state.forget() }

// DeleteAccount asks for the account to be deleted. The control plane waits
// seven days, then removes its rows, the DNS records and the backup
// objects; the handle is never given to anyone else. Until then,
// RestoreAccount undoes it. The date is recorded in the link state.
func (c *Client) DeleteAccount(ctx context.Context) error {
	if _, err := c.linked(); err != nil {
		return err
	}
	r, err := c.call(ctx, http.MethodDelete, "/v1/account", nil, maxReply, false)
	if err != nil {
		return err
	}
	var out struct {
		DeleteAt time.Time `json:"delete_at"`
	}
	if err := decode(r, &out); err != nil {
		return err
	}
	if out.DeleteAt.IsZero() {
		return errors.New("cloud: the control plane did not say when the account will be deleted")
	}
	return c.state.update(func(l *linkRecord) { l.DeleteAt = out.DeleteAt.UTC() })
}

// RestoreAccount cancels a deletion that has not happened yet.
func (c *Client) RestoreAccount(ctx context.Context) error {
	if _, err := c.linked(); err != nil {
		return err
	}
	r, err := c.call(ctx, http.MethodPost, "/v1/account/restore", nil, maxReply, false)
	if err != nil {
		return err
	}
	if err := decode(r, nil); err != nil {
		return err
	}
	return c.state.update(func(l *linkRecord) { l.DeleteAt = time.Time{} })
}

// linked returns the link record, and insists this client talks to the
// control plane the machine linked with.
func (c *Client) linked() (*linkRecord, error) {
	rec, err := c.state.record()
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, ErrNotLinked
	}
	if rec.API != c.api {
		return nil, fmt.Errorf("cloud: this machine is linked with %s, not %s", rec.API, c.api)
	}
	return rec, nil
}

// accept verifies an entitlement and checks that it is bound to this
// machine's device key.
func (c *Client) accept(tok string) (entitle.Claims, error) {
	cl, err := entitle.Verify(tok, c.keys, c.Now())
	if err != nil {
		return entitle.Claims{}, fmt.Errorf("cloud: entitlement refused: %w", err)
	}
	pub, err := c.PublicKey()
	if err != nil {
		return entitle.Claims{}, err
	}
	k, err := cl.Key()
	if err != nil || subtle.ConstantTimeCompare(k, pub) != 1 {
		return entitle.Claims{}, ErrWrongKey
	}
	// The handle and hosts end up in link.json and on the terminal.
	if CheckHandle(cl.Handle) != nil || slices.ContainsFunc(cl.Hosts, func(h string) bool { return !isHostname(h) }) {
		return entitle.Claims{}, fmt.Errorf("cloud: entitlement refused: handle %q or its hosts break the rules", clip(cl.Handle, 40))
	}
	return cl, nil
}

// isHostname accepts a lowercase DNS name of letters, digits, '-' and dots,
// at most 253 bytes, with labels of 1 to 63.
func isHostname(h string) bool {
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			if ch := label[i]; !('a' <= ch && ch <= 'z' || '0' <= ch && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}

// CheckHandle applies the handle rules: 3 to 32 of a-z, 0-9 and '-', not
// starting or ending with '-', and no "--" (which IDNA reserves).
func CheckHandle(h string) error {
	bad := fmt.Errorf("cloud: handle %q must be 3 to 32 of a-z, 0-9 and single hyphens inside", h)
	if len(h) < 3 || len(h) > 32 || h[0] == '-' || h[len(h)-1] == '-' || strings.Contains(h, "--") {
		return bad
	}
	for i := 0; i < len(h); i++ {
		if ch := h[i]; !('a' <= ch && ch <= 'z' || '0' <= ch && ch <= '9' || ch == '-') {
			return bad
		}
	}
	return nil
}

// CheckACMEAccount accepts an ACME account URI that can go into a CAA
// accounturi parameter: an https URL of at most 512 bytes, with no user
// info, query or fragment, and none of the characters CAA or DNS text
// would need escaped.
func CheckACMEAccount(uri string) error {
	bad := fmt.Errorf("cloud: ACME account %q is not an https URL fit for a CAA record", clip(uri, 80))
	if uri == "" || len(uri) > 512 || strings.ContainsAny(uri, "\";\\ ") {
		return bad
	}
	for i := 0; i < len(uri); i++ {
		if uri[i] < 0x21 || uri[i] > 0x7e {
			return bad
		}
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(uri, "#") {
		return bad
	}
	return nil
}

// checkID accepts an id the control plane gave: 1 to 64 of A-Z, a-z, 0-9,
// '_' and '-'. It goes into a URL path unescaped.
func checkID(id string) error {
	if len(id) == 0 || len(id) > 64 {
		return fmt.Errorf("cloud: bad link id %q", clip(id, 80))
	}
	for i := 0; i < len(id); i++ {
		if ch := id[i]; !('a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || '0' <= ch && ch <= '9' || ch == '_' || ch == '-') {
			return fmt.Errorf("cloud: bad link id %q", clip(id, 80))
		}
	}
	return nil
}

// checkBrowserURL accepts a page the control plane wants opened in the
// user's browser and printed on the terminal: https (http only from a
// loopback control plane), at most 2048 bytes of printable ASCII, no user
// info.
func (c *Client) checkBrowserURL(s string) error {
	u, err := url.Parse(s)
	ok := err == nil && len(s) <= 2048 && u.Host != "" && u.User == nil &&
		!strings.ContainsFunc(s, func(r rune) bool { return r < 0x21 || r > 0x7e }) &&
		(u.Scheme == "https" || u.Scheme == "http" && strings.HasPrefix(c.api, "http://"))
	if !ok {
		return fmt.Errorf("cloud: the control plane sent a page that is not a web address: %q", clip(s, 80))
	}
	return nil
}
