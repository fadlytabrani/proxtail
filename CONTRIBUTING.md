# Contributing to `proxtail`

Thank you for your interest in contributing to `proxtail`! This guide covers our architecture, development workflow, testing standards, and pull request process.

---

## Architecture Overview

`proxtail` is structured as a minimal, modular Go project:

```text
proxtail/
├── cmd/proxtail/          # CLI entrypoint, tsnet lifecycle manager, config watcher, and HTTP health/metrics server
├── internal/config/       # YAML configuration parsing, convention-over-configuration defaults, and validation
├── internal/proxy/        # High-throughput TCP/UDP forwarding engine, buffer pools, and hot-reload state machine
└── examples/              # Ready-to-run Docker Compose, Kubernetes, and multi-tailnet YAML examples
```

---

## Development Setup

### Prerequisites
- **Go `1.27.2+`** *(Note: If you have Go `1.21+` installed locally, the `Makefile` automatically sets `GOTOOLCHAIN=auto` to download and run the exact Go toolchain specified in `go.mod`.)*
- **Docker** *(optional, for building and testing container images)*

### Common `make` Targets

| Command | Description |
| :--- | :--- |
| `make build` | Compile the `./proxtail` binary with version metadata. |
| `make test` | Run all unit and integration tests. |
| `make race` | Run the full test suite with the Go `-race` detector enabled. |
| `make bench` | Run synthetic socket TCP and UDP throughput benchmarks (`internal/proxy`). |
| `make bench-real` | Run real end-to-end `tsnet` + WireGuard (`gVisor` + `ChaCha20-Poly1305`) throughput benchmarks. |
| `make vet` | Run `go vet ./...` static analysis. |
| `make check` | Run `fmt`, `vet`, and `race` tests (run before opening a PR). |
| `make validate` | Build `proxtail` and validate `examples/config.yaml`. |
| `make docker-build` | Build the container image locally. |

---

## Dependency Management Guidelines

- **Do not run unrestricted `go get -u ./...`** across indirect dependencies. `tailscale.com` pins a specific commit of `gvisor.dev/gvisor` for its userspace netstack (`gonet`); upgrading `gvisor.dev/gvisor` independently to `master` breaks standard `go mod` builds.
- When upgrading `tailscale.com`, target the direct dependency explicitly:
  ```bash
  GOTOOLCHAIN=auto go get tailscale.com@vX.Y.Z
  GOTOOLCHAIN=auto go mod tidy
  ```

---

## Coding & Testing Standards

1. **Zero Data Races**: All concurrent code paths (TCP/UDP forwarding, hot reload, tailnet lifecycle, metrics) must pass `make race` (`go test -v -race ./...`).
2. **Performance & Allocations**: Changes to the hot data path in `internal/proxy` should be verified with `make bench` (`go test -bench=. -benchmem ./internal/proxy`) to avoid regressions in throughput or heap allocations per packet/stream.
3. **Backwards Compatibility & Defaults**: Configuration changes in `internal/config` should preserve convention-over-configuration defaults and include unit tests in `internal/config/config_test.go`.
4. **Documentation**: Any new configuration field, CLI flag, or environment variable must be documented in `README.md` and `examples/`.

---

## Commit Message Conventions

We follow [Conventional Commits](https://www.conventionalcommits.org/):
- `feat:` — New user-facing feature (e.g., `feat: enrich unified /healthz endpoint with self and peer metadata`)
- `fix:` — Bug fix (e.g., `fix: prevent UDP session leak on listener stop`)
- `perf:` — Performance optimization
- `docs:` — Documentation or example updates
- `ci:` — GitHub Actions or build pipeline changes
- `test:` — Adding or refactoring tests
- `chore:` — Maintenance and dependency updates

---

## Pull Request Process

1. Fork the repository and create a feature branch from `main`.
2. Make your changes and add unit/integration tests covering the new behavior.
3. Run `make check` and `make validate` locally and ensure all checks pass.
4. Open a Pull Request against `main` and fill out the PR template checklist.
