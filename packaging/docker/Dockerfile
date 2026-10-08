# Hosted twin image: headless Mirrin, API on 7742, state on a volume.
# The binaries are cross-compiled on the host so the image build needs no Go
# toolchain or module downloads:
#   make dist-portable      # dist/mirrin-linux-amd64 and dist/mirrin-linux-arm64
#   docker build --platform linux/arm64 -f packaging/docker/Dockerfile -t mirrin-twin .
# The gateway's own device key lands in /data/gateway/token once the twin is
# up (entrypoint.sh); the healthcheck asks the public /healthz and needs no key.
FROM alpine:3.20
ARG TARGETARCH
RUN apk add --no-cache ca-certificates tzdata wget && adduser -D -u 10001 twin
COPY dist/mirrin-linux-${TARGETARCH} /usr/local/bin/mirrin
COPY packaging/docker/entrypoint.sh packaging/docker/healthcheck.sh /usr/local/bin/
ENV MIRRIN_HOME=/data
RUN mkdir -p /data && chown twin:twin /data
VOLUME ["/data"]
USER twin
EXPOSE 7742
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s CMD ["/usr/local/bin/healthcheck.sh"]
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
