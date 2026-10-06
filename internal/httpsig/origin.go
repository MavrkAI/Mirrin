package httpsig

// Where a request is going. @target-uri binds a signature to a scheme and
// authority, so which ones the verifier uses decides which server a signed
// request is good for.

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ErrWrongHost means the request names a host the server does not answer
// to, such as a request signed for staging or a self-hosted control plane
// and replayed here. It wraps ErrSignature.
var ErrWrongHost = fmt.Errorf("%w: request is for another host", ErrSignature)

// origin is the scheme and authority that @target-uri, @scheme and
// @authority are built from. The authority is lowercase and has no default
// port.
type origin struct{ scheme, authority string }

// makeOrigin normalises a scheme and host. Only http and https exist here.
func makeOrigin(scheme, host string) (origin, error) {
	scheme, host = strings.ToLower(scheme), strings.ToLower(host)
	switch scheme {
	case "https":
		host = strings.TrimSuffix(host, ":443")
	case "http":
		host = strings.TrimSuffix(host, ":80")
	default:
		return origin{}, fmt.Errorf("httpsig: scheme %q is not http or https", scheme)
	}
	if host == "" {
		return origin{}, errors.New("httpsig: request has no host")
	}
	return origin{scheme, host}, nil
}

// host is the authority r names: r.Host, which on a server is the Host
// header (or an absolute-form target's host), else r.URL.Host.
func host(r *http.Request) string {
	if r.Host != "" {
		return r.Host
	}
	return r.URL.Host
}

// clientOrigin is where a client's request is going, as the transport will
// send it.
func clientOrigin(r *http.Request) (origin, error) {
	return makeOrigin(r.URL.Scheme, host(r))
}

// listenerOrigin is Verify's view: the authority is r.Host, and the scheme
// is r.URL.Scheme if the server set it (behind a TLS-terminating proxy), else
// https on a TLS listener and http otherwise. An absolute-form target is
// refused, because net/http then takes r.URL.Scheme from the client.
func listenerOrigin(r *http.Request) (origin, error) {
	if r.RequestURI != "" && !strings.HasPrefix(r.RequestURI, "/") {
		return origin{}, fmt.Errorf("%w: request target is not origin-form", ErrSignature)
	}
	scheme := r.URL.Scheme
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	return makeOrigin(scheme, host(r))
}

// matchOrigin is VerifyFor's view: the one origin, of those the server
// answers to, whose authority r.Host names. The scheme is the origin's, so
// neither the listener nor an absolute-form target can change it.
func matchOrigin(r *http.Request, origins []string) (origin, error) {
	var list []origin
	for _, s := range origins {
		o, err := parseOrigin(s)
		if err != nil {
			return origin{}, err
		}
		for _, p := range list {
			if p.authority == o.authority {
				return origin{}, fmt.Errorf("httpsig: origins %q share a host", origins)
			}
		}
		list = append(list, o)
	}
	if len(list) == 0 {
		return origin{}, errors.New("httpsig: VerifyFor needs the server's origins")
	}
	for _, o := range list {
		if h, err := makeOrigin(o.scheme, host(r)); err == nil && h.authority == o.authority {
			return o, nil
		}
	}
	return origin{}, ErrWrongHost
}

// parseOrigin reads a server origin such as "https://cloud.mirrin.app": a
// scheme and a host with an optional port, and nothing else.
func parseOrigin(s string) (origin, error) {
	u, err := url.Parse(s)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || u.Path != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.HasSuffix(s, "#") {
		return origin{}, fmt.Errorf("httpsig: origin %q is not scheme://host[:port]", s)
	}
	return makeOrigin(u.Scheme, u.Host)
}
