package browser

import (
	"bufio"
	"context"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Chrome used to follow this computer's proxy settings itself. It now sends
// everything through the guard, so the guard follows them instead, after its
// check: macOS System Settings, Windows Internet Options, and GNOME or KDE on
// Linux. What it can't follow yet (an automatic configuration script, proxy
// discovery, SOCKS4) is reported by the Browser health check.

// sysProxy is the proxy this computer's settings name.
type sysProxy struct {
	http, https  string   // host:port of the proxy for http:// and https:// pages
	socks        string   // host:port of a SOCKS5 proxy for what those two don't cover
	exceptions   []string // hosts that go direct: "*.local", "intranet.example.com", "10.0.0.0/8"
	simpleDirect bool     // names without a dot go direct
	// unsupported names a setting the guard can't follow yet; the browser
	// connects directly where it would have applied.
	unsupported string
}

// The settings the guard can't follow, as the health check names them.
const (
	pacSetting    = "an automatic configuration script (PAC)"
	wpadSetting   = "automatic proxy discovery (WPAD)"
	socks4Setting = "a SOCKS4 proxy setting"
)

// readSystemProxy reads this computer's proxy settings: nil for none. A
// variable so tests can stand in for it.
var readSystemProxy = func() *sysProxy {
	switch runtime.GOOS {
	case "darwin":
		return parseScutil(scutilProxy())
	case "windows":
		return windowsProxy()
	default:
		return desktopProxy()
	}
}

// systemProxy is the proxy this computer's settings name for a target (nil
// when there is none), and the setting it can't follow, if any.
func systemProxy() (pick func(*url.URL) *url.URL, unsupported string) {
	p := readSystemProxy()
	if p == nil {
		return nil, ""
	}
	if p.http == "" && p.https == "" && p.socks == "" {
		return nil, p.unsupported
	}
	return p.forURL, p.unsupported
}

// forURL picks the proxy for u, or nil for a direct connection. As in
// Chrome, an https page uses the secure web proxy and a plain one the web
// proxy, never each other's; SOCKS takes whatever neither covers.
func (p *sysProxy) forURL(u *url.URL) *url.URL {
	if p.bypass(strings.ToLower(u.Hostname())) {
		return nil
	}
	hp := p.http
	if u.Scheme == "https" {
		hp = p.https
	}
	if hp != "" {
		return &url.URL{Scheme: "http", Host: hp}
	}
	if p.socks != "" {
		return &url.URL{Scheme: "socks5", Host: p.socks}
	}
	return nil
}

// bypass reports whether host goes direct.
func (p *sysProxy) bypass(host string) bool {
	if p.simpleDirect && !strings.Contains(host, ".") && !strings.Contains(host, ":") {
		return true
	}
	for _, e := range p.exceptions {
		if matchHost(e, host) {
			return true
		}
	}
	return false
}

// matchHost matches a proxy exception: "*.example.com" (the domain too),
// ".example.com", "10.*", "10.0.0.0/8" or an exact name.
func matchHost(pat, host string) bool {
	pat = strings.ToLower(strings.TrimSpace(pat))
	switch {
	case pat == "":
		return false
	case strings.Contains(pat, "/"):
		pfx, err := netip.ParsePrefix(pat)
		ip, ierr := netip.ParseAddr(host)
		return err == nil && ierr == nil && pfx.Contains(ip.Unmap())
	case strings.HasPrefix(pat, "*.") && host == pat[2:]:
		return true
	case strings.HasPrefix(pat, "."):
		return host == pat[1:] || strings.HasSuffix(host, pat)
	case strings.Contains(pat, "*"):
		ok, _ := path.Match(pat, host)
		return ok
	}
	return host == pat
}

// hostPort is "host:port" from a setting that may carry a scheme or leave
// the port out.
func hostPort(v, defPort string) string {
	v = strings.TrimSpace(v)
	if _, rest, ok := strings.Cut(v, "://"); ok {
		v = rest
	}
	v = strings.TrimSuffix(v, "/")
	if v == "" {
		return ""
	}
	if h, p, err := net.SplitHostPort(v); err == nil {
		if p == "" || p == "0" {
			p = defPort
		}
		return net.JoinHostPort(h, p)
	}
	return net.JoinHostPort(strings.Trim(v, "[]"), defPort)
}

// scutilProxy reads the Mac's proxy settings (System Settings > Network >
// Proxies). A variable so tests can stand in for it.
var scutilProxy = func() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	return command("scutil", "--proxy")
}

// command runs a settings reader briefly, "" if it can't.
func command(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// parseScutil reads `scutil --proxy` output:
//
//	<dictionary> {
//	  ExceptionsList : <array> {
//	    0 : *.local
//	  }
//	  HTTPSEnable : 1
//	  HTTPSPort : 8080
//	  HTTPSProxy : proxy.example.com
//	}
func parseScutil(out string) *sysProxy {
	if strings.TrimSpace(out) == "" {
		return nil
	}
	kv := map[string]string{}
	p := &sysProxy{}
	inExceptions := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "}" {
			inExceptions = false
			continue
		}
		k, v, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if inExceptions {
			p.exceptions = append(p.exceptions, strings.ToLower(v))
			continue
		}
		if k == "ExceptionsList" && strings.HasPrefix(v, "<array>") {
			inExceptions = true
			continue
		}
		kv[k] = v
	}
	proxy := func(enable, host, port, defPort string) string {
		if kv[enable] != "1" || kv[host] == "" {
			return ""
		}
		return hostPort(net.JoinHostPort(kv[host], kv[port]), defPort)
	}
	p.http = proxy("HTTPEnable", "HTTPProxy", "HTTPPort", "80")
	p.https = proxy("HTTPSEnable", "HTTPSProxy", "HTTPSPort", "80")
	p.socks = proxy("SOCKSEnable", "SOCKSProxy", "SOCKSPort", "1080")
	p.simpleDirect = kv["ExcludeSimpleHostnames"] == "1"
	switch {
	case kv["ProxyAutoConfigEnable"] == "1":
		p.unsupported = pacSetting
	case kv["ProxyAutoDiscoveryEnable"] == "1":
		p.unsupported = wpadSetting
	}
	return p
}

// parseWindowsProxy reads Internet Options' proxy, from the registry values
// ProxyEnable, ProxyServer ("proxy:8080", or per scheme
// "http=proxy:80;https=proxy:443;socks=proxy:1080"), ProxyOverride
// ("<local>;*.corp.example") and AutoConfigURL.
func parseWindowsProxy(enable bool, server, override, autoConfig string) *sysProxy {
	p := &sysProxy{}
	if strings.TrimSpace(autoConfig) != "" {
		p.unsupported = pacSetting
	}
	if enable && strings.TrimSpace(server) != "" {
		if !strings.Contains(server, "=") {
			p.http = hostPort(server, "80")
			p.https = p.http
		}
		for _, part := range strings.Split(server, ";") {
			k, v, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "http":
				p.http = hostPort(v, "80")
			case "https":
				p.https = hostPort(v, "80")
			case "socks":
				// Windows means SOCKS4 here unless it says socks5://.
				if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "socks5://") {
					p.socks = hostPort(v, "1080")
				} else if p.unsupported == "" {
					p.unsupported = socks4Setting
				}
			}
		}
		for _, e := range strings.Split(override, ";") {
			switch e = strings.ToLower(strings.TrimSpace(e)); e {
			case "":
			case "<local>":
				p.simpleDirect = true
			default:
				p.exceptions = append(p.exceptions, e)
			}
		}
	}
	if p.http == "" && p.https == "" && p.socks == "" && p.unsupported == "" {
		return nil
	}
	return p
}

// desktopProxy is the Linux desktop's proxy, where Chrome would have read
// it: GNOME's settings (and those of desktops built on them) or KDE's. With
// no desktop (a server, a service) Chrome goes by the environment, as the
// guard does.
func desktopProxy() *sysProxy {
	desk := strings.ToLower(os.Getenv("XDG_CURRENT_DESKTOP") + ":" + os.Getenv("DESKTOP_SESSION"))
	switch {
	case strings.Contains(desk, "kde"):
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil
			}
			dir = filepath.Join(home, ".config")
		}
		b, _ := os.ReadFile(filepath.Join(dir, "kioslaverc"))
		return parseKioslaverc(string(b))
	case strings.Contains(desk, "gnome"), strings.Contains(desk, "unity"), strings.Contains(desk, "cinnamon"),
		strings.Contains(desk, "pantheon"), strings.Contains(desk, "budgie"):
		return parseGsettings(gsettingsProxy())
	}
	return nil
}

// gsettingsProxy lists GNOME's proxy settings. A variable for tests.
var gsettingsProxy = func() string { return command("gsettings", "list-recursively", "org.gnome.system.proxy") }

// parseGsettings reads `gsettings list-recursively org.gnome.system.proxy`:
//
//	org.gnome.system.proxy mode 'manual'
//	org.gnome.system.proxy ignore-hosts ['localhost', '127.0.0.0/8']
//	org.gnome.system.proxy.http host 'proxy.example.com'
//	org.gnome.system.proxy.http port 3128
func parseGsettings(out string) *sysProxy {
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), " ", 3)
		if len(f) == 3 {
			kv[strings.TrimPrefix(f[0], "org.gnome.system.proxy")+" "+f[1]] = strings.Trim(f[2], "'")
		}
	}
	switch kv[" mode"] {
	case "auto":
		if kv[" autoconfig-url"] != "" {
			return &sysProxy{unsupported: pacSetting}
		}
		return &sysProxy{unsupported: wpadSetting}
	case "manual":
	default:
		return nil
	}
	proxy := func(scheme, defPort string) string {
		host, port := kv["."+scheme+" host"], kv["."+scheme+" port"]
		if host == "" {
			return ""
		}
		return hostPort(net.JoinHostPort(host, port), defPort)
	}
	p := &sysProxy{http: proxy("http", "80"), https: proxy("https", "80"), socks: proxy("socks", "1080")}
	if kv[" use-same-proxy"] == "true" && p.http != "" {
		p.https = p.http
	}
	list := strings.Trim(kv[" ignore-hosts"], "[]@as ")
	for _, e := range strings.Split(list, ",") {
		if e = strings.Trim(strings.TrimSpace(e), "'"); e != "" {
			p.exceptions = append(p.exceptions, e)
		}
	}
	if p.http == "" && p.https == "" && p.socks == "" {
		return nil
	}
	return p
}

// parseKioslaverc reads KDE's proxy settings (~/.config/kioslaverc):
//
//	[Proxy Settings]
//	ProxyType=1
//	httpProxy=http://proxy.example.com 3128
//	NoProxyFor=localhost,127.0.0.1
//
// ProxyType is 0 for none, 1 set by hand, 2 a configuration script, 3
// discovery and 4 the environment.
func parseKioslaverc(out string) *sysProxy {
	kv := map[string]string{}
	in := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			in = line == "[Proxy Settings]"
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && in {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	switch kv["ProxyType"] {
	case "2":
		return &sysProxy{unsupported: pacSetting}
	case "3":
		return &sysProxy{unsupported: wpadSetting}
	case "1":
	default:
		return nil
	}
	proxy := func(key, defPort string) string {
		v := kv[key]
		if host, port, ok := strings.Cut(strings.TrimSpace(v), " "); ok { // "http://proxy 3128"
			if _, err := strconv.Atoi(port); err == nil {
				v = strings.TrimSuffix(host, "/") + ":" + port
			}
		}
		return hostPort(v, defPort)
	}
	p := &sysProxy{http: proxy("httpProxy", "80"), https: proxy("httpsProxy", "80"), socks: proxy("socksProxy", "1080")}
	for _, e := range strings.Split(kv["NoProxyFor"], ",") {
		if e = strings.TrimSpace(e); e != "" {
			p.exceptions = append(p.exceptions, e)
		}
	}
	if p.http == "" && p.https == "" && p.socks == "" {
		return nil
	}
	return p
}
