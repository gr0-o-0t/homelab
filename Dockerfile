# The homelab agent image: one container per PC (`homelab init` / `homelab
# join` create it, `homelab upgrade` replaces it).
#
# The base is the official Docker CLI image (Alpine + a static docker CLI),
# not distroless: the agent shells out to `docker` (DB dumps via docker exec,
# docker service logs, docker logs) and its backup helpers run this same
# image with busybox sh, tar and rm. Pinned by tag and digest.
#
# Nothing is compiled here: the context holds the static headless binaries
# (CGO_ENABLED=0, -tags nogui) that `make release` / `make image` build, at
# dist/image/linux_<arch>/homelab, so the image runs exactly the released
# binary. Runs as root: the agent drives Docker through its socket.
FROM docker:29.9.3-cli@sha256:2357639ad1ea7d0194134bf3292622fd4e562483966ed674e5d7c945b5f3b28d

ARG TARGETARCH
ARG VERSION=dev

# ca-certificates is in the base already; zone data is not.
RUN apk add --no-cache tzdata

COPY dist/image/linux_${TARGETARCH}/homelab /usr/local/bin/homelab

# `homelab upgrade` reads the version label to check the agents it replaced.
LABEL org.opencontainers.image.title="homelab" \
      org.opencontainers.image.description="homelab agent: Swarm, edge (Caddy on tsnet) and remote CLI" \
      org.opencontainers.image.source="https://github.com/gr0-o-0t/homelab" \
      org.opencontainers.image.version="${VERSION}"

ENTRYPOINT ["/usr/local/bin/homelab"]
CMD ["agent"]
