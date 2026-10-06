package logs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// Hidden replaces a secret.
const Hidden = "[hidden]"

// secretPatterns match credentials by their shape, wherever they appear: in
// an error that quotes a URL, a header, a config value.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{8,}`),                                     // Anthropic
	regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{16,}`),                     // OpenAI and compatible
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{30,}`),                                         // Google API keys
	regexp.MustCompile(`ya29\.[0-9A-Za-z_-]{20,}`),                                       // Google OAuth access tokens
	regexp.MustCompile(`1//[0-9A-Za-z_-]{30,}`),                                          // Google OAuth refresh tokens
	regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`),                                   // Slack bot and user tokens
	regexp.MustCompile(`xapp-[A-Za-z0-9-]{10,}`),                                         // Slack app tokens
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`),        // GitHub
	regexp.MustCompile(`\d{6,12}:[A-Za-z0-9_-]{30,}`),                                    // Telegram bot tokens (also inside api.telegram.org/bot… URLs)
	regexp.MustCompile(`\b[MNO][A-Za-z\d_-]{23,27}\.[A-Za-z\d_-]{6}\.[A-Za-z\d_-]{27,}`), // Discord bot tokens
	regexp.MustCompile(`abt1_[0-9a-f]{16}_[A-Za-z0-9_-]{20,}`),                           // Mirrin device tokens
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),     // JWTs
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                           // AWS access key ids
	regexp.MustCompile(`\bAC[0-9a-f]{32}\b`),                                             // Twilio account ids
	regexp.MustCompile(`AGE-SECRET-KEY-(?:PQ-)?1[0-9A-Z]{20,}`),                          // age identities (mirrin backup key --age)
}

// secretContexts keep a label and hide what follows it: "Bearer …",
// "token=…", "password: …", "Authorization: …", user:password@ in URLs.
var secretContexts = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?(?:basic\s+|bearer\s+)?)[^\s"',]{6,}`),
	regexp.MustCompile(`(?i)([?&](?:access_token|refresh_token|token|key|api_key|apikey|auth|password|secret|sig|signature|client_secret)=)[^&\s"']+`),
	regexp.MustCompile(`(?i)(\b(?:api[_-]?key|access[_-]?token|refresh[_-]?token|auth[_-]?token|client[_-]?secret|secret|password|passwd)["']?\s*[:=]\s*["']?)[^\s"',}&]{4,}`),
	regexp.MustCompile(`(://[^/\s:@]+:)[^@\s/]+(@)`),
}

// Redactor takes secrets out of text: every value the owner saved as a
// secret (secrets.env, the keys in the environment, the API token), and
// anything shaped like a credential.
type Redactor struct {
	mu      sync.Mutex
	known   []string
	checked time.Time
	load    func() []string
}

// NewRedactor makes a redactor for the Mirrin home. It learns secrets saved
// later (from the menu, say) within half a minute.
func NewRedactor(home string) *Redactor {
	return &Redactor{load: func() []string { return knownSecrets(home) }}
}

// NewRedactorWith makes a redactor that also hides the given values.
func NewRedactorWith(values ...string) *Redactor {
	vals := append([]string(nil), values...)
	return &Redactor{load: func() []string { return vals }}
}

// String returns s with every secret replaced by [hidden].
func (r *Redactor) String(s string) string {
	if s == "" {
		return s
	}
	if r != nil {
		for _, v := range r.values() {
			if strings.Contains(s, v) {
				s = strings.ReplaceAll(s, v, Hidden)
			}
		}
	}
	for _, re := range secretContexts {
		s = re.ReplaceAllString(s, "${1}"+Hidden+"${2}")
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, Hidden)
	}
	return s
}

// values are the known secrets, longest first (so a key that contains
// another is hidden whole), read again every half minute. A returned slice
// is never changed afterwards, so callers use it without the lock.
func (r *Redactor) values() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.load == nil || !r.checked.IsZero() && time.Since(r.checked) < 30*time.Second {
		return r.known
	}
	r.checked = time.Now()
	seen := map[string]bool{}
	var known []string
	for _, v := range r.load() {
		v = strings.TrimSpace(v)
		if len(v) >= 6 && !seen[v] {
			seen[v] = true
			known = append(known, v)
		}
	}
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })
	r.known = known
	return known
}

// knownSecrets reads the secret values this home holds: secrets.env, keys in
// the environment, the local API token and a paired remote's token.
func knownSecrets(home string) []string {
	var vals []string
	if saved, err := config.ReadSecrets(); err == nil {
		for _, v := range saved {
			vals = append(vals, v)
		}
	}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if SecretName(k) {
			vals = append(vals, v)
		}
	}
	for _, p := range []string{filepath.Join(home, "data", "api.token"), filepath.Join(home, "remote.yaml")} {
		if b, err := os.ReadFile(p); err == nil {
			if strings.HasSuffix(p, ".yaml") {
				for _, line := range strings.Split(string(b), "\n") {
					if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "token" {
						vals = append(vals, strings.Trim(strings.TrimSpace(v), `"'`))
					}
				}
				continue
			}
			vals = append(vals, strings.TrimSpace(string(b)))
		}
	}
	return vals
}

// SecretName reports whether a setting or variable name holds a secret
// (a key, token or password), as opposed to naming where one is kept
// (…_env, …_file) or counting something (max_tokens).
func SecretName(name string) bool {
	n := strings.ToLower(name)
	for _, suffix := range []string{"_env", "env", "_file", "_path", "_dir"} {
		if strings.HasSuffix(n, suffix) {
			return false
		}
	}
	if strings.Contains(n, "max_tokens") || strings.HasSuffix(n, "tokens") {
		return false
	}
	for _, w := range []string{"password", "passwd", "secret", "token", "apikey", "api_key", "authorization", "cookie", "credential", "private_key", "auth"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return strings.HasSuffix(n, "_key") || n == "key"
}

// redactHandler takes secrets out of every record before it is written.
type redactHandler struct {
	next slog.Handler
	r    *Redactor
}

// Redacting wraps a handler so that nothing it writes carries a secret.
func Redacting(next slog.Handler, r *Redactor) slog.Handler {
	return &redactHandler{next: next, r: r}
}

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.r.String(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactHandler) WithAttrs(as []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(as))
	for i, a := range as {
		clean[i] = h.attr(a)
	}
	return &redactHandler{next: h.next.WithAttrs(clean), r: h.r}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{next: h.next.WithGroup(name), r: h.r}
}

func (h *redactHandler) attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		if SecretName(a.Key) && v.String() != "" {
			return slog.String(a.Key, Hidden)
		}
		return slog.String(a.Key, h.r.String(v.String()))
	case slog.KindGroup:
		as := v.Group()
		clean := make([]slog.Attr, len(as))
		for i, x := range as {
			clean[i] = h.attr(x)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(clean...)}
	case slog.KindAny:
		if v.Any() == nil {
			return a
		}
		s := fmt.Sprint(v.Any())
		if SecretName(a.Key) {
			return slog.String(a.Key, Hidden)
		}
		if c := h.r.String(s); c != s {
			return slog.String(a.Key, c)
		}
		return slog.Attr{Key: a.Key, Value: v}
	}
	return slog.Attr{Key: a.Key, Value: v}
}
