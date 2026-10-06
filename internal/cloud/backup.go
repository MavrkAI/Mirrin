package cloud

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// Backup storage and recovery (docs/cloud-api.md §5.7 and §5.8). The
// control plane decides what may be stored and hands out presigned URLs;
// the bytes go straight to storage, and are ciphertext the 12 backup words
// open, which neither the control plane nor storage ever holds. The
// recovery key those words make signs the two things only the owner may
// do: bind a namespace to this machine's account, and move the account to
// a new machine.

// Presigned is one presigned storage request: Method to URL with Headers,
// before Expires.
type Presigned struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Expires time.Time         `json:"expires"`
}

// StoredObject is one backup object the control plane counts as stored.
type StoredObject struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

// Backup operations a URL is presigned for.
const (
	OpPut    = "put"
	OpGet    = "get"
	OpDelete = "delete"
)

// Codes the backup routes answer with, for errors.As on an *APIError.
const (
	CodeNoNamespace    = "no_namespace"    // 403: no backup words bound yet
	CodeExists         = "exists"          // 409: a name is never reused
	CodeRemoved        = "removed"         // 409: a name removed before, never taken again
	CodeOverQuota      = "over_quota"      // 413
	CodeLapsed         = "lapsed"          // 402
	CodeNotFound       = "not_found"       // 404
	CodeNamespaceBound = "namespace_bound" // 409: backups made with other words are kept
	CodeSuperseded     = "superseded"      // 409: another machine recovered the account
)

// HasCode reports whether err is an answer from the control plane with
// this error code.
func HasCode(err error, code string) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == code
}

// b64 is how keys and signatures travel.
var b64 = base64.RawURLEncoding

// BindNamespace binds the backup namespace of the recovery key to this
// machine's account, with the recovery key's signature over this device's
// key. Binding it again is harmless.
func (c *Client) BindNamespace(ctx context.Context, recovery ed25519.PrivateKey) error {
	if _, err := c.linked(); err != nil {
		return err
	}
	dev, err := c.PublicKey()
	if err != nil {
		return err
	}
	rpub := recovery.Public().(ed25519.PublicKey)
	ns, rp := entitle.Namespace(rpub), entitle.EncodeKey(rpub)
	sig := ed25519.Sign(recovery, entitle.BindMessage(ns, rp, entitle.EncodeKey(dev)))
	r, err := c.call(ctx, http.MethodPost, "/v1/backup/namespaces", struct {
		NS          string `json:"ns"`
		RecoveryPub string `json:"recovery_pub"`
		Sig         string `json:"sig_recovery"`
	}{ns, rp, b64.EncodeToString(sig)}, maxReply, false)
	if err != nil {
		return err
	}
	return decode(r, nil)
}

// Presign asks for a URL for one op on the backup object name (a put of
// size bytes). The URL must point at plain storage: https, or http only
// when the control plane itself is a loopback dev server.
func (c *Client) Presign(ctx context.Context, op, name string, size int64) (Presigned, error) {
	if _, err := c.linked(); err != nil {
		return Presigned{}, err
	}
	r, err := c.call(ctx, http.MethodPost, "/v1/backup/presign", struct {
		Op   string `json:"op"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}{op, name, size}, maxReply, false)
	if err != nil {
		return Presigned{}, err
	}
	var p Presigned
	if err := decode(r, &p); err != nil {
		return Presigned{}, err
	}
	want := map[string]string{OpPut: http.MethodPut, OpGet: http.MethodGet, OpDelete: http.MethodDelete}[op]
	if p.Method != want || c.checkStorageURL(p.URL) != nil {
		return Presigned{}, fmt.Errorf("cloud: the control plane presigned %s %q for a %s", clip(p.Method, 16), clip(p.URL, 80), op)
	}
	for k, v := range p.Headers {
		// Only headers that describe the body; nothing that would carry
		// credentials or change where the request goes.
		if !strings.EqualFold(k, "Content-Length") && !strings.EqualFold(k, "Content-Type") || strings.ContainsAny(v, "\r\n") {
			return Presigned{}, fmt.Errorf("cloud: the control plane presigned a request with header %q", clip(k, 40))
		}
	}
	return p, nil
}

// checkStorageURL accepts a presigned URL: https (http only from a loopback
// control plane), no user info, printable ASCII, at most 8 KiB.
func (c *Client) checkStorageURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || len(s) > 8<<10 || u.Host == "" || u.User != nil ||
		strings.ContainsFunc(s, func(r rune) bool { return r < 0x21 || r > 0x7e }) ||
		!(u.Scheme == "https" || u.Scheme == "http" && strings.HasPrefix(c.api, "http://")) {
		return errors.New("cloud: not a storage URL")
	}
	return nil
}

// BackupListing is what GET /v1/backup/objects answers: the namespace
// bound to the account and what it holds.
type BackupListing struct {
	NS      string         `json:"ns"`
	Objects []StoredObject `json:"objects"`
	Bytes   int64          `json:"bytes"`
	Held    int64          `json:"held"`
	Quota   int64          `json:"quota"`
}

// BackupList lists the stored backup objects, and the namespace they are
// kept under.
func (c *Client) BackupList(ctx context.Context) (BackupListing, error) {
	if _, err := c.linked(); err != nil {
		return BackupListing{}, err
	}
	r, err := c.call(ctx, http.MethodGet, "/v1/backup/objects", nil, maxMeReply, false)
	if err != nil {
		return BackupListing{}, err
	}
	var out BackupListing
	if err := decode(r, &out); err != nil {
		return BackupListing{}, err
	}
	return out, nil
}

// BackupObjects lists the stored backup objects.
func (c *Client) BackupObjects(ctx context.Context) ([]StoredObject, error) {
	l, err := c.BackupList(ctx)
	return l.Objects, err
}

// Commit asks the control plane to count name as stored, once it has
// checked that storage holds size bytes under it.
func (c *Client) Commit(ctx context.Context, name string, size int64) error {
	if _, err := c.linked(); err != nil {
		return err
	}
	r, err := c.call(ctx, http.MethodPost, "/v1/backup/commit", struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}{name, size}, maxReply, false)
	if err != nil {
		return err
	}
	return decode(r, nil)
}

// storageHTTP sends presigned requests: no redirects, and no overall
// timeout, since a snapshot can take a while; the caller's context bounds
// it. Its Transport is nil, as the egress test expects.
var storageHTTP = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Transfer sends a presigned request, with body (size bytes) for a put,
// and records it in the egress ledger: method, host, path and sizes, never
// the query, which holds the signature. The caller closes the answer's
// body.
func (c *Client) Transfer(ctx context.Context, p Presigned, body io.Reader, size int64) (*http.Response, error) {
	if err := c.checkStorageURL(p.URL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, p.Method, p.URL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range p.Headers {
		if !strings.EqualFold(k, "Content-Length") {
			req.Header.Set(k, v)
		}
	}
	if body != nil {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	}
	led, err := openLedger(c.dataDir)
	if err != nil {
		return nil, err
	}
	defer led.Close()
	e := Entry{At: c.Now(), Method: p.Method, Host: req.URL.Host, Path: req.URL.EscapedPath()}
	if body != nil {
		e.ReqBytes = size
	}
	res, err := storageHTTP.Do(req)
	if err != nil {
		if lerr := writeEntry(led, e); lerr != nil {
			return nil, errors.Join(err, lerr)
		}
		return nil, err
	}
	e.Status = res.StatusCode
	if res.ContentLength > 0 {
		e.RespBytes = res.ContentLength
	}
	if err := writeEntry(led, e); err != nil {
		res.Body.Close()
		return nil, err
	}
	return res, nil
}

// Recovered is what a recovery moved to this machine.
type Recovered struct {
	Handle string
	Gen    int64
	// Entitled is false when payment has lapsed: the backups can still be
	// fetched for 90 days, but nothing else is served.
	Entitled bool
}

// Recover moves the account whose backups the recovery key's namespace
// holds to this machine, which must not be linked: it makes a device key
// and sends only public things, with the recovery key's signature over
// them. The control plane raises the handle's generation, so every other
// machine stands by; deny-lists their keys; points the handle's CAA record
// at acmeAccount ("" lets no CA issue until this machine names its
// account); and answers with an entitlement for this machine, which is
// stored like a link's.
func (c *Client) Recover(ctx context.Context, recovery ed25519.PrivateKey, acmeAccount string) (Recovered, error) {
	if acmeAccount != "" {
		if err := CheckACMEAccount(acmeAccount); err != nil {
			return Recovered{}, err
		}
	}
	if info, ok, err := c.state.Info(); err != nil {
		return Recovered{}, err
	} else if ok {
		return Recovered{}, fmt.Errorf("cloud: this machine is already linked to %s as %s", info.API, info.Handle)
	}
	key, err := c.deviceKey(true)
	if err != nil {
		return Recovered{}, err
	}
	dev := entitle.EncodeKey(key.Public().(ed25519.PublicKey))
	ns := entitle.Namespace(recovery.Public().(ed25519.PublicKey))
	ts := c.Now().UTC().Truncate(time.Second)
	sig := ed25519.Sign(recovery, entitle.RecoverMessage(ns, dev, acmeAccount, ts))
	r, err := c.call(ctx, http.MethodPost, "/v1/recover", struct {
		NS          string    `json:"ns"`
		DevicePub   string    `json:"device_pub"`
		ACMEAccount string    `json:"acme_account"`
		TS          time.Time `json:"ts"`
		Sig         string    `json:"sig_recovery"`
	}{ns, dev, acmeAccount, ts, b64.EncodeToString(sig)}, maxReply, true)
	if err != nil {
		return Recovered{}, err
	}
	var out struct {
		Entitlement string `json:"entitlement"`
		Handle      string `json:"handle"`
		Gen         int64  `json:"gen"`
	}
	if err := decode(r, &out); err != nil {
		return Recovered{}, err
	}
	if CheckHandle(out.Handle) != nil || out.Gen < 1 {
		return Recovered{}, fmt.Errorf("cloud: recovery answered handle %q at generation %d", clip(out.Handle, 40), out.Gen)
	}
	now := c.Now()
	if out.Entitlement == "" {
		return Recovered{Handle: out.Handle, Gen: out.Gen}, c.state.saveRecovered(c.api, out.Handle, out.Gen, now)
	}
	cl, err := c.accept(out.Entitlement)
	if err != nil {
		return Recovered{}, err
	}
	if cl.Handle != out.Handle || cl.Gen != out.Gen {
		return Recovered{}, fmt.Errorf("cloud: recovery answered %s at %d with an entitlement for %s at %d", out.Handle, out.Gen, cl.Handle, cl.Gen)
	}
	if err := c.state.saveEntitlement(c.api, out.Entitlement, cl, now, true); err != nil {
		return Recovered{}, err
	}
	return Recovered{Handle: cl.Handle, Gen: cl.Gen, Entitled: true}, nil
}
