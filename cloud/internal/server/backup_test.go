package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MavrkAI/Mirrin/cloud/internal/storage"
	"github.com/MavrkAI/Mirrin/cloud/internal/store"
	"github.com/MavrkAI/Mirrin/internal/entitle"
)

// snapName is a snapshot name for day i of September 2026.
func snapName(i int) string {
	at := time.Date(2026, 9, 1, 3, 30, 0, 0, time.UTC).AddDate(0, 0, i)
	return fmt.Sprintf("snap-%s-%08x.age", at.Format("20060102T150405Z"), i)
}

func sig(k ed25519.PrivateKey, msg []byte) string {
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(k, msg))
}

// bind binds the namespace of the recovery key rec for dev's account.
func (e *env) bind(t *testing.T, dev, rec ed25519.PrivateKey) (string, resp) {
	t.Helper()
	rpub := pubOf(rec)
	ns := entitle.Namespace(rec.Public().(ed25519.PublicKey))
	b, _ := json.Marshal(map[string]string{"ns": ns, "recovery_pub": rpub, "sig_recovery": sig(rec, entitle.BindMessage(ns, rpub, pubOf(dev)))})
	return ns, send(t, e.signed(t, dev, "POST", "/v1/backup/namespaces", b))
}

// presignAs asks for a URL for op on name.
func (e *env) presignAs(t *testing.T, dev ed25519.PrivateKey, op, name string, size int64) (resp, storage.Signed) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"op": op, "name": name, "size": size})
	r := send(t, e.signed(t, dev, "POST", "/v1/backup/presign", b))
	var s storage.Signed
	if r.status == 200 {
		var raw struct {
			URL     string            `json:"url"`
			Method  string            `json:"method"`
			Headers map[string]string `json:"headers"`
			Expires time.Time         `json:"expires"`
		}
		r.json(t, &raw)
		s = storage.Signed{URL: raw.URL, Method: raw.Method, Headers: raw.Headers, Expires: raw.Expires}
	}
	return r, s
}

// transfer sends a presigned request with body, length bytes long.
func transfer(t *testing.T, s storage.Signed, body []byte, length int64) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(s.Method, s.URL, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = length
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func (e *env) commitAs(t *testing.T, dev ed25519.PrivateKey, name string, size int64) resp {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"name": name, "size": size})
	return send(t, e.signed(t, dev, "POST", "/v1/backup/commit", b))
}

// upload stores body as name the way the daemon does: presign, PUT, commit.
func (e *env) upload(t *testing.T, dev ed25519.PrivateKey, name string, body []byte) {
	t.Helper()
	r, s := e.presignAs(t, dev, storage.OpPut, name, int64(len(body)))
	if r.status != 200 {
		t.Fatalf("presign put %s: %d %s", name, r.status, r.body)
	}
	if st, b := transfer(t, s, body, int64(len(body))); st != 200 {
		t.Fatalf("PUT %s: %d %s", name, st, b)
	}
	if r := e.commitAs(t, dev, name, int64(len(body))); r.status != 200 {
		t.Fatalf("commit %s: %d %s", name, r.status, r.body)
	}
}

type listed struct {
	Objects []struct {
		Name    string    `json:"name"`
		Size    int64     `json:"size"`
		Created time.Time `json:"created"`
	} `json:"objects"`
	Bytes int64 `json:"bytes"`
	Quota int64 `json:"quota"`
}

func (e *env) list(t *testing.T, dev ed25519.PrivateKey) (resp, listed) {
	t.Helper()
	r := send(t, e.signed(t, dev, "GET", "/v1/backup/objects", nil))
	var l listed
	if r.status == 200 {
		r.json(t, &l)
	}
	return r, l
}

func (l listed) names() []string {
	var out []string
	for _, o := range l.Objects {
		out = append(out, o.Name)
	}
	return out
}

// A presign, a list or a commit before the namespace is bound gets 403.
func TestBackupNeedsABoundNamespace(t *testing.T) {
	e := newEnv(t, false, nil)
	dev := newKey(t)
	e.link(t, dev, "brisk-heron-10", acme)
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(1), 10); r.status != http.StatusForbidden || r.code != "no_namespace" {
		t.Errorf("presign: %d %s", r.status, r.body)
	}
	if r, _ := e.list(t, dev); r.status != http.StatusForbidden || r.code != "no_namespace" {
		t.Errorf("list: %d %s", r.status, r.body)
	}
	if r := e.commitAs(t, dev, snapName(1), 10); r.status != http.StatusForbidden || r.code != "no_namespace" {
		t.Errorf("commit: %d %s", r.status, r.body)
	}
	// Unlinked keys can't reach the routes at all.
	if r, _ := e.presignAs(t, newKey(t), storage.OpGet, snapName(1), 0); r.status != http.StatusUnauthorized {
		t.Errorf("a stranger's presign: %d %s", r.status, r.body)
	}
}

func TestBindNamespace(t *testing.T) {
	e := newEnv(t, false, nil)
	dev, rec := newKey(t), newKey(t)
	e.link(t, dev, "brisk-heron-11", acme)
	rpub := pubOf(rec)
	ns := entitle.Namespace(rec.Public().(ed25519.PublicKey))
	post := func(in map[string]string) resp {
		b, _ := json.Marshal(in)
		return send(t, e.signed(t, dev, "POST", "/v1/backup/namespaces", b))
	}
	good := sig(rec, entitle.BindMessage(ns, rpub, pubOf(dev)))
	if r := post(map[string]string{"ns": "abcdefghijklmnopqrstuvwxyz", "recovery_pub": rpub, "sig_recovery": good}); r.status != http.StatusBadRequest {
		t.Errorf("a namespace not of the key: %d %s", r.status, r.body)
	}
	if r := post(map[string]string{"ns": ns, "recovery_pub": rpub, "sig_recovery": sig(newKey(t), entitle.BindMessage(ns, rpub, pubOf(dev)))}); r.status != http.StatusUnauthorized {
		t.Errorf("another key's signature: %d %s", r.status, r.body)
	}
	if r := post(map[string]string{"ns": ns, "recovery_pub": rpub, "sig_recovery": sig(rec, entitle.BindMessage(ns, rpub, pubOf(newKey(t))))}); r.status != http.StatusUnauthorized {
		t.Errorf("a signature for another device: %d %s", r.status, r.body)
	}
	if r := post(map[string]string{"ns": ns, "recovery_pub": rpub, "sig_recovery": good, "extra": "x"}); r.status != http.StatusBadRequest {
		t.Errorf("an unknown member: %d %s", r.status, r.body)
	}
	if r := post(map[string]string{"ns": ns, "recovery_pub": rpub, "sig_recovery": good}); r.status != http.StatusNoContent {
		t.Fatalf("bind: %d %s", r.status, r.body)
	}
	if _, r := e.bind(t, dev, rec); r.status != http.StatusNoContent {
		t.Errorf("binding again: %d %s", r.status, r.body)
	}
	// Another account can't take these words.
	other := newKey(t)
	e.link(t, other, "brisk-heron-99", acme)
	if _, r := e.bind(t, other, rec); r.status != http.StatusConflict || r.code != "namespace_taken" {
		t.Errorf("another account binding the same words: %d %s", r.status, r.body)
	}
	// New words replace an empty namespace, but not one holding backups.
	e.upload(t, dev, snapName(1), []byte("ciphertext"))
	if _, r := e.bind(t, dev, newKey(t)); r.status != http.StatusConflict || r.code != "namespace_bound" {
		t.Errorf("new words over stored backups: %d %s", r.status, r.body)
	}
	if _, r := e.bind(t, other, newKey(t)); r.status != http.StatusNoContent {
		t.Errorf("new words: %d %s", r.status, r.body)
	}
	if _, r := e.bind(t, other, newKey(t)); r.status != http.StatusNoContent {
		t.Errorf("newer words over an empty namespace: %d %s", r.status, r.body)
	}
}

func TestPresignCommitListDelete(t *testing.T) {
	e := newEnv(t, false, nil)
	dev, rec := newKey(t), newKey(t)
	e.link(t, dev, "brisk-heron-12", acme)
	ns, _ := e.bind(t, dev, rec)

	for _, bad := range []string{"../x.age", "notes.txt", "a/" + snapName(1), "snap-x.age", ""} {
		if r, _ := e.presignAs(t, dev, storage.OpPut, bad, 1); r.status != http.StatusBadRequest {
			t.Errorf("presign %q: %d %s", bad, r.status, r.body)
		}
	}
	if r, _ := e.presignAs(t, dev, "copy", snapName(1), 1); r.status != http.StatusBadRequest {
		t.Errorf("an unknown op: %d", r.status)
	}

	body := []byte("0123456789")
	r, put := e.presignAs(t, dev, storage.OpPut, snapName(5), 10)
	if r.status != 200 {
		t.Fatalf("presign: %d %s", r.status, r.body)
	}
	// The key is always ns/<ns>/<name>, whatever the daemon asked.
	u, _ := url.Parse(put.URL)
	if u.Path != "/storage/ns/"+ns+"/"+snapName(5) || put.Method != "PUT" || put.Headers["Content-Length"] != "10" {
		t.Fatalf("presigned %+v", put)
	}
	if d := time.Until(put.Expires); d > 15*time.Minute+time.Second || d < 14*time.Minute {
		t.Errorf("expires in %s, want 15 minutes", d)
	}
	// The store refuses a Content-Length other than the size allowed.
	if st, _ := transfer(t, put, append(body, 'x'), 11); st == 200 {
		t.Fatal("the store took 11 bytes for a 10-byte URL")
	}
	if r := e.commitAs(t, dev, snapName(5), 10); r.status != http.StatusConflict || r.code != "not_uploaded" {
		t.Errorf("commit before upload: %d %s", r.status, r.body)
	}
	if st, b := transfer(t, put, body, 10); st != 200 {
		t.Fatalf("PUT: %d %s", st, b)
	}
	// HEAD-verified: a commit claiming another size removes the object.
	if r := e.commitAs(t, dev, snapName(5), 11); r.status != http.StatusConflict || r.code != "size_mismatch" {
		t.Errorf("commit of the wrong size: %d %s", r.status, r.body)
	}
	if n, _ := e.objects.List(t.Context(), storage.Prefix(ns)); len(n) != 0 {
		t.Fatalf("a wrong-sized object stayed: %v", n)
	}
	// And its name is gone for good.
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(5), 10); r.status != http.StatusConflict || r.code != "removed" {
		t.Errorf("a discarded name was presigned again: %d %s", r.status, r.body)
	}
	if r := e.commitAs(t, dev, snapName(5), 10); r.status != http.StatusConflict || r.code != "removed" {
		t.Errorf("a discarded name was committed: %d %s", r.status, r.body)
	}
	e.upload(t, dev, snapName(1), body)
	if r := e.commitAs(t, dev, snapName(1), 10); r.status != 200 {
		t.Errorf("committing again: %d %s", r.status, r.body)
	}
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(1), 10); r.status != http.StatusConflict || r.code != "exists" {
		t.Errorf("a name is never replaced: %d %s", r.status, r.body)
	}
	e.upload(t, dev, "handover-20260903T090000Z-5e6f7a8b.age", []byte("marker"))
	_, l := e.list(t, dev)
	if !slices.Equal(l.names(), []string{"handover-20260903T090000Z-5e6f7a8b.age", snapName(1)}) || l.Bytes != 16 || l.Quota != DevConfig().BackupQuota {
		t.Fatalf("list %+v", l)
	}

	r, get := e.presignAs(t, dev, storage.OpGet, snapName(1), 0)
	if r.status != 200 {
		t.Fatalf("presign get: %d %s", r.status, r.body)
	}
	if st, b := transfer(t, get, nil, 0); st != 200 || !bytes.Equal(b, body) {
		t.Fatalf("GET: %d %q", st, b)
	}
	if r, _ := e.presignAs(t, dev, storage.OpGet, snapName(2), 0); r.status != http.StatusNotFound {
		t.Errorf("get of nothing: %d %s", r.status, r.body)
	}
	r, del := e.presignAs(t, dev, storage.OpDelete, snapName(1), 0)
	if r.status != 200 {
		t.Fatalf("presign delete: %d %s", r.status, r.body)
	}
	if st, _ := transfer(t, del, nil, 0); st != http.StatusNoContent {
		t.Fatalf("DELETE: %d", st)
	}
	if _, l := e.list(t, dev); len(l.Objects) != 1 || l.Bytes != 6 {
		t.Fatalf("after delete %+v", l)
	}
	if _, err := e.objects.Head(t.Context(), "ns/"+ns+"/"+snapName(1)); err == nil {
		t.Fatal("the object is still stored")
	}
}

func TestQuotaAtPresign(t *testing.T) {
	e := newEnv(t, false, func(c *Config) { c.BackupQuota = 100 })
	dev := newKey(t)
	e.link(t, dev, "brisk-heron-13", acme)
	e.bind(t, dev, newKey(t))
	e.upload(t, dev, snapName(1), bytes.Repeat([]byte("x"), 60))
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(2), 41); r.status != http.StatusRequestEntityTooLarge || r.code != "over_quota" {
		t.Errorf("over quota: %d %s", r.status, r.body)
	}
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(2), 40); r.status != 200 {
		t.Errorf("up to the quota: %d %s", r.status, r.body)
	}
}

// held is what the account's pending uploads and removed objects hold
// against its quota, as the list reports it.
func (e *env) held(t *testing.T, dev ed25519.PrivateKey) int64 {
	t.Helper()
	r := send(t, e.signed(t, dev, "GET", "/v1/backup/objects", nil))
	var l struct {
		Held int64 `json:"held"`
	}
	r.json(t, &l)
	return l.Held
}

// An upload presigned and never committed holds its size against the quota
// from the presign until the sweep removes it, a while after its URL ran
// out; storage is then rid of it and its name is never used again.
func TestQuotaHoldsUncommittedUploads(t *testing.T) {
	e := newEnv(t, true, func(c *Config) { c.BackupQuota = 100 })
	dev := newKey(t)
	e.link(t, dev, "brisk-heron-16", acme)
	ns, _ := e.bind(t, dev, newKey(t))

	r, put := e.presignAs(t, dev, storage.OpPut, snapName(1), 60)
	if r.status != 200 {
		t.Fatalf("presign: %d %s", r.status, r.body)
	}
	if st, b := transfer(t, put, bytes.Repeat([]byte("x"), 60), 60); st != 200 {
		t.Fatalf("PUT: %d %s", st, b)
	}
	// Never committed, it still counts: many names at once can't add up to
	// more than the quota.
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(2), 41); r.status != http.StatusRequestEntityTooLarge || r.code != "over_quota" {
		t.Errorf("presign over what the pending upload leaves: %d %s", r.status, r.body)
	}
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(2), 40); r.status != 200 {
		t.Errorf("presign of what is left: %d %s", r.status, r.body)
	}
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(3), 1); r.status != http.StatusRequestEntityTooLarge {
		t.Errorf("a third presign past the quota: %d %s", r.status, r.body)
	}
	// Presigning the same upload again holds it once; at another size it is
	// refused.
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(1), 60); r.status != 200 {
		t.Errorf("presign again: %d %s", r.status, r.body)
	}
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(1), 50); r.status != http.StatusConflict {
		t.Errorf("presign again at another size: %d %s", r.status, r.body)
	}
	if h := e.held(t, dev); h != 100 {
		t.Errorf("held %d, want 100", h)
	}

	// The sweep leaves uploads alone while a PUT may still be arriving.
	e.clock.Add(presignTTL + uploadGrace - time.Minute)
	if err := e.s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h := e.held(t, dev); h != 100 {
		t.Errorf("held %d before the grace ran out, want 100", h)
	}
	e.clock.Add(2 * time.Minute)
	if err := e.s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if objs, _ := e.objects.List(t.Context(), storage.Prefix(ns)); len(objs) != 0 {
		t.Fatalf("the sweep left an orphan: %+v", objs)
	}
	if h := e.held(t, dev); h != 0 {
		t.Errorf("held %d after the sweep, want 0", h)
	}
	if r := e.commitAs(t, dev, snapName(1), 60); r.status != http.StatusConflict {
		t.Errorf("commit of a swept upload: %d %s", r.status, r.body)
	}
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(1), 60); r.status != http.StatusConflict || r.code != "removed" {
		t.Errorf("a swept name was presigned again: %d %s", r.status, r.body)
	}
	e.upload(t, dev, snapName(4), bytes.Repeat([]byte("y"), 100))
}

// A delete is done by the control plane before it answers, so a daemon that
// never sends the DELETE leaves nothing stored and uncounted; an upload it
// deletes holds its size until its URL can put nothing back, and a name
// deleted is never used again.
func TestPresignDeleteDeletes(t *testing.T) {
	e := newEnv(t, true, func(c *Config) { c.BackupQuota = 100 })
	dev := newKey(t)
	e.link(t, dev, "brisk-heron-17", acme)
	ns, _ := e.bind(t, dev, newKey(t))
	key := func(name string) string { k, _ := storage.Key(ns, name); return k }

	e.upload(t, dev, snapName(1), bytes.Repeat([]byte("x"), 30))
	if r, _ := e.presignAs(t, dev, storage.OpDelete, snapName(1), 0); r.status != 200 {
		t.Fatalf("presign delete: %d %s", r.status, r.body)
	}
	if _, err := e.objects.Head(t.Context(), key(snapName(1))); err == nil {
		t.Fatal("the object is stored after a delete the daemon never sent")
	}
	if _, l := e.list(t, dev); l.Bytes != 0 || len(l.Objects) != 0 || e.held(t, dev) != 0 {
		t.Fatalf("after delete %+v held %d", l, e.held(t, dev))
	}
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(1), 30); r.status != http.StatusConflict || r.code != "removed" {
		t.Errorf("a deleted name was presigned again: %d %s", r.status, r.body)
	}

	// An upload deleted before its commit: its URL could still put it back,
	// so its size is held, and the sweep deletes the key again.
	r, put := e.presignAs(t, dev, storage.OpPut, snapName(2), 70)
	if r.status != 200 {
		t.Fatalf("presign: %d %s", r.status, r.body)
	}
	if r, _ := e.presignAs(t, dev, storage.OpDelete, snapName(2), 0); r.status != 200 {
		t.Fatalf("presign delete: %d %s", r.status, r.body)
	}
	if st, b := transfer(t, put, bytes.Repeat([]byte("z"), 70), 70); st != 200 {
		t.Fatalf("PUT with the old URL: %d %s", st, b)
	}
	if h := e.held(t, dev); h != 70 {
		t.Errorf("held %d after deleting a pending upload, want 70", h)
	}
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(3), 31); r.status != http.StatusRequestEntityTooLarge {
		t.Errorf("the deleted upload stopped counting: %d %s", r.status, r.body)
	}
	if r := e.commitAs(t, dev, snapName(2), 70); r.status != http.StatusConflict || r.code != "removed" {
		t.Errorf("commit of a deleted upload: %d %s", r.status, r.body)
	}
	e.clock.Add(presignTTL + uploadGrace + time.Minute)
	if err := e.s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if objs, _ := e.objects.List(t.Context(), storage.Prefix(ns)); len(objs) != 0 {
		t.Fatalf("the sweep left what the old URL put back: %+v", objs)
	}
	if h := e.held(t, dev); h != 0 {
		t.Errorf("held %d after the sweep, want 0", h)
	}
}

// Lapsed: nothing new is kept (402), and what is there can be listed and
// fetched for 90 days.
func TestLapsedReadsFor90Days(t *testing.T) {
	e := newEnv(t, true, nil)
	dev := newKey(t)
	e.link(t, dev, "brisk-heron-14", acme)
	e.bind(t, dev, newKey(t))
	e.upload(t, dev, snapName(1), []byte("ciphertext"))
	past := e.now().Add(-time.Hour).Format(time.RFC3339)
	if r := e.dev(t, "/dev/paid-through", `{"handle":"brisk-heron-14","paid_through":"`+past+`"}`); r.status != http.StatusNoContent {
		t.Fatalf("lapse: %d %s", r.status, r.body)
	}
	if r, _ := e.presignAs(t, dev, storage.OpPut, snapName(2), 1); r.status != http.StatusPaymentRequired || r.code != "lapsed" {
		t.Errorf("put while lapsed: %d %s", r.status, r.body)
	}
	e.clock.Add(89 * 24 * time.Hour)
	if r, l := e.list(t, dev); r.status != 200 || len(l.Objects) != 1 {
		t.Errorf("list 89 days on: %d %s", r.status, r.body)
	}
	r, get := e.presignAs(t, dev, storage.OpGet, snapName(1), 0)
	if r.status != 200 {
		t.Fatalf("get 89 days on: %d %s", r.status, r.body)
	}
	if st, b := transfer(t, get, nil, 0); st != 200 || string(b) != "ciphertext" {
		t.Errorf("GET: %d %q", st, b)
	}
	e.clock.Add(2 * 24 * time.Hour)
	if r, _ := e.list(t, dev); r.status != http.StatusPaymentRequired {
		t.Errorf("list 91 days on: %d %s", r.status, r.body)
	}
	if r, _ := e.presignAs(t, dev, storage.OpGet, snapName(1), 0); r.status != http.StatusPaymentRequired {
		t.Errorf("get 91 days on: %d %s", r.status, r.body)
	}
}

func TestRetention(t *testing.T) {
	now := time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC)
	var objs []store.StoredObject
	// A snapshot every day for a year, and a marker.
	for d := 0; d < 365; d++ {
		at := now.AddDate(0, 0, -d)
		objs = append(objs, store.StoredObject{Name: fmt.Sprintf("snap-%s-%08x.age", at.Format("20060102T150405Z"), d)})
	}
	objs = append(objs, store.StoredObject{Name: "handover-20260101T090000Z-5e6f7a8b.age"})
	future := fmt.Sprintf("snap-%s-00000000.age", now.AddDate(0, 0, 3).Format("20060102T150405Z"))
	objs = append(objs, store.StoredObject{Name: future})
	drops := retentionDrops(objs, "", now)
	if kept := len(objs) - len(drops); kept != 7+4+6+2 {
		t.Errorf("kept %d, want 7 daily, 4 weekly, 6 monthly, the marker and the future-dated one", kept)
	}
	for _, d := range drops {
		if strings.HasPrefix(d, "handover-") || d == future || d == objs[0].Name {
			t.Errorf("dropped %s", d)
		}
	}
	if len(retentionDrops(objs[:3], "", now)) != 0 {
		t.Error("three snapshots were pruned")
	}

	// What was just stored is kept, even beside a newer name the same day.
	var same []store.StoredObject
	for i := 1; i <= 4; i++ {
		same = append(same, store.StoredObject{Name: fmt.Sprintf("snap-20261231T120000Z-%08d.age", i)})
	}
	if d := retentionDrops(same, "", now); !slices.Equal(d, []string{same[1].Name, same[2].Name}) {
		t.Errorf("drops %v", d)
	}
	if d := retentionDrops(same, same[1].Name, now); !slices.Equal(d, []string{same[2].Name}) {
		t.Errorf("drops %v, want only the other one", d)
	}

	// Commit prunes, in the store and the records.
	e := newEnv(t, true, nil)
	dev := newKey(t)
	e.link(t, dev, "brisk-heron-15", acme)
	ns, _ := e.bind(t, dev, newKey(t))
	for i := 1; i <= 20; i++ {
		e.upload(t, dev, snapName(i), []byte("x"))
	}
	_, l := e.list(t, dev)
	if len(l.Objects) >= 20 || !slices.Contains(l.names(), snapName(20)) || l.Bytes != int64(len(l.Objects)) {
		t.Fatalf("after 20 daily snapshots: %v, %d bytes", l.names(), l.Bytes)
	}
	stored, _ := e.objects.List(t.Context(), storage.Prefix(ns))
	if len(stored) != len(l.Objects) {
		t.Fatalf("the store holds %d objects, the records %d", len(stored), len(l.Objects))
	}
}

// fakeRelay polls the deny list as a hosted relay does: fetched, verified
// with the dl-* keys, and applied only if its seq is newer.
type fakeRelay struct {
	e    *env
	seq  int64
	keys map[string]bool
}

func (f *fakeRelay) poll(t *testing.T) {
	l := f.e.denyList(t)
	if l.Seq <= f.seq {
		return
	}
	f.seq, f.keys = l.Seq, map[string]bool{}
	for _, k := range l.Keys {
		f.keys[k.Value] = true
	}
}

func (f *fakeRelay) admits(key ed25519.PrivateKey) bool { return !f.keys[pubOf(key)] }

func (e *env) recoverAs(t *testing.T, dev, rec ed25519.PrivateKey, ns, acmeURI string, ts time.Time) resp {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"ns": ns, "device_pub": pubOf(dev), "acme_account": acmeURI, "ts": ts.UTC().Format(time.RFC3339),
		"sig_recovery": sig(rec, entitle.RecoverMessage(ns, pubOf(dev), acmeURI, ts.Truncate(time.Second)))})
	return send(t, e.signed(t, dev, "POST", "/v1/recover", b))
}

// Recovery from the 12 words on a new machine: the generation goes up, the
// old machine's key is deny-listed (a relay refuses it at its next poll)
// and hears 409 superseded, the handle's CAA names the new ACME account,
// and the new key gets an entitlement and the backups.
func TestRecover(t *testing.T) {
	e := newEnv(t, false, nil)
	old, rec := newKey(t), newKey(t)
	e.link(t, old, "brisk-heron-16", acme)
	ns, _ := e.bind(t, old, rec)
	e.upload(t, old, snapName(1), []byte("ciphertext"))
	relay := &fakeRelay{e: e}
	relay.poll(t)
	if !relay.admits(old) {
		t.Fatal("the relay refuses the old key before any recovery")
	}

	fresh := newKey(t)
	const newACME = "https://acme-v02.api.letsencrypt.org/acme/acct/987654321"
	// The wrong words: their namespace is bound nowhere.
	wrong := newKey(t)
	if r := e.recoverAs(t, fresh, wrong, entitle.Namespace(wrong.Public().(ed25519.PublicKey)), newACME, time.Now()); r.status != http.StatusUnauthorized {
		t.Errorf("wrong words: %d %s", r.status, r.body)
	}
	// The right namespace, signed by other words.
	if r := e.recoverAs(t, fresh, wrong, ns, newACME, time.Now()); r.status != http.StatusUnauthorized {
		t.Errorf("a signature the words didn't make: %d %s", r.status, r.body)
	}
	if r := e.recoverAs(t, fresh, rec, ns, newACME, time.Now().Add(-10*time.Minute)); r.status != http.StatusUnauthorized {
		t.Errorf("a stale recovery: %d %s", r.status, r.body)
	}
	// Signed by a key other than the one it names.
	b, _ := json.Marshal(map[string]string{"ns": ns, "device_pub": pubOf(fresh), "acme_account": "", "ts": time.Now().UTC().Format(time.RFC3339),
		"sig_recovery": sig(rec, entitle.RecoverMessage(ns, pubOf(fresh), "", time.Now().Truncate(time.Second)))})
	if r := send(t, e.signed(t, newKey(t), "POST", "/v1/recover", b)); r.status != http.StatusUnauthorized {
		t.Errorf("another key's request: %d %s", r.status, r.body)
	}
	if h := e.handle(t, "brisk-heron-16"); h.Gen != 1 {
		t.Fatalf("refused recoveries moved the generation to %d", h.Gen)
	}

	r := e.recoverAs(t, fresh, rec, ns, newACME, time.Now())
	if r.status != 200 {
		t.Fatalf("recover: %d %s", r.status, r.body)
	}
	var out struct {
		Entitlement string `json:"entitlement"`
		Handle      string `json:"handle"`
		Gen         int64  `json:"gen"`
	}
	r.json(t, &out)
	ent, _ := e.fetchKeys(t)
	cl, err := entitle.Verify(out.Entitlement, ent, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cl.Cnf != pubOf(fresh) || cl.Gen != 2 || cl.Handle != "brisk-heron-16" || out.Handle != cl.Handle || out.Gen != 2 {
		t.Fatalf("entitlement %+v, answer %+v", cl, out)
	}
	// The CAA record now pins the new machine's ACME account.
	recs, _ := e.dns.Records("brisk-heron-16")
	if len(recs.CAA) == 0 || !strings.Contains(recs.CAA[0].Value, "accounturi="+newACME) {
		t.Errorf("CAA %v", recs.CAA)
	}
	// A relay refuses the old key at its next poll.
	relay.poll(t)
	if relay.admits(old) || !relay.admits(fresh) {
		t.Error("after one poll the relay still admits the old key, or refuses the new one")
	}
	// The old machine hears superseded, and can't touch the backups.
	if r := send(t, e.signed(t, old, "POST", "/v1/entitlement/refresh", nil)); r.status != http.StatusConflict || r.code != "superseded" {
		t.Errorf("old refresh: %d %s", r.status, r.body)
	}
	if r, _ := e.presignAs(t, old, storage.OpGet, snapName(1), 0); r.status != http.StatusConflict || r.code != "superseded" {
		t.Errorf("old presign: %d %s", r.status, r.body)
	}
	// The new one lists and fetches them.
	if _, l := e.list(t, fresh); !slices.Equal(l.names(), []string{snapName(1)}) {
		t.Errorf("new machine lists %v", l.names())
	}
	// Routes that don't ask for the current generation answer the same, and
	// run nothing: an unlink from the old machine doesn't revoke it, so it
	// still hears superseded rather than revoked.
	if r := send(t, e.signed(t, old, "DELETE", "/v1/device", nil)); r.status != http.StatusConflict || r.code != "superseded" {
		t.Errorf("old unlink: %d %s", r.status, r.body)
	}
	if r := send(t, e.signed(t, old, "GET", "/v1/me", nil)); r.status != http.StatusConflict || r.code != "superseded" {
		t.Errorf("old me after its unlink: %d %s", r.status, r.body)
	}
	// The same recovery again (a lost answer) moves nothing.
	if r := e.recoverAs(t, fresh, rec, ns, newACME, time.Now()); r.status != 200 || e.handle(t, "brisk-heron-16").Gen != 2 {
		t.Errorf("a repeated recovery: %d %s", r.status, r.body)
	}
	// The old key can't recover back without the words' signature for it,
	// and with it, it is refused: it is on the deny list.
	if r := e.recoverAs(t, old, rec, ns, "", time.Now()); r.status != http.StatusForbidden {
		t.Errorf("the old key recovering: %d %s", r.status, r.body)
	}
}

func (e *env) handle(t *testing.T, name string) store.Handle {
	t.Helper()
	var h store.Handle
	e.store.View(t.Context(), func(tx *store.Tx) error {
		var err error
		h, err = tx.Handle(name)
		return err
	})
	return h
}

// A recovery while lapsed still moves the account, so the new machine can
// fetch its backups, but brings no entitlement.
func TestRecoverWhileLapsed(t *testing.T) {
	e := newEnv(t, false, nil)
	old, rec := newKey(t), newKey(t)
	e.link(t, old, "brisk-heron-17", acme)
	ns, _ := e.bind(t, old, rec)
	e.upload(t, old, snapName(1), []byte("ciphertext"))
	e.dev(t, "/dev/paid-through", `{"handle":"brisk-heron-17","paid_through":"`+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+`"}`)
	fresh := newKey(t)
	r := e.recoverAs(t, fresh, rec, ns, "", time.Now())
	var out map[string]any
	r.json(t, &out)
	if r.status != 200 || out["entitlement"] != nil || out["handle"] != "brisk-heron-17" || out["paid_through"] == nil {
		t.Fatalf("recover while lapsed: %d %s", r.status, r.body)
	}
	if _, l := e.list(t, fresh); len(l.Objects) != 1 {
		t.Errorf("list %+v", l)
	}
	recs, _ := e.dns.Records("brisk-heron-17")
	if len(recs.CAA) == 0 || recs.CAA[0].Value != ";" {
		t.Errorf("with no ACME account named, the CAA record should let nobody issue: %v", recs.CAA)
	}
}

// A deleted account's backups leave storage with its rows.
func TestDeletionRemovesBackups(t *testing.T) {
	e := newEnv(t, true, nil)
	dev := newKey(t)
	e.link(t, dev, "brisk-heron-18", acme)
	ns, _ := e.bind(t, dev, newKey(t))
	e.upload(t, dev, snapName(1), []byte("ciphertext"))
	if r := send(t, e.signed(t, dev, "DELETE", "/v1/account", nil)); r.status != http.StatusAccepted {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	e.clock.Add(8 * 24 * time.Hour)
	if err := e.s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if objs, _ := e.objects.List(t.Context(), storage.Prefix(ns)); len(objs) != 0 {
		t.Fatalf("left %v", objs)
	}
	e.store.View(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Namespace(ns); err == nil {
			t.Error("the namespace row stayed")
		}
		return nil
	})
}
