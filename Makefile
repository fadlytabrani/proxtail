BINARY_NAME := proxtail
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GO          := GOTOOLCHAIN=auto go
LDFLAGS     := -s -w -X main.version=$(VERSION)
IMAGE_REPO  ?= ghcr.io/fadlytabrani/proxtail

.PHONY: all build test race bench vet fmt check validate docker-build clean help

all: check build

## build: Compile the proxtail binary with embedded version
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY_NAME) ./cmd/proxtail

## test: Run unit and integration tests
test:
	$(GO) test -v ./...

## race: Run unit and integration tests with the Go race detector enabled
race:
	$(GO) test -v -race ./...

## bench: Run TCP and UDP throughput benchmarks
bench:
	$(GO) test -run=^$$ -bench=. -benchmem ./internal/proxy

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
