# mirrin-relay image: the relay alone, running as an unprivileged user.
# The binary is cross-compiled on the host (make relay-release), so the image
# build needs no Go toolchain or module downloads:
#   make relay-release
#   docker build --platform linux/amd64 -f packaging/relay/Dockerfile -t mirrin-relay .
# Run it with relay.yaml mounted read-only and a volume for the ACME cache
# and the last good deny list (docs/relay-selfhost.md). Use the host's
# network: behind Docker's port publishing every client would appear to
# come from the bridge gateway, and per-address limits would lump them
# together.
#   docker run -d --name mirrin-relay --restart unless-stopped --network host --stop-timeout 40 \
#     -v /etc/mirrin-relay:/etc/mirrin-relay:ro -v mirrin-relay:/var/lib/mirrin-relay mirrin-relay
# The base is pinned by digest (the multi-arch index of alpine:3.24, which
# is supported until 2028); bump both together.
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
ARG TARGETARCH
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10002 relay \
    && mkdir -p /etc/mirrin-relay /var/lib/mirrin-relay \
    && chown relay:relay /var/lib/mirrin-relay
COPY dist/mirrin-relay-linux-${TARGETARCH} /usr/local/bin/mirrin-relay
# Bind :443 and :80 without root, also under --network host.
RUN apk add --no-cache --virtual .caps libcap \
    && setcap cap_net_bind_service=+ep /usr/local/bin/mirrin-relay \
    && apk del .caps
USER relay
EXPOSE 443 80
VOLUME ["/var/lib/mirrin-relay"]
# /healthz on the metrics listener (metrics_listen, 127.0.0.1:9100 by default).
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:9100/healthz >/dev/null || exit 1
# SIGTERM drains: daemons are told to use another relay, and spliced
# connections get up to 30 s. Docker waits only 10 s by default, so run
# with --stop-timeout 40 (above).
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/mirrin-relay"]
CMD ["serve", "--config", "/etc/mirrin-relay/relay.yaml"]
