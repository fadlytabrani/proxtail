# Changelog

All notable changes to `proxtail` will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Zero-Port Discovery Mode**:
  - Allow starting `proxtail` with zero port mappings (`ports: []` or omitted) for initial tailnet peer and Virtual IP (VIP) services discovery via `GET /healthz`.
  - Allow dynamic hot-reload transitions down to 0 ports (`ReloadConfig`), gracefully closing active TCP and UDP listeners without dropping tailnet connectivity.
  - Added discovery mode startup logging when running with 0 configured port mappings.
  - Supported `/readyz` readiness probe returning `200 OK` in zero-port mode once tailnet nodes are connected.
  - Updated `runHealthCheck` (`-healthcheck`) to return an informative error when neither `health_port` nor TCP listeners are configured.

## [0.2.0] - 2026-10-29

### Added
- **Tailnet Peer & Virtual IP (VIP) Services Discovery**:
  - Direct peer discovery in `GET /healthz` via `tsnet.Server.LocalClient().Status()`.
  - Surfaces active peers, Tailscale IPv4/IPv6 addresses, hostname, MagicDNS names, OS, online/active status, rx/tx counters, and last seen timestamps.
  - Tailscale Virtual IP (VIP) services discovery in `GET /healthz` via `tsnet.Server.LocalClient().GetServices()`.
  - Automatically resolves MagicDNS FQDNs for VIP services (`<service-name>.<magic-dns-suffix>`) using `st.CurrentTailnet.MagicDNSSuffix`, alongside VIP IPv4/IPv6 addresses and exposed proto-port ranges.
  - Streamlined single-pass discovery per tailnet (`discoverTailnetDetails`) with a bounded 2-second timeout context and graceful failure tolerance.
- **Cross-Platform Standalone Binaries & Distribution**:
  - Cross-compiled standalone executables across 7 targets: Linux (`amd64`, `arm64`), macOS (`amd64`, `arm64`), Windows (`amd64`, `arm64`), and FreeBSD (`amd64`).
  - Added unversioned standalone binaries (`proxtail-<os>-<arch>`) on GitHub Releases for simple curl/wget installation scripts.
  - Packaged release archives (`.tar.gz` and `.zip`) including `README.md` and `LICENSE`, alongside `SHA256SUMS` verification manifest.
- **Performance & Multiprocessing Optimizations**:
  - Multi-core UDP ingress reader pool (`min(GOMAXPROCS, 8)` workers) scaling parallel UDP throughput by 3.24x to ~61,000 pkts/s.
  - 16-shard cache-line-padded `udpSessionTable` and striped atomic counters (`paddedAtomicInt64`), eliminating cross-core false sharing and lock contention.
  - Reduced per-connection TCP bridging goroutines by 33% (2 goroutines instead of 3 per connection).
  - Profile-Guided Optimization (PGO) support with `cmd/proxtail/default.pgo` generated from real WireGuard workloads.
  - Coalesced shared UDP listener write deadlines and socket buffer tuning.
  - End-to-end real Tailscale WireGuard throughput benchmark suite (`make bench-real`) using `tailscale.com/tstest/integration/testcontrol`.
- **OAuth & Configuration Enhancements**:
  - Automatic Tailscale OAuth ACL tag discovery (`resolveOAuthTags`), fetching authorized tags directly from Tailscale's token endpoint when omitted in configuration.
  - Environment variable expansion (`os.ExpandEnv`) in YAML configurations (e.g. `hostname: proxtail-${HOSTNAME}`).
- **Kubernetes Support**:
  - Added `GET /readyz` readiness endpoint (`200 OK` when all listeners are bound, `503 Service Unavailable` during startup/shutdown).
  - Added Kubernetes `ConfigMap` symlink (`..data`) atomic swap detection for zero-downtime hot reloads.
  - Added `resolveTailnetDir` fallback to `os.TempDir()` for `readOnlyRootFilesystem: true` compatibility.
  - Updated `Dockerfile` to run as numeric UID/GID `10001:10001` for Kubernetes `securityContext.runAsNonRoot: true` (`restricted` Pod Security Standard) and added container `HEALTHCHECK`.
  - Added `PROXTAIL_HEALTH_PORT` and `PROXTAIL_LISTEN_ADDRESS` environment variables.
  - Added `-healthcheck` and `-version` (`-v`) CLI flags.
  - Added production-ready Kubernetes manifest (`examples/kubernetes.yaml`).
- **Tailscale & Proxy Core**:
  - Added OAuth Client Secret (`tskey-client-...` / `TS_CLIENT_SECRET_<NAME>`) support for non-expiring node authentication.
  - Added Tailscale ACL `tags` (`AdvertiseTags`), `wireguard_port`, and `web_client` configuration options.
  - Added global and per-port `listen_address` IP binding support.
  - Added optional HTTP health (`GET /healthz`) and Prometheus metrics (`GET /metrics`) server (`proxy.health_port`).
  - Added `-validate` (`-check`) CLI flag for CI/CD configuration validation.
  - Parallelized multi-tailnet startup using `sync.WaitGroup.Go`.
- **CI/CD & Project Governance**:
  - Added multi-arch (`linux/amd64`, `linux/arm64`) GitHub Actions workflow publishing Cosign-signed images to `ghcr.io`.
  - Added automated GitHub Release workflow, Dependabot configuration, `Makefile`, issue/PR templates, `CONTRIBUTING.md`, `SECURITY.md`, and `CODE_OF_CONDUCT.md`.

### Changed
- Upgraded toolchain and dependencies to Go `1.27.2`, Alpine `3.24`, `tailscale.com v1.104.1`, and `fsnotify v1.10.1`.
- Simplified configuration schema with convention-over-configuration defaults (`port` alias, auto-inferred destination port, default `TS_AUTHKEY_<NAME>` discovery, and single-tailnet `TS_AUTHKEY` fallback).
- Optimized TCP streaming (`19+ GB/s` per stream) and zero-allocation UDP session lookup (`netip.AddrPort` keys, cache-line padded atomic counters, and coalesced deadline updates).
