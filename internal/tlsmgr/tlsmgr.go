// Package tlsmgr owns certificates for listeners on this machine.
package tlsmgr

import (
	"context"
	"crypto/tls"
)

type Source interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
	Hostnames() []string
	SPKIs() []string
	Run(context.Context) error
}

// Challenger is a Source that answers ACME TLS-ALPN-01 on its listener
// (ACME does). ChallengeConfig returns nil for an ordinary hello.
type Challenger interface {
	ChallengeConfig(*tls.ClientHelloInfo) (*tls.Config, error)
}

func TLSConfig(src Source, extraALPN ...string) *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: src.GetCertificate, NextProtos: append([]string{"h2", "http/1.1"}, extraALPN...)}
	if ch, ok := src.(Challenger); ok {
		c.GetConfigForClient = ch.ChallengeConfig
	}
	return c
}
