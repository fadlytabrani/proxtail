# syntax=docker/dockerfile:1

# Build stage: Cross-compile natively on the host build platform for fast multi-arch builds
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24 AS builder

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG BUILD_DATE=""

# Install build dependencies
RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Cache Go module dependencies before copying source code
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and compile a static, stripped binary for the target architecture
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.buildDate=${BUILD_DATE}" -o proxtail ./cmd/proxtail

# Runtime stage: Lightweight Alpine image running as an unprivileged user
FROM alpine:3.24

# Install CA certificates for Tailscale control plane TLS verification
RUN apk add --no-cache ca-certificates

# Create non-root user with fixed numeric UID/GID (10001:10001) for Kubernetes runAsNonRoot compatibility
RUN addgroup -g 10001 -S proxtail && \
    adduser -u 10001 -S -G proxtail -h /var/lib/tsnet proxtail && \
    mkdir -p /etc/proxtail /var/lib/tsnet && \
    chown -R 10001:10001 /etc/proxtail /var/lib/tsnet

ENV TS_STATE_DIR=/var/lib/tsnet
ENV PROXTAIL_CONFIG=/etc/proxtail/config.yaml

# Copy compiled binary and default example configuration
COPY --from=builder /app/proxtail /usr/local/bin/proxtail
COPY --from=builder --chown=10001:10001 /app/examples/config.yaml /etc/proxtail/config.yaml

# Numeric UID:GID required by Kubernetes securityContext.runAsNonRoot: true
USER 10001:10001

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/proxtail", "-healthcheck"]

# Exec form ensures SIGTERM and SIGHUP reach proxtail directly for graceful shutdown and hot reload
CMD ["/usr/local/bin/proxtail"]
