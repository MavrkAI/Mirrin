package identity

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// A setting is secret when its name says so (api_key, token, password,
// secret…) and it holds text, or when it is an environment value handed to an
// MCP server. Secrets inside other text are cut out too: the password in a
// URL, the value in NAME=value when NAME looks secret (a flag, an environment
// assignment in a command, a URL query), the value after a secret flag
// (--token …) and bearer headers. Secrets never leave the machine; the
// destination keeps its own.

var secretWords = []string{"api_key", "apikey", "token", "password", "passwd", "secret", "credential", "private_key"}

// notSecret are names that contain a secret word but hold no secret.
var notSecret = map[string]bool{"max_tokens": true, "tokenizer": true}

// isSecretKey reports whether a config key names a secret. Keys that say where
// a secret lives (…_env, …_file) are not secrets themselves.
func isSecretKey(k string) bool {
	k = strings.ToLower(k)
	if notSecret[k] {
		return false
	}
	for _, suf := range []string{"_env", "_file", "_dir", "_path"} {
		if strings.HasSuffix(k, suf) {
			return false
		}
	}
	for _, w := range secretWords {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// argName turns a flag or variable name (--api-key, GITHUB_TOKEN) into the
// form isSecretKey reads: lower case, words joined by _.
func argName(s string) string {
	return strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(strings.TrimLeft(s, "-")))
}

// negated reports whether a name switches something off rather than naming it
// (no_password, use_token).
func negated(n string) bool {
	for _, p := range []string{"no_", "use_", "without_", "skip_", "disable_", "ask_"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// isSecretName reports whether NAME in NAME=value (or a header name) says the
// value is secret.
func isSecretName(s string) bool {
	n := argName(s)
	return !negated(n) && isSecretKey(n)
}

// isSecretFlag reports whether a command-line flag takes a secret as its next
// argument. Only a flag that ends in a secret word counts: --token and
// --api-key do, --tokenizer and --no-password (whose next argument is a
// path or a positional) do not.
func isSecretFlag(a string) bool {
	if !strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
		return false
	}
	n := argName(a)
	if n == "" || negated(n) || notSecret[n] {
		return false
	}
	if n == "key" {
		return true
	}
	for _, w := range secretWords {
		if strings.HasSuffix(n, w) {
			return true
		}
	}
	return strings.HasSuffix(n, "_key") && isSecretKey(n) // --secret-access-key
}

// isSecretPath reports whether the string at path is a secret.
func isSecretPath(path []string) bool {
	if len(path) == 0 || isMCPToolRisk(path) {
		return false
	}
	return isMCPEnv(path) || isSecretKey(path[len(path)-1])
}

// isMCPEnv reports whether path is a value in an MCP server's env.
func isMCPEnv(path []string) bool {
	return len(path) == 5 && path[0] == "mcp" && path[1] == "servers" && path[3] == "env"
}

// isMCPToolRisk reports whether path is a risk override for one MCP tool. The
// tool names are the user's, so get_password is a tool, not a password, and
// its risk (read, write, dangerous) must travel.
func isMCPToolRisk(path []string) bool {
	return len(path) == 5 && path[0] == "mcp" && path[1] == "servers" && path[3] == "tool_risk"
}

func isMCPArgs(path []string) bool {
	return len(path) == 4 && path[0] == "mcp" && path[1] == "servers" && path[3] == "args"
}

func isMCPServers(path []string) bool {
	return len(path) == 2 && path[0] == "mcp" && path[1] == "servers"
}

// envRef is an MCP env value that names a variable instead of holding one
// ("$GITHUB_TOKEN", "${GITHUB_TOKEN}"): procenv.Expand hands the server this
// machine's own value, so the reference holds no secret and travels as is.
var envRef = regexp.MustCompile(`^\$\{?[A-Za-z_][A-Za-z0-9_]*\}?$`)

// scrubLeaf is what an exported string at path looks like: blank for a
// secret, otherwise the text with any secrets inside it cut out.
func scrubLeaf(path []string, s string) string {
	if isMCPEnv(path) && envRef.MatchString(strings.TrimSpace(s)) {
		return s
	}
	if isSecretPath(path) {
		return ""
	}
	return scrubText(s)
}

// scrubText cuts the secrets out of free text: URL passwords and the values
// of secret-looking NAME=value pairs.
func scrubText(s string) string {
	return scrubAssignments(stripURLCredentials(s))
}

// userinfo finds the user part of a URL anywhere in text: scheme://user[:password]@.
// The greedy match runs to the last @ before the host, so a password with @ in it
// is caught whole.
var userinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*)://([^\s/?#"'<>]*)@`)

// stripURLCredentials removes the password from every URL in s, and the whole
// user part of an http(s) URL, where a token often stands in for the user name.
func stripURLCredentials(s string) string {
	if !strings.Contains(s, "://") || !strings.Contains(s, "@") {
		return s
	}
	return replaceSubmatches(userinfo, s, func(m []string) string {
		scheme, info := m[1], m[2]
		user, _, hasPassword := strings.Cut(info, ":")
		switch strings.TrimPrefix(strings.ToLower(scheme), "git+") {
		case "http", "https", "ws", "wss":
			return scheme + "://"
		}
		switch {
		case user == "":
			return scheme + "://"
		case hasPassword:
			return scheme + "://" + user + "@"
		}
		return m[0]
	})
}

// assignment finds NAME=value: at the start of text, after a space or shell
// punctuation (sh -c "API_KEY=… exec"), or as a URL query parameter.
var assignment = regexp.MustCompile(`(^|[\s;&|("'?])(-{0,2}[A-Za-z_][A-Za-z0-9_.-]*)=("[^"]*"|'[^']*'|[^\s;&|)"'#]*)`)

// scrubAssignments blanks the value of each NAME=value whose name looks secret.
func scrubAssignments(s string) string {
	if !strings.Contains(s, "=") {
		return s
	}
	return replaceSubmatches(assignment, s, func(m []string) string {
		if m[3] == "" || !isSecretName(m[2]) {
			return m[0]
		}
		return m[1] + m[2] + "="
	})
}

// header matches an HTTP header passed as one argument: "X-API-Key: …".
var header = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*):\s*\S`)

// scrubArg cuts the secrets out of one command-line argument.
func scrubArg(a string) string {
	if l := strings.ToLower(a); strings.Contains(l, "bearer ") || strings.Contains(l, "authorization:") {
		return ""
	}
	if m := header.FindStringSubmatch(a); m != nil && isSecretName(m[1]) {
		return m[1] + ": "
	}
	return scrubText(a)
}

// scrubArgs cuts the secrets out of an MCP server's command line in place:
// URL passwords (postgresql://user:…@host), secret NAME=value pairs (--api-key=…,
// -e GITHUB_TOKEN=…, sh -c "API_KEY=… exec"), the argument after a secret flag
// (--token …) and bearer headers. It returns the indexes it changed.
func scrubArgs(args []any) []int {
	var hit []int
	secretNext := false
	for i, x := range args {
		a, ok := x.(string)
		if !ok {
			secretNext = false
			continue
		}
		c := scrubArg(a)
		if secretNext && !strings.HasPrefix(a, "-") {
			c = ""
		}
		secretNext = isSecretFlag(a)
		if c != a {
			args[i], hit = c, append(hit, i)
		}
	}
	return hit
}

// replaceSubmatches is ReplaceAllStringFunc with the submatches in hand.
func replaceSubmatches(re *regexp.Regexp, s string, fn func(m []string) string) string {
	var b strings.Builder
	last := 0
	for _, idx := range re.FindAllStringSubmatchIndex(s, -1) {
		m := make([]string, len(idx)/2)
		for i := range m {
			if idx[2*i] >= 0 {
				m[i] = s[idx[2*i]:idx[2*i+1]]
			}
		}
		b.WriteString(s[last:idx[0]])
		b.WriteString(fn(m))
		last = idx[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// scrubSecrets blanks every secret in a parsed config and returns the names of
// the ones that had a value, so the importer can say what to set up again.
func scrubSecrets(cfg map[string]any) []string {
	var found []string
	var walk func(v any, path []string)
	walk = func(v any, path []string) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				p := extend(path, k)
				if s, ok := val.(string); ok {
					if c := scrubLeaf(p, s); c != s {
						t[k] = c
						found = append(found, label(p))
					}
					continue
				}
				walk(val, p)
			}
		case []any:
			if isMCPArgs(path) {
				for _, i := range scrubArgs(t) {
					found = append(found, label(extend(path, indexLabel(i))))
				}
				return
			}
			for i, x := range t {
				p := extend(path, itemLabel(x, i))
				if s, ok := x.(string); ok {
					if c := scrubLeaf(p, s); c != s {
						t[i] = c
						found = append(found, label(p))
					}
					continue
				}
				walk(x, p)
			}
		}
	}
	walk(cfg, nil)
	sort.Strings(found)
	return found
}

// finishSecrets runs after a merge. For each secret the export left out
// (declared) that this machine has no value for, it reports it: MCP env names
// by server, the rest as secrets still to set. A secret whose …_env variable
// is set here (in the environment or secrets.env) is not missing. An MCP
// server starts with only its own env block (procenv), so a left-out env
// value this machine has becomes "$NAME", which hands the server this
// machine's value; one it hasn't is dropped rather than passed on blank.
func finishSecrets(cfg map[string]any, declared []string) (missing []string, env map[string][]string) {
	want := map[string]bool{}
	for _, d := range declared {
		want[d] = true
	}
	env = map[string][]string{}
	var walk func(v any, path []string)
	walk = func(v any, path []string) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				p := extend(path, k)
				s, ok := val.(string)
				if !ok {
					walk(val, p)
					continue
				}
				if !want[label(p)] || scrubLeaf(p, s) != s {
					continue // not left out, or it has its value here
				}
				if isMCPEnv(p) {
					if s != "" {
						continue // this machine's own value, or a "$NAME" it passes on
					}
					if config.Secret(k) != "" {
						t[k] = "$" + k
						continue
					}
					delete(t, k)
					env[p[2]] = append(env[p[2]], k)
					continue
				}
				if isSecretKey(k) {
					name := strings.ToUpper(k) // elevenlabs_api_key → ELEVENLABS_API_KEY
					if named, _ := t[k+"_env"].(string); named != "" {
						name = named
					}
					if config.Secret(name) != "" {
						continue
					}
					// A model key is also found under its provider's own
					// variable (config.ProviderKey), often kept in secrets.env.
					if prov := keyProvider(p, t); prov != "" && config.Secret(config.DefaultKeyEnv(prov)) != "" {
						continue
					}
				}
				missing = append(missing, label(p))
			}
		case []any:
			// An argument holds its secret here when scrubbing changes it.
			// The list is scrubbed whole: a flag's value depends on the flag.
			var scrubbed []any
			if isMCPArgs(path) {
				scrubbed = append([]any(nil), t...)
				scrubArgs(scrubbed)
			}
			for i, x := range t {
				p := extend(path, itemLabel(x, i))
				s, ok := x.(string)
				if !ok {
					walk(x, p)
					continue
				}
				clean := scrubLeaf(p, s)
				if scrubbed != nil {
					clean, _ = scrubbed[i].(string)
				}
				if want[label(p)] && clean == s {
					missing = append(missing, label(p))
				}
			}
		}
	}
	walk(cfg, nil)
	sort.Strings(missing)
	for _, names := range env {
		sort.Strings(names)
	}
	return missing, env
}

// keyProvider names the model provider a key at path is for:
// llm.providers.<p>.api_key, or llm.api_key for the provider next to it.
func keyProvider(path []string, parent map[string]any) string {
	switch {
	case len(path) == 4 && path[0] == "llm" && path[1] == "providers" && path[3] == "api_key":
		return path[2]
	case len(path) == 2 && path[0] == "llm" && path[1] == "api_key":
		prov, _ := parent["provider"].(string)
		return prov
	}
	return ""
}

// MCPEnvLabel splits a left-out setting's name into the MCP server and the
// environment variable, when it is one (mcp.servers.fs.env.LOG_LEVEL).
func MCPEnvLabel(l string) (server, name string, ok bool) {
	rest, ok := strings.CutPrefix(l, "mcp.servers.")
	if !ok {
		return "", "", false
	}
	i := strings.LastIndex(rest, ".env.")
	if i <= 0 {
		return "", "", false
	}
	return rest[:i], rest[i+len(".env."):], true
}

// extend returns path+elem without sharing path's backing array.
func extend(path []string, elem string) []string {
	return append(path[:len(path):len(path)], elem)
}

// itemLabel names a list item: by its name for named things (MCP servers),
// otherwise by index.
func itemLabel(x any, i int) string {
	if m, ok := x.(map[string]any); ok {
		if n, ok := m["name"].(string); ok && n != "" {
			return n
		}
	}
	return indexLabel(i)
}

func indexLabel(i int) string { return "[" + strconv.Itoa(i) + "]" }

// label renders a path the way the config file reads: llm.providers.openai.api_key.
func label(path []string) string {
	var b strings.Builder
	for i, e := range path {
		if i > 0 && !strings.HasPrefix(e, "[") {
			b.WriteByte('.')
		}
		b.WriteString(e)
	}
	return b.String()
}
