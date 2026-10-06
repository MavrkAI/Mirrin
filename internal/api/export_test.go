package api

import (
	"context"
	"net/http"
)

// ServeHTTPAs serves one request as if it arrived on a listener of this
// exposure. Tests only: in the running server a connection's listener, never
// the caller, says what a request is worth.
func (s *Server) ServeHTTPAs(w http.ResponseWriter, r *http.Request, exp Exposure, hostnames ...string) {
	l := listener{kind: kindLoopback, via: "loopback"}
	if exp == Remote {
		l = listener{kind: kindRemote, via: "remote", hosts: hostnames}
	}
	s.Handler().ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), listenerKey, l)))
}
