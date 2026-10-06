package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake keeps objects in a directory and serves them at presigned URLs of its
// own: base/<key>?op=…&size=…&exp=…&sig=…, where sig is an HMAC-SHA256 of
// the op, key, size and expiry under a secret only the Fake holds. Like a
// bucket with presigned URLs, it checks each request against what was
// signed, and more strictly than R2 can: a PUT must carry exactly the size
// signed, as its Content-Length and its body.
type Fake struct {
	dir    string
	base   *url.URL
	secret []byte
	// Now is the clock URLs expire by; nil is time.Now.
	Now func() time.Time

	mu sync.Mutex
}

// NewFake keeps objects in dir and signs URLs under base, such as
// http://127.0.0.1:8787/storage, where Fake must be served (Handler).
func NewFake(dir, base string) (*Fake, error) {
	u, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" {
		return nil, fmt.Errorf("storage: fake base %q is not an http(s) URL", base)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	rand.Read(secret)
	return &Fake{dir: dir, base: u, secret: secret}, nil
}

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Path is the URL path the Fake is served under, such as /storage/.
func (f *Fake) Path() string { return f.base.Path + "/" }

func (f *Fake) sign(op, key string, size, exp int64) string {
	m := hmac.New(sha256.New, f.secret)
	fmt.Fprintf(m, "%s\n%s\n%d\n%d", op, key, size, exp)
	return hex.EncodeToString(m.Sum(nil))
}

// Presign implements Provider.
func (f *Fake) Presign(_ context.Context, op, key string, size int64, ttl time.Duration) (Signed, error) {
	method, err := methodOf(op)
	if err != nil {
		return Signed{}, err
	}
	if !validKey(key) || size < 0 {
		return Signed{}, fmt.Errorf("storage: bad key %q or size %d", key, size)
	}
	if op != OpPut {
		size = 0
	}
	exp := f.now().Add(ttl).UTC().Truncate(time.Second)
	q := url.Values{"op": {op}, "size": {strconv.FormatInt(size, 10)}, "exp": {strconv.FormatInt(exp.Unix(), 10)}, "sig": {f.sign(op, key, size, exp.Unix())}}
	u := *f.base
	u.Path += "/" + key
	u.RawQuery = q.Encode()
	s := Signed{URL: u.String(), Method: method, Headers: map[string]string{}, Expires: exp}
	if op == OpPut {
		s.Headers["Content-Length"] = strconv.FormatInt(size, 10)
	}
	return s, nil
}

// validKey accepts ns/<ns>/<name> and nothing else.
func validKey(key string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 3 && parts[0] == "ns" && ValidNamespace(parts[1]) && ValidName(parts[2])
}

func (f *Fake) file(key string) string { return filepath.Join(f.dir, filepath.FromSlash(key)) }

// ServeHTTP serves the presigned requests.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.URL.Path, f.Path())
	q := r.URL.Query()
	op := q.Get("op")
	method, err := methodOf(op)
	size, serr := strconv.ParseInt(q.Get("size"), 10, 64)
	exp, eerr := strconv.ParseInt(q.Get("exp"), 10, 64)
	sig, _ := hex.DecodeString(q.Get("sig"))
	want, _ := hex.DecodeString(f.sign(op, key, size, exp))
	switch {
	case !ok || !validKey(key) || err != nil || serr != nil || eerr != nil || !hmac.Equal(sig, want):
		storageError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	case r.Method != method:
		storageError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	case f.now().Unix() > exp:
		storageError(w, http.StatusForbidden, "AccessDenied: Request has expired")
		return
	}
	switch op {
	case OpPut:
		if r.ContentLength != size {
			storageError(w, http.StatusBadRequest, "BadRequest: Content-Length must be the size signed")
			return
		}
		if err := f.put(key, r.Body, size); err != nil {
			storageError(w, http.StatusBadRequest, "IncompleteBody: "+err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	case OpGet:
		fh, err := os.Open(f.file(key))
		if err != nil {
			storageError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		defer fh.Close()
		st, err := fh.Stat()
		if err != nil {
			storageError(w, http.StatusInternalServerError, "InternalError")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		io.Copy(w, fh)
	case OpDelete:
		f.mu.Lock()
		os.Remove(f.file(key))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

func storageError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, msg+"\n")
}

// put writes exactly size bytes from body to key: to a temporary file, then
// into place, so nothing half-written is ever at key.
func (f *Fake) put(key string, body io.Reader, size int64) error {
	p := f.file(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".put-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(body, size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%d bytes arrived, %d were signed", n, size)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return os.Rename(tmp.Name(), p)
}

// Head implements Provider.
func (f *Fake) Head(_ context.Context, key string) (int64, error) {
	if !validKey(key) {
		return 0, ErrNotFound
	}
	st, err := os.Stat(f.file(key))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// List implements Provider.
func (f *Fake) List(_ context.Context, prefix string) ([]Obj, error) {
	var out []Obj
	err := filepath.WalkDir(f.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".put-") {
			return nil
		}
		rel, err := filepath.Rel(f.dir, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, Obj{Key: key, Size: info.Size(), Modified: info.ModTime().UTC()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, err
}

// DeletePrefix implements Provider.
func (f *Fake) DeletePrefix(ctx context.Context, prefix string) error {
	objs, err := f.List(ctx, prefix)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range objs {
		if err := os.Remove(f.file(o.Key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Contains reports whether any byte the Fake holds on disk includes needle,
// for tests that a store never sees plain text.
func (f *Fake) Contains(needle []byte) bool {
	found := false
	filepath.WalkDir(f.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, needle) {
			found = true
		}
		return nil
	})
	return found
}
