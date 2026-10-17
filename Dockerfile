# syntax=docker/dockerfile:1

# Build stage: Cross-compile natively on the host build platform for fast multi-arch builds
FROM --platform=$BUILDPLATFORM golang:1.25.1-alpine3.22 AS builder

ARG TARGETOS=linux
ARG TARGETARCH

# Install build dependencies
RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Cache Go module dependencies before copying source code
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and compile a static, stripped binary for the target architecture
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o proxtail ./cmd/proxtail

# Runtime stage: Lightweight Alpine image running as an unprivileged user
FROM alpine:3.22

# Install CA certificates for Tailscale control plane TLS verification
RUN apk add --no-cache ca-certificates

# Create non-root user and directories for configuration and persistent Tailscale state
RUN addgroup -S proxtail && adduser -S -G proxtail proxtail && \
    mkdir -p /etc/proxtail /var/lib/tsnet && \
    chown -R proxtail:proxtail /etc/proxtail /var/lib/tsnet

ENV TS_STATE_DIR=/var/lib/tsnet
ENV PROXTAIL_CONFIG=/etc/proxtail/config.yaml

# Copy compiled binary and default example configuration
COPY --from=builder /app/proxtail /usr/local/bin/proxtail
COPY --from=builder --chown=proxtail:proxtail /app/examples/config.yaml /etc/proxtail/config.yaml

USER proxtail

# Exec form ensures SIGTERM and SIGHUP reach proxtail directly for graceful shutdown and hot reload
CMD ["/usr/local/bin/proxtail"]
