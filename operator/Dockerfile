# Build the manager binary.
#
# --platform=$BUILDPLATFORM pins the builder stage to the machine doing the
# building, so a linux/arm64 target cross-compiles natively instead of running
# the whole Go toolchain under QEMU. Without it a two-platform build is several
# minutes slower for no benefit — the Go compiler already emits any GOARCH.
#
# Go 1.27 floor is a SECURITY floor, not a feature one. The eMudhra CA
# compromise (2026-10-20, CVE-2026-111943) was answered in the standard library
# by a crypto/x509 denylist that fails verification when one of the eight
# recoverable intermediate keys appears in a chain — shipped in go1.27.3 and
# go1.26.10 only. 1.25 is past end of support and never received it, so a binary
# built on it carries no denylist and falls back entirely to whatever root store
# the runtime image holds. Floating the minor tag keeps future patch fixes
# arriving on rebuild; do not pin back below 1.27.3.
FROM --platform=$BUILDPLATFORM golang:1.27 AS builder
WORKDIR /workspace

# Assert the floor instead of trusting the tag. `golang:1.27` is mutable, CI
# exports a type=gha,mode=max BuildKit cache, and the denylist arrived in a
# PATCH release — so "the tag says 1.27" does not establish 1.27.3. A build that
# resolves an older 1.27.x fails here rather than shipping a manager binary with
# no eMudhra denylist and nothing but the distroless root store behind it.
#
# The workflows also pass pull: true so the base is re-resolved every run; this
# check is what catches it if that ever regresses.
RUN set -eux; \
    min=go1.27.3; \
    have="$(go env GOVERSION)"; \
    echo "go toolchain: ${have} (floor ${min})"; \
    if [ "$(printf '%s\n%s\n' "$min" "$have" | sort -V | head -n1)" != "$min" ]; then \
        echo "FATAL: ${have} is below the ${min} security floor (CVE-2026-111943)" >&2; \
        exit 1; \
    fi

# Dependencies first, so a source-only change reuses the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# CGO off and a static link: the runtime image has no libc to link against.
#
# TARGETOS/TARGETARCH are declared WITHOUT defaults on purpose. BuildKit
# populates both automatically for any build, so a default is never needed —
# and `=amd64` was actively harmful: it won over the host platform, so a plain
# `docker build` on an arm64 machine produced an amd64 binary inside an
# arm64-labelled image. That fails at exec time with a confusing error rather
# than at build time with an honest one.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -a -trimpath -ldflags="-s -w" -o manager cmd/main.go

# Distroless static: no shell, no package manager, nothing to exec into.
# The nonroot variant runs as uid 65532, matching the Deployment's
# runAsNonRoot. A scratch image would satisfy that too, but this one carries
# CA certificates, which pgx needs to open a TLS connection at all.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
