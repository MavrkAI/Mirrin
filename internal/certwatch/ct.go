package certwatch

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Issuance is one certificate a CT log shows for a watched name.
type Issuance struct {
	Source    string    `json:"source"`
	ID        string    `json:"id"`
	DNSNames  []string  `json:"dns_names"`
	SPKI      string    `json:"spki"` // unpadded base64url SHA-256 of the SubjectPublicKeyInfo
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	Issuer    string    `json:"issuer,omitempty"`
}

// CTSource lists the certificates logged for a host, wildcards that cover
// it included.
type CTSource interface {
	Issuances(ctx context.Context, host string) ([]Issuance, error)
}

// Named is a CTSource with a name to show the owner.
type Named interface{ Name() string }

func sourceName(s CTSource) string {
	if n, ok := s.(Named); ok {
		return n.Name()
	}
	return fmt.Sprintf("%T", s)
}

const maxBody = 8 << 20

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func get(ctx context.Context, c *http.Client, u string, h map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mirrin-certwatch")
	for k, v := range h {
		req.Header.Set(k, v)
	}
	res, err := client(c).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, res.Status)
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("%s sent too much", req.URL.Host)
	}
	return b, nil
}

// CertSpotter asks SSLMate's Cert Spotter API, with match_wildcards so a
// *.parent certificate that covers the host is listed too.
type CertSpotter struct {
	BaseURL string // default https://api.certspotter.com
	Token   string // optional API key
	Client  *http.Client
}

func (*CertSpotter) Name() string { return "Cert Spotter" }

// maxPages bounds one poll; a name with more certificates than this is
// either very old or under attack, and the rest come on the next poll.
const maxPages = 20

func (c *CertSpotter) Issuances(ctx context.Context, host string) ([]Issuance, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.certspotter.com"
	}
	var h map[string]string
	if c.Token != "" {
		h = map[string]string{"Authorization": "Bearer " + c.Token}
	}
	var out []Issuance
	after := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{"domain": {host}, "match_wildcards": {"true"}, "expand": {"dns_names", "issuer"}}
		if after != "" {
			q.Set("after", after)
		}
		b, err := get(ctx, c.Client, base+"/v1/issuances?"+q.Encode(), h)
		if err != nil {
			return nil, err
		}
		var rows []struct {
			ID           string    `json:"id"`
			DNSNames     []string  `json:"dns_names"`
			PubkeySHA256 string    `json:"pubkey_sha256"`
			NotBefore    time.Time `json:"not_before"`
			NotAfter     time.Time `json:"not_after"`
			Issuer       *struct {
				FriendlyName string `json:"friendly_name"`
				Name         string `json:"name"`
			} `json:"issuer"`
		}
		if err := json.Unmarshal(b, &rows); err != nil {
			return nil, fmt.Errorf("Cert Spotter sent something unexpected: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			sum, err := hex.DecodeString(r.PubkeySHA256)
			if err != nil || len(sum) != sha256.Size {
				return nil, fmt.Errorf("Cert Spotter issuance %s has no key hash", r.ID)
			}
			is := Issuance{Source: c.Name(), ID: r.ID, DNSNames: r.DNSNames, SPKI: base64.RawURLEncoding.EncodeToString(sum), NotBefore: r.NotBefore, NotAfter: r.NotAfter}
			if r.Issuer != nil {
				is.Issuer = r.Issuer.FriendlyName
				if is.Issuer == "" {
					is.Issuer = r.Issuer.Name
				}
			}
			out = append(out, is)
		}
		after = rows[len(rows)-1].ID
	}
	return out, nil
}

// CrtSh asks crt.sh for the exact name and for *.parent. Its JSON has no
// key hash, so each certificate newer than NotBefore is downloaded once to
// read its key.
type CrtSh struct {
	BaseURL string // default https://crt.sh
	Client  *http.Client
	// NotBefore, if set, skips downloading certificates older than it
	// (the watcher's checkpoint): they can't raise an alarm.
	NotBefore func() time.Time
	// MaxFetch bounds downloads per poll. Zero is 50.
	MaxFetch int

	mu    sync.Mutex
	spkis map[string]string // crt.sh id → SPKI
}

func (*CrtSh) Name() string { return "crt.sh" }

func (c *CrtSh) Issuances(ctx context.Context, host string) ([]Issuance, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://crt.sh"
	}
	queries := []string{host}
	if w := wildcardFor(host); w != "" {
		queries = append(queries, w)
	}
	var since time.Time
	if c.NotBefore != nil {
		since = c.NotBefore()
	}
	limit := c.MaxFetch
	if limit <= 0 {
		limit = 50
	}
	seen := map[string]bool{}
	var out []Issuance
	fetched := 0
	for _, q := range queries {
		b, err := get(ctx, c.Client, base+"/?"+url.Values{"q": {q}, "output": {"json"}, "deduplicate": {"Y"}}.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var rows []struct {
			ID         int64  `json:"id"`
			IssuerName string `json:"issuer_name"`
			NameValue  string `json:"name_value"`
			NotBefore  string `json:"not_before"`
			NotAfter   string `json:"not_after"`
		}
		if err := json.Unmarshal(b, &rows); err != nil {
			return nil, fmt.Errorf("crt.sh sent something unexpected: %w", err)
		}
		for _, r := range rows {
			id := strconv.FormatInt(r.ID, 10)
			names := strings.Fields(strings.ToLower(r.NameValue))
			if seen[id] || !Covers(names, host) {
				continue
			}
			seen[id] = true
			nb, err1 := parseCrtShTime(r.NotBefore)
			na, err2 := parseCrtShTime(r.NotAfter)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("crt.sh entry %s has a bad date", id)
			}
			is := Issuance{Source: c.Name(), ID: id, DNSNames: names, NotBefore: nb, NotAfter: na, Issuer: r.IssuerName}
			if nb.After(since) {
				if spki, ok := c.cached(id); ok {
					is.SPKI = spki
				} else {
					if fetched >= limit {
						return nil, errors.New("crt.sh lists more new certificates than one poll reads; the rest come next time")
					}
					fetched++
					if is.SPKI, err = c.download(ctx, base, id); err != nil {
						return nil, err
					}
				}
			}
			out = append(out, is)
		}
	}
	return out, nil
}

func (c *CrtSh) cached(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.spkis[id]
	return s, ok
}

func (c *CrtSh) download(ctx context.Context, base, id string) (string, error) {
	b, err := get(ctx, c.Client, base+"/?d="+url.QueryEscape(id), nil)
	if err != nil {
		return "", err
	}
	der := b
	if blk, _ := pem.Decode(b); blk != nil {
		der = blk.Bytes
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", fmt.Errorf("crt.sh certificate %s: %w", id, err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	spki := base64.RawURLEncoding.EncodeToString(sum[:])
	c.mu.Lock()
	if c.spkis == nil {
		c.spkis = map[string]string{}
	}
	c.spkis[id] = spki
	c.mu.Unlock()
	return spki, nil
}

func parseCrtShTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04:05", time.RFC3339, "2006-01-02T15:04:05.999999"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q", s)
}

// wildcardFor is the wildcard that would cover host: *.parent, when the
// parent itself has a dot (no CA issues *.com).
func wildcardFor(host string) string {
	_, parent, ok := strings.Cut(host, ".")
	if !ok || !strings.Contains(parent, ".") {
		return ""
	}
	return "*." + parent
}

// Covers reports whether a certificate for names is valid for host: the
// exact name, or a wildcard one label up.
func Covers(names []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	w := wildcardFor(host)
	for _, n := range names {
		n = strings.ToLower(strings.TrimSuffix(n, "."))
		if n == host || w != "" && n == w {
			return true
		}
	}
	return false
}
