package tlsmgr

import (
	"context"
	"github.com/MavrkAI/Mirrin/internal/tailscale"
	"os"
	"path/filepath"
	"time"
)

type tailscaleSource struct {
	*files
	cli       *tailscale.CLI
	host, dir string
}

func Tailscale(ctx context.Context, cli *tailscale.CLI) (Source, error) {
	s, e := cli.Status(ctx)
	if e != nil {
		return nil, e
	}
	dir, e := os.MkdirTemp("", "mirrin-tls-")
	if e != nil {
		return nil, e
	}
	t := &tailscaleSource{files: &files{cert: filepath.Join(dir, "cert.pem"), key: filepath.Join(dir, "key.pem"), managed: true}, cli: cli, host: s.Self.DNSName, dir: dir}
	if e = t.issue(ctx); e != nil {
		os.RemoveAll(dir)
		return nil, e
	}
	return t, nil
}
func (t *tailscaleSource) issue(ctx context.Context) error {
	_, e := t.cli.Run(ctx, "cert", "--cert-file", t.cert, "--key-file", t.key, t.host)
	if e != nil {
		return e
	}
	if e = os.Chmod(t.key, 0600); e != nil {
		return e
	}
	return t.reload()
}
func (t *tailscaleSource) Run(ctx context.Context) error {
	defer os.RemoveAll(t.dir)
	return t.maintain(ctx, time.Hour, func(ctx context.Context) error {
		p, err := t.GetCertificate(nil)
		if err != nil {
			return err
		}
		if p.Leaf.NotAfter.Sub(t.time()) < 30*24*time.Hour {
			return t.issue(ctx)
		}
		return nil
	})
}
