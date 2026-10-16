# Build stage: Compile the proxtail Go program using a Go-based Alpine image
FROM golang:1.25.1-alpine3.22 AS builder

# Install required build tools: git for fetching Go dependencies, ca-certificates for secure connections
RUN apk add --no-cache git ca-certificates

# Set the working directory inside the container for the build process
WORKDIR /app

# Copy the entire source code (including go.mod, go.sum, and source files) into the container
COPY . .

# Download and cache Go module dependencies to ensure reproducible builds
RUN go mod tidy

# Build the proxtail binary
RUN go build -o proxtail ./cmd/proxtail

# Runtime stage: Create a lightweight Alpine-based image for running the application
FROM alpine:3.21

# Install runtime dependencies: ca-certificates for secure Tailscale connections
RUN apk add --no-cache ca-certificates

# Create directories for the configuration file and Tailscale state
# /etc/proxtail holds config.yaml, /var/lib/tsnet stores Tailscale state
RUN mkdir -p /etc/proxtail /var/lib/tsnet

# Set the TS_STATE_DIR environment variable to specify where Tailscale stores its state
ENV TS_STATE_DIR=/var/lib/tsnet
ENV PROXTAIL_CONFIG=/etc/proxtail/config.yaml

# Copy the compiled proxtail binary from the builder stage to the runtime image
COPY --from=builder /app/proxtail /usr/local/bin/proxtail

# Copy the sample configuration file to the default location
# This can be overridden by mounting a custom config at runtime
COPY --from=builder /app/examples/config.yaml /etc/proxtail/config.yaml

# Run the proxtail binary using the exec form to ensure signals (e.g., SIGTERM, SIGHUP)
# are properly passed to the process for graceful shutdown and hot reload
CMD ["/usr/local/bin/proxtail"]
