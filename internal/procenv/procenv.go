// Package procenv builds the environment for programs the twin starts on the
// owner's behalf (custom tools, MCP servers). They get what a program needs
// to run, plus the variables the owner named for them, and never the rest of
// the daemon's environment: that is where API keys and channel tokens live.
package procenv

import (
	"os"
	"regexp"
	"strings"

	"github.com/MavrkAI/Mirrin/internal/config"
)

// base lists the variables every child gets when set: where things are, who
// is running, locale, and how to reach the network. None of them is a secret
// in the usual setup.
var base = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP", "TERM", "TZ",
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "LC_NUMERIC", "LC_TIME", "LC_COLLATE", "LC_MONETARY",
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	// Where language runtimes and their packages live, so tools written
	// for them still start.
	"JAVA_HOME", "PYTHONPATH", "VIRTUAL_ENV", "CONDA_PREFIX", "NVM_DIR", "NODE_PATH",
	"GOPATH", "GOROOT", "CARGO_HOME", "RUSTUP_HOME",
	// The desktop and local services a tool may drive.
	"DISPLAY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS", "XDG_DATA_DIRS", "XDG_CONFIG_DIRS", "DOCKER_HOST",
	// Windows needs these to start most programs at all.
	"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
	"PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "HOMEDRIVE", "HOMEPATH",
}

// Base returns the minimal environment, taken from the current process.
func Base() []string {
	return pick(os.Environ(), base)
}

// With returns the minimal environment plus the named variables: from the
// current process, or else saved with mirrin (secrets.env, which the daemon
// no longer exports; see config.Secret). Names set nowhere are skipped.
func With(names ...string) []string {
	env := os.Environ()
	out := append(pick(env, base), pick(env, names)...)
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || len(pick(env, []string{n})) > 0 {
			continue
		}
		if v := config.Secret(n); v != "" {
			out = append(out, n+"="+v)
		}
	}
	return out
}

// pick keeps the entries of env whose name is in names. Names compare
// case-insensitively, as Windows does; on other systems that only ever adds
// a harmless lower-case twin such as http_proxy.
func pick(env, names []string) []string {
	var out []string
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		for _, n := range names {
			if strings.EqualFold(k, strings.TrimSpace(n)) {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

var reRef = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?$`)

// Expand resolves a configured value: "$NAME" or "${NAME}" means "the
// daemon's own NAME" (its environment, then secrets.env), so a token can be
// passed along without writing it into config.yaml. Anything else is used as
// written.
func Expand(v string) string {
	if m := reRef.FindStringSubmatch(strings.TrimSpace(v)); m != nil {
		return config.Secret(m[1])
	}
	return v
}
