# Build the manager binary.
FROM golang:1.25 AS builder
WORKDIR /workspace

# Dependencies first, so a source-only change reuses the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# CGO off and a static link: the runtime image has no libc to link against.
# TARGETOS/TARGETARCH come from buildx so the same file cross-builds.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
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
