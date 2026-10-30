BINARY_NAME := proxtail
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_DATE  ?= $(shell date -u +%Y-%m-%d)
GO          := GOTOOLCHAIN=auto go
LDFLAGS     := -s -w -X main.version=$(VERSION) -X main.buildDate=$(BUILD_DATE)
PGO_PROFILE := $(CURDIR)/cmd/proxtail/default.pgo
IMAGE_REPO  ?= ghcr.io/fadlytabrani/proxtail

.PHONY: all build pgo test race bench bench-real vet fmt check validate docker-build release clean help

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
	docker build --build-arg VERSION=$(VERSION) --build-arg BUILD_DATE=$(BUILD_DATE) -t $(IMAGE_REPO):$(VERSION) -t $(IMAGE_REPO):latest .

## release: Validate, test, stamp CHANGELOG.md, and create annotated release tag (usage: make release VERSION=vX.Y.Z)
release:
	@if [ -z "$( filter v%,$(VERSION) )" ] || ! echo "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$'; then \
		echo "Error: VERSION must be a valid SemVer tag (e.g. make release VERSION=v0.3.1)"; \
		exit 1; \
	fi
	@if git rev-parse "$(VERSION)" >/dev/null 2>&1; then \
		echo "Error: Git tag $(VERSION) already exists"; \
		exit 1; \
	fi
	@if ! git diff --quiet || ! git diff --cached --quiet; then \
		echo "Error: Working tree has uncommitted changes; commit or stash before releasing"; \
		exit 1; \
	fi
	$(MAKE) check validate VERSION=$(VERSION) BUILD_DATE=$(BUILD_DATE)
	@REL_VER="$${VERSION#v}"; \
	if ! grep -q "^## \[$$REL_VER\]" CHANGELOG.md; then \
		awk -v ver="$$REL_VER" -v dt="$(BUILD_DATE)" \
			'/^## \[Unreleased\]/ { print; print ""; print "## [" ver "] - " dt; next } { print }' \
			CHANGELOG.md > CHANGELOG.md.tmp && mv CHANGELOG.md.tmp CHANGELOG.md; \
		git add CHANGELOG.md; \
		git commit -m "chore(release): prepare $(VERSION)"; \
	fi
	git tag -a "$(VERSION)" -m "proxtail $(VERSION) ($(BUILD_DATE))"
	@echo "Created annotated tag $(VERSION) ($(BUILD_DATE))."
	@echo "Run 'git push origin main --follow-tags' to publish."

## clean: Remove build artifacts
clean:
	rm -f $(BINARY_NAME) $(BINARY_NAME)-* *.out *.test

## help: Show available make targets
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/  /'
