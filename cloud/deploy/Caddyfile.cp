# Caddy on the control-plane VM: TLS for the control plane's public name,
# and the watcher's /healthz for the external dead-man check. Installed as
# /etc/caddy/Caddyfile. There is deliberately no `log` directive: Caddy
# then keeps no access log, so no client address is written anywhere
# (cloud/README.md, "No IP addresses").
{
	email @@OPS_EMAIL@@
}

@@CLOUD_HOST@@ {
	encode zstd gzip
	# mirrin-cloud reads the client address from X-Forwarded-For, which
	# Caddy sets, and only for requests from 127.0.0.1 (trusted_proxies).
	reverse_proxy 127.0.0.1:8787 {
		header_up -X-Real-IP
	}
	header {
		Strict-Transport-Security "max-age=63072000"
		X-Content-Type-Options nosniff
		-Server
	}
}

@@WATCH_HOST@@ {
	handle /healthz {
		reverse_proxy 127.0.0.1:9103
	}
	handle {
		respond 404
	}
}
