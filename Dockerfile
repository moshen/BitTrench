# The image packages the release build; it does not produce one.
#
# `mise run release` has already cross-compiled dist/linux-<arch>/bittrench with
# the pinned toolchain, and the release workflow unpacks the published archive
# into that tree before building here - so the bytes in the image for a version
# are the bytes in its archive, rather than a second build that merely ought to
# match. That is also why there is no builder stage and no RUN at all: with
# nothing but COPY, buildx assembles both architectures on one runner with no
# emulation.
#
# distroless static, not scratch: HTTP(S) tracker fetches need a CA bundle, and
# it also brings /etc/passwd and tzdata. There is no shell in here - use
# `docker cp` or a second container to poke at a volume.
FROM gcr.io/distroless/static-debian12:latest

ARG TARGETARCH
ARG VERSION=dev

LABEL org.opencontainers.image.title="BitTrench" \
      org.opencontainers.image.description="A user-space WireGuard BitTorrent daemon" \
      org.opencontainers.image.source="https://github.com/moshen/BitTrench" \
      org.opencontainers.image.version="${VERSION}"

COPY dist/linux-${TARGETARCH}/bittrench /usr/local/bin/bittrench
# Shipped so an operator can lift a reference config out of the image itself
# rather than hunting for the one that matches this version.
COPY dist/linux-${TARGETARCH}/config.sample.toml /usr/share/bittrench/config.sample.toml

# /config is the first entry in the daemon's own search path, `./config.toml`,
# so mounting a config here needs no -config flag and leaves the documented
# search order intact. The state database resolves beside whichever file is
# found, which puts it here too - so this has to be writable, not read-only.
#
# No VOLUME, here or for the downloads: an undeclared mount is a startup error
# naming the missing config, while a declared one silently becomes an anonymous
# volume - and an operator who cannot find the gigabytes they just downloaded is
# worse served than one who is told to pass -v.
WORKDIR /config

# The container's loopback is its own: a published port reaches nothing bound to
# 127.0.0.1, so the image fixes both halves of the listener. They override
# `[api] listen_interface` and `listen_port`, which means the port to publish is
# 6800 no matter what the mounted config says - and `docker run -p` maps the
# same way for everyone. Overridable in turn by `docker run -e`, for an operator
# running several containers on one network.
#
# Binding 0.0.0.0 makes the RPC endpoint and the web UI reachable from wherever
# the published port is reachable. Set `[api] username` and `password`, or
# publish to 127.0.0.1 only; the daemon warns about this combination at startup.
#
# Downloads go to a path the image names rather than one the config file does,
# for the same reason: a config written for a host, or copied between
# deployments, must not decide where a container writes. /downloads is the mount
# to pass, whatever save_path says - and a config that sets no save_path at all
# works in here, since this satisfies the requirement that one be set.
#
# Logs go to stderr only. The rolling files are for a host and for the Windows
# service, where stderr is disconnected; in a container they would grow inside a
# volume nobody reads, while the runtime is already collecting the output that
# `docker logs` prints.
ENV BITTRENCH_API_LISTEN_INTERFACE=0.0.0.0 \
    BITTRENCH_API_LISTEN_PORT=6800 \
    BITTRENCH_TORRENT_SAVE_PATH=/downloads \
    BITTRENCH_LOGGING_TO_FILE=0

# Only the RPC/UI listener. The BitTorrent listener, when `listen_in_tunnel` is
# on, lives inside the tunnel and opens no socket on this container's stack -
# there is nothing to publish and nothing an inbound peer could reach here.
EXPOSE 6800/tcp

# No USER: the daemon writes the state database and every download into bind
# mounts the operator owns, and a fixed uid that cannot write to them fails at
# startup in a way that reads as a bug in the daemon. Run it as
# yourself with `docker run --user "$(id -u):$(id -g)"` - it needs no
# capabilities either way, since the WireGuard device is entirely in user space.
ENTRYPOINT ["/usr/local/bin/bittrench"]
