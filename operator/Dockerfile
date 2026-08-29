# Build the manager binary.
#
# --platform=$BUILDPLATFORM pins the builder stage to the machine doing the
# building, so a linux/arm64 target cross-compiles natively instead of running
# the whole Go toolchain under QEMU. Without it a two-platform build is several
# minutes slower for no benefit — the Go compiler already emits any GOARCH.
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
WORKDIR /workspace

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
