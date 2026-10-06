package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MavrkAI/Mirrin/internal/sigv4"
)

// R2 keeps objects in one Cloudflare R2 bucket (any S3-compatible store
// works), path-style: Endpoint/Bucket/<key>. Presigned URLs carry a SigV4
// signature in their query (internal/sigv4). A PUT's URL signs its
// Content-Length as well as the host, so R2 refuses a body of any other
// length; the control plane's HEAD before it counts an object checks again.
type R2 struct {
	// Endpoint is https://<account>.r2.cloudflarestorage.com.
	Endpoint string
	Bucket   string
	// Region is "auto" for R2.
	Region string
	Creds  sigv4.Creds
	// HTTP sends Head, List and DeletePrefix; nil is a client with a 30 s
	// timeout.
	HTTP *http.Client
	Now  func() time.Time
}

func (r *R2) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *R2) region() string {
	if r.Region == "" {
		return "auto"
	}
	return r.Region
}

func (r *R2) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// url is the object's URL, or the bucket's for key "".
func (r *R2) url(key string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(r.Endpoint, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || r.Bucket == "" {
		return "", fmt.Errorf("storage: r2 endpoint %q or bucket %q is not usable", r.Endpoint, r.Bucket)
	}
	u.Path = "/" + r.Bucket
	if key != "" {
		u.Path += "/" + key
	}
	return u.String(), nil
}

// Presign implements Provider.
func (r *R2) Presign(_ context.Context, op, key string, size int64, ttl time.Duration) (Signed, error) {
	method, err := methodOf(op)
	if err != nil {
		return Signed{}, err
	}
	if !validKey(key) || size < 0 {
		return Signed{}, fmt.Errorf("storage: bad key %q or size %d", key, size)
	}
	raw, err := r.url(key)
	if err != nil {
		return Signed{}, err
	}
	now := r.now().UTC().Truncate(time.Second)
	var u string
	if op == OpPut {
		u, err = sigv4.PresignSized(method, raw, r.Creds, r.region(), "s3", ttl, now, size)
	} else {
		u, err = sigv4.Presign(method, raw, r.Creds, r.region(), "s3", ttl, now)
	}
	if err != nil {
		return Signed{}, err
	}
	s := Signed{URL: u, Method: method, Headers: map[string]string{}, Expires: now.Add(ttl)}
	if op == OpPut {
		s.Headers["Content-Length"] = strconv.FormatInt(size, 10)
	}
	return s, nil
}

// do sends one signed request with no body.
func (r *R2) do(ctx context.Context, method, raw string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, raw, nil)
	if err != nil {
		return nil, err
	}
	if err := sigv4.Sign(req, r.Creds, r.region(), "s3", r.now(), sigv4.EmptyPayloadHash); err != nil {
		return nil, err
	}
	return r.client().Do(req)
}

// Head implements Provider.
func (r *R2) Head(ctx context.Context, key string) (int64, error) {
	if !validKey(key) {
		return 0, ErrNotFound
	}
	raw, err := r.url(key)
	if err != nil {
		return 0, err
	}
	res, err := r.do(ctx, http.MethodHead, raw)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound:
		return 0, ErrNotFound
	case res.StatusCode != http.StatusOK:
		return 0, fmt.Errorf("storage: HEAD %s: %s", key, res.Status)
	case res.ContentLength < 0:
		return 0, fmt.Errorf("storage: HEAD %s: no Content-Length", key)
	}
	return res.ContentLength, nil
}

// listResult is the part of ListObjectsV2's answer List reads.
type listResult struct {
	Contents []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// List implements Provider, a page of ListObjectsV2 at a time.
func (r *R2) List(ctx context.Context, prefix string) ([]Obj, error) {
	base, err := r.url("")
	if err != nil {
		return nil, err
	}
	var out []Obj
	token := ""
	for page := 0; ; page++ {
		if page > 10000 {
			return nil, errors.New("storage: list never ended")
		}
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		res, err := r.do(ctx, http.MethodGet, base+"?"+q.Encode())
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("storage: list %s: %s", prefix, res.Status)
		}
		var lr listResult
		if err := xml.Unmarshal(b, &lr); err != nil {
			return nil, fmt.Errorf("storage: list %s: %w", prefix, err)
		}
		for _, c := range lr.Contents {
			out = append(out, Obj{Key: c.Key, Size: c.Size, Modified: c.LastModified.UTC()})
		}
		if !lr.IsTruncated || lr.NextContinuationToken == "" {
			return out, nil
		}
		token = lr.NextContinuationToken
	}
}

// DeletePrefix implements Provider, one DELETE per object.
func (r *R2) DeletePrefix(ctx context.Context, prefix string) error {
	if !strings.HasPrefix(prefix, "ns/") {
		return fmt.Errorf("storage: refusing to delete outside ns/: %q", prefix)
	}
	objs, err := r.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, o := range objs {
		raw, err := r.url(o.Key)
		if err != nil {
			return err
		}
		res, err := r.do(ctx, http.MethodDelete, raw)
		if err != nil {
			return err
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNotFound {
			return fmt.Errorf("storage: DELETE %s: %s", o.Key, res.Status)
		}
	}
	return nil
}
