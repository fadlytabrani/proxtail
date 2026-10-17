BINARY_NAME := proxtail
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GO          := GOTOOLCHAIN=auto go
LDFLAGS     := -s -w -X main.version=$(VERSION)
PGO_PROFILE := $(CURDIR)/cmd/proxtail/default.pgo
IMAGE_REPO  ?= ghcr.io/fadlytabrani/proxtail

.PHONY: all build pgo test race bench bench-real vet fmt check validate docker-build clean help

all: check build

## build: Compile the proxtail binary with embedded version and PGO (cmd/proxtail/default.pgo)
build:
	CGO_ENABLED=0 $(GO) build -pgo=auto -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY_NAME) ./cmd/proxtail

## pgo: Regenerate cmd/proxtail/default.pgo from real end-to-end tsnet + WireGuard benchmarks
pgo:
	$(GO) test -run=^$$ -bench='^BenchmarkRealTSNet' -benchmem -benchtime=3s -cpuprofile=$(PGO_PROFILE) ./internal/proxy

## test: Run unit and integration tests
test:
	$(GO) test -v ./...

## race: Run unit and integration tests with the Go race detector enabled
race:
	$(GO) test -v -race ./...

## bench: Run synthetic socket TCP and UDP throughput benchmarks (with PGO)
bench:
	$(GO) test -pgo=$(PGO_PROFILE) -run=^$$ -bench='^Benchmark(TCP|UDP|Multi)' -benchmem ./internal/proxy

## bench-real: Run real end-to-end tsnet + WireGuard (ChaCha20-Poly1305 + gVisor) throughput benchmarks (with PGO)
bench-real:
	$(GO) test -pgo=$(PGO_PROFILE) -run=^$$ -bench='^BenchmarkRealTSNet' -benchmem -benchtime=3s ./internal/proxy

## vet: Run go vet static analysis
vet:
	$(GO) vet ./...

## fmt: Format all Go source files and verify cleanliness
fmt:
	$(GO) fmt ./...

## validate: Validate the example configuration files
validate: build
	./$(BINARY_NAME) -validate -c examples/config.yaml

## check: Run formatting, static analysis, and race-enabled test suite
check: fmt vet race

## docker-build: Build the container image locally
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE_REPO):$(VERSION) -t $(IMAGE_REPO):latest .

## clean: Remove build artifacts
clean:
	rm -f $(BINARY_NAME) $(BINARY_NAME)-* *.out *.test

## help: Show available make targets
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/  /'
