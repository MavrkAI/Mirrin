package api

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// mount is a set of routes a feature adds without editing this package's
// route table.
type mount struct {
	name string
	exp  Exposure
	fn   func(*http.ServeMux, Authz)
}

// Mount adds routes: fn registers them on the server's mux, wrapping each
// handler with a (Require, Public or Local). exp is where they answer:
// LoopbackOnly, or Remote for the listeners other devices reach too. Mount
// before the server starts; the routes are fixed on first use.
func (s *Server) Mount(name string, exp Exposure, fn func(*http.ServeMux, Authz)) *Server {
	s.mounts = append(s.mounts, mount{name: name, exp: exp, fn: fn})
	return s
}

// Serve answers on ln until ctx ends. exp is what the listener is:
// LoopbackOnly for this computer (the master key works, connections from
// elsewhere are refused), Remote for one other devices reach (device keys
// only; Host must be one of hostnames). via names the route in logs and
// device records: tailscale, files, relay r1…
func (s *Server) Serve(ctx context.Context, ln net.Listener, exp Exposure, via string, hostnames ...string) error {
	return s.ServeListener(ctx, ln, ListenerConfig{Exposure: exp, Via: via, Hostnames: hostnames})
}

// ListenerConfig describes a listener for ServeListener.
type ListenerConfig struct {
	Exposure  Exposure
	Via       string
	Hostnames []string // the names a Remote listener answers to
	// URL is how other devices reach a Remote listener (https://name[:port]);
	// pairing codes and links carry it, with Pins.
	URL string
	// Pins are the SPKI pins (SPKIPin) of the listener's current and next
	// certificate, for terminals to pin.
	Pins []string
}

// ServeListener is Serve with everything a remote listener can say about
// itself.
func (s *Server) ServeListener(ctx context.Context, ln net.Listener, c ListenerConfig) error {
	l := listener{kind: kindLoopback, via: c.Via}
	if c.Exposure == Remote {
		l.kind = kindRemote
		for _, h := range c.Hostnames {
			if h = strings.TrimSpace(h); h != "" {
				l.hosts = append(l.hosts, h)
			}
		}
		if c.URL != "" {
			b := Base{URL: strings.TrimRight(c.URL, "/"), Pins: append([]string(nil), c.Pins...)}
			s.lmu.Lock()
			s.reached = append(s.reached, b)
			s.lmu.Unlock()
			defer func() {
				s.lmu.Lock()
				for i, r := range s.reached {
					if r.URL == b.URL {
						s.reached = append(s.reached[:i], s.reached[i+1:]...)
						break
					}
				}
				s.lmu.Unlock()
			}()
		}
	}
	if l.via == "" {
		l.via = "loopback"
		if l.kind == kindRemote {
			l.via = "remote"
		}
	}
	return s.serve(ctx, ln, l, false)
}
