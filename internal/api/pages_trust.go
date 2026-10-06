package api

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The Trust page: what leaves this computer (the list in
// docs/threat-model.md, "Outbound connections", marked on or off for this
// twin), the certificate other devices see and whether the public logs
// hold one that isn't this machine's, and how to check all of it yourself.

// Outbound is one kind of connection the twin can make.
type Outbound struct {
	ID    string `json:"id"`
	What  string `json:"what"`  // who it talks to, in a few words
	When  string `json:"when"`  // what makes it happen
	Data  string `json:"data"`  // what they get
	On    bool   `json:"on"`    // whether this twin makes it
	Where string `json:"where"` // this twin's destination, when on
}

// OutboundCatalog is every kind of connection the twin can make, all off:
// the twin marks the ones it makes. It follows docs/threat-model.md.
func OutboundCatalog() []Outbound {
	return []Outbound{
		{ID: "model", What: "Your model provider", When: "Every reply, routine and background task, and an hourly check",
			Data: "Your messages, the memories relevant to them and what tools found. A local model (Ollama) keeps this on this computer."},
		{ID: "weather", What: "Open-Meteo, for the weather", When: "The presence screen, every 15 minutes",
			Data: "Rough coordinates for your city. No messages or memories."},
		{ID: "google", What: "Google", When: "Every few minutes for mail and calendar, and when a task uses them",
			Data: "Requests to your own Calendar, Gmail and Drive, which identify your account."},
		{ID: "channels", What: "Your messaging apps", When: "While each channel is connected",
			Data: "The messages you exchange with your twin there, and their attachments."},
		{ID: "web", What: "Websites your twin reads", When: "Only when a task opens a page or fetches a link",
			Data: "The requests any browser sends. Chrome's own background services may also contact Google."},
		{ID: "voice", What: "ElevenLabs and Twilio", When: "Only if you set up online voice or phone calls",
			Data: "Text to speak (ElevenLabs); calls and numbers (Twilio). Local voice sends nothing."},
		{ID: "backup", What: "Your backup destination", When: "Nightly at 03:30, and soon after a device or passkey changes",
			Data: "Encrypted snapshots only. The 12 words that open them never leave your paper."},
		{ID: "push", What: "Apple, Google, Mozilla or Microsoft push services", When: "When a paired phone has notifications on",
			Data: "Encrypted notifications. The service sees their size and timing, not what they say."},
		{ID: "tailscale", What: "Tailscale", When: "While other devices reach your twin over your tailnet",
			Data: "The Tailscale app's own traffic, and a certificate for this computer's ts.net name, which is published in public logs."},
		{ID: "relay", What: "Your own relay", When: "While other devices reach your twin through it",
			Data: "Encrypted connections: the relay passes them on and can't read them. It sees addresses and timing."},
		{ID: "certificates", What: "Let's Encrypt, certificate logs and DNS resolvers", When: "When the certificate renews, and every 6 hours",
			Data: "Your twin's public name and public key. Logs (Cert Spotter, crt.sh) and resolvers (1.1.1.1, 8.8.8.8, 9.9.9.9) are asked about that name."},
		{ID: "updates", What: "GitHub", When: "Only when you run mirrin update",
			Data: "A request for the latest release. Your twin never checks for updates by itself."},
		{ID: "linked", What: "The service this computer is linked to", When: "Once a day, while linked",
			Data: "Signed requests carrying public keys and backup sizes; nothing you said."},
	}
}

// CertInfo is the certificate other devices see at one address.
type CertInfo struct {
	Route string `json:"route"` // tailscale, files or relay
	Host  string `json:"host"`
	Port  int    `json:"port"` // the listener's port (443 when the address has none)
	// Current and Next are SPKI pins (base64url SHA-256 of the public key):
	// the key in use and the one it will move to.
	Current string `json:"current,omitempty"`
	Next    string `json:"next,omitempty"`
	// SHA256 is the certificate's own fingerprint, as browsers show it.
	SHA256   string    `json:"sha256,omitempty"`
	Issuer   string    `json:"issuer,omitempty"`
	NotAfter time.Time `json:"not_after,omitzero"`
}

// CTStatus is the certificate log watch (relay mode).
type CTStatus struct {
	Checked     time.Time `json:"checked,omitzero"`
	SourcesUp   int       `json:"sources_up"`
	SourcesDown []string  `json:"sources_down,omitempty"`
	CAAProblem  string    `json:"caa_problem,omitempty"`
	// State is ok, warn or fail, with the self-check's words.
	State  string `json:"state"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
	// Alarm is set while the certificate alarm holds approvals.
	Alarm string `json:"alarm,omitempty"`
}

// TrustInfo is what the Trust page shows.
type TrustInfo struct {
	Outbound []Outbound `json:"outbound"`
	Certs    []CertInfo `json:"certs"`
	CT       *CTStatus  `json:"ct,omitempty"`
}

// TrustBackend says what this twin sends where, and what it presents.
type TrustBackend interface {
	Trust(ctx context.Context) TrustInfo
}

// remoteLeaf is how a remote listener's certificate is found.
type remoteLeaf struct {
	via string
	src interface {
		GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
	}
}

// remoteLeaf records where a remote listener's certificate comes from.
// Callers hold lmu.
func (s *Server) remoteLeaf(url, via string, src interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}) {
	if s.remoteLeaves == nil {
		s.remoteLeaves = map[string]remoteLeaf{}
	}
	s.remoteLeaves[url] = remoteLeaf{via: via, src: src}
}

// RemoteBases are the TLS listeners other devices reach now, with their
// pins.
func (s *Server) RemoteBases() []Base { return s.remoteBases() }

// Fingerprint is a certificate's SHA-256 as browsers show it: pairs of
// hex digits with colons.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

// RemoteCerts describes the certificate each remote listener presents.
func (s *Server) RemoteCerts() []CertInfo {
	bases := s.remoteBases()
	s.lmu.Lock()
	leaves := make(map[string]remoteLeaf, len(s.remoteLeaves))
	for k, v := range s.remoteLeaves {
		leaves[k] = v
	}
	s.lmu.Unlock()
	out := []CertInfo{}
	for _, b := range bases {
		c := CertInfo{Host: strings.TrimPrefix(b.URL, "https://"), Port: 443}
		if h, p, err := net.SplitHostPort(c.Host); err == nil {
			c.Host = h
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				c.Port = n
			}
		}
		if len(b.Pins) > 0 {
			c.Current = b.Pins[0]
		}
		if len(b.Pins) > 1 {
			c.Next = b.Pins[1]
		}
		if l, ok := leaves[b.URL]; ok {
			c.Route = l.via
			if tc, err := l.src.GetCertificate(nil); err == nil && tc != nil {
				leaf := tc.Leaf
				if leaf == nil && len(tc.Certificate) > 0 {
					leaf, _ = x509.ParseCertificate(tc.Certificate[0])
				}
				if leaf != nil {
					c.SHA256, c.Issuer, c.NotAfter = Fingerprint(leaf), leaf.Issuer.CommonName, leaf.NotAfter
					if c.Current == "" {
						c.Current = SPKIPin(leaf)
					}
				}
			}
		}
		out = append(out, c)
	}
	return out
}

func (s *Server) trustRoutes(mux *http.ServeMux, a Authz) {
	mux.HandleFunc("GET /trust/info", a.Local(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		info := s.pages.Trust(ctx)
		if info.Outbound == nil {
			info.Outbound = OutboundCatalog()
		}
		if info.Certs == nil {
			info.Certs = s.RemoteCerts()
		}
		writeJSON(w, info)
	}))
}
