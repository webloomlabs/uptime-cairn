# syntax=docker/dockerfile:1

# One artefact, built in the one order that works: the frontend produces static
# output, that output is copied into internal/ui/dist, and only then is the Go
# binary built — //go:embed reads the directory at compile time, so a Go build
# that runs first embeds the placeholder and serves a dashboard that is not
# there (internal/ui/embed.go).
#
# Both build stages pin themselves to BUILDPLATFORM and cross-compile to
# TARGETARCH rather than running under emulation. The frontend output is
# architecture-independent and would be identical either way; the Go build is
# pure Go — modernc.org/sqlite needs no cgo — so `GOARCH=arm64` is a flag rather
# than a toolchain. Building linux/arm64 under QEMU instead would take roughly
# an order of magnitude longer to produce the same bytes.

FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
# The lockfile alone first, so a source-only change does not reinstall.
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# adapter-static is configured to write ../internal/ui/dist — outside this
# WORKDIR on purpose, because that is where the Go build expects it.
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# After the source, so it overwrites the committed .gitkeep placeholder rather
# than being overwritten by it.
COPY --from=web /src/internal/ui/dist ./internal/ui/dist

ARG TARGETOS
ARG TARGETARCH
# Defaults match internal/version: an unstamped build reports "dev", and a local
# build claiming to be a release is how an unreproducible binary ends up in a
# bug report. BUILD_DATE comes from the commit, not the clock — a wall-clock
# timestamp defeats reproducible builds on its own.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w \
        -X github.com/webloomlabs/uptime-cairn/internal/version.Version=${VERSION} \
        -X github.com/webloomlabs/uptime-cairn/internal/version.Commit=${COMMIT} \
        -X github.com/webloomlabs/uptime-cairn/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/cairn ./cmd/cairn

# Alpine rather than distroless, and the difference is about 8 MB against three
# things an operator actually needs. The documented online backup path is
# `sqlite3 … 'VACUUM INTO …'` run beside the database (docs/operations/backup-restore.md);
# HEALTHCHECK needs something that can make an HTTP request; and a monitoring
# tool that cannot be shelled into at 3am is a monitoring tool nobody trusts.
# ca-certificates is not optional at all — without it every HTTPS and TLS-expiry
# monitor fails certificate verification and reports the target down.
FROM alpine:3.22
# The group is created with an explicit GID rather than left to adduser, and
# that is load-bearing twice over: USER below names the numeric pair so the
# image works under a runtime that never reads /etc/passwd, and the compose
# file's net.ipv4.ping_group_range has to name the same GID for unprivileged
# ICMP to open. `adduser -D` alone assigns the next free GID, which is not
# 10001, and the failure is a container that cannot write to its own volume.
# `upgrade` before `add`, and it is not belt-and-braces. The base tag floats to
# the latest 3.22 patch, but apk will not replace a package the image already
# carries when something merely depends on it — so a pulled-in libcrypto3 stays
# at whatever the base image shipped even when the index holds a fixed build.
# That is exactly how a CVE with an available fix survives a rebuild, and the
# image scan fails on fixable findings rather than on findings.
RUN apk upgrade --no-cache \
 && apk add --no-cache ca-certificates tzdata sqlite \
 && addgroup -g 10001 cairn \
 && adduser -D -u 10001 -G cairn -h /data cairn \
 && mkdir -p /data \
 && chown 10001:10001 /data
COPY --from=build /out/cairn /usr/local/bin/cairn
USER 10001:10001
WORKDIR /data
VOLUME /data
EXPOSE 3000
# 127.0.0.1, not the published address: this asks the process whether it is
# serving, and routing the question through the host's network stack would make
# a proxy misconfiguration look like a dead process.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:3000/healthz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/cairn"]
# Defaults, and they match internal/config.Default() rather than restating it
# differently — a default that disagrees with the documentation is a support
# ticket waiting to happen. Everything here is overridable by appending flags to
# `docker run`, because ENTRYPOINT/CMD splits exactly that way.
CMD ["--data-dir=/data", "--listen=:3000"]
