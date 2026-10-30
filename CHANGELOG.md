# Changelog

All notable changes to `proxtail` will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While the project is pre-`1.0.0` (`0.y.z`), backward compatibility is not guaranteed across releases.

## [Unreleased]

## [0.3.1] - 2026-10-31

### Added
- **Zero-Tailnet Standby Mode** ([#18](https://github.com/fadlytabrani/proxtail/issues/18)):
  - Allow starting `proxtail` with 0 tailnets configured (`tailnets: []` or omitted) so container appliances and orchestrators can boot in standby mode (`GET /healthz`, `GET /readyz`, and `GET /metrics` active) and dynamically provision tailnets via `SIGHUP` hot reload.
  - Added standby mode startup logging and `0 -> N -> 0` tailnet hot-reload lifecycle tests.
- **Release & Version Metadata Enhancements**:
  - Embedded UTC build date (`main.buildDate`) into `proxtail -version` output (`proxtail vX.Y.Z (YYYY-MM-DD)`) and GitHub Release display titles.
  - Added `make release VERSION=vX.Y.Z` target in `Makefile` and updated GHCR container tagging so `main` branch builds publish `:edge`/`:main` while only stable `v*.*.*` tags move `:latest`.

## [0.3.0] - 2026-10-30

### Added
- **Automatic Tailscale VIP Service Discovery & Dynamic Proxying**:
  - Automatically discovers Tailscale Virtual IP (VIP) services (`LocalClient().GetServices()`) per tailnet and proxies their advertised TCP/UDP ports onto local listeners allocated from a reserved configurable range (`proxy.vip_port_range`, default `"30000-30999"`, override via `PROXTAIL_VIP_PORT_RANGE`).
  - Enabled by default on every tailnet via minimal tailnet-level boolean `auto_proxy_vips` (default `true`; set `auto_proxy_vips: false` to opt out per tailnet).
  - Enforces strict port reservation in `config.Validate`: manual `port` / `source_port` mappings and `proxy.health_port` are rejected if they fall inside `proxy.vip_port_range`.
  - Event-driven continuous synchronization via Tailscale's local IPN notification bus (`WatchIPNBus(ctx, ipn.NotifyInitialState)` tracking `SelfChange` / `State` with zero peer-churn overhead) paired with a configurable fallback refresh interval (`proxy.vip_refresh_interval`, default `30` seconds, override via `PROXTAIL_VIP_REFRESH_INTERVAL`).
  - Sticky local port allocation keyed by `(tailnet, service, target_port)` so dual-protocol VIP services (e.g., TCP+UDP on port `53`) share the same local port number and adding/removing VIP services at runtime never shifts or drops unaffected active connections.
  - Fail-fast startup and atomic runtime reconciliation rollback when any VIP port allocation (`vip_port_range` exhaustion or wildcard port range) or socket bind fails, plus `vip_service` attribution on auto-proxied entries in `GET /healthz` `ports[]`.
- **Active TCP & UDP Upstream Probing, Local Listener Suspension & Per-Port Health Status**:
  - Added opt-in active upstream reachability probing (`proxy.probe` with `enabled`, `interval`, `timeout`, `consecutive_failures`, `cooldown`, `PROXTAIL_PROBE_ENABLED`, and per-port `probe: true/false` overrides) with hot-reload support.
  - Automatically suspends local TCP `net.Listener` (`TCP RST` / `ECONNREFUSED`, supporting half-open SYN monitors such as F5 BIG-IP `tcp_half_open`) and UDP `net.PacketConn` (`ICMP Port Unreachable`) when upstream destinations are unreachable, and re-binds automatically upon recovery.
  - Implemented 5-point low-CPU probing optimizations: zero-overhead opt-in fast path, coalesced traffic-aware cooldown (`250ms` CAS window), asymmetric fast-fail on live traffic vs. consecutive idle threshold, 2-second per-tailnet cached in-memory Tailscale peer online lookup before dialing, and staggered startup/interval jitter (`±20%`).
  - Exposed per-port runtime health status (`ports[]`: `source_port`, `listen_address`, `listen_addr`, `destination`, `protocol`, `tailnet`, `up`, `last_error`) in `GET /healthz`, and updated `GET /readyz` to return `503 Service Unavailable` when all configured listeners are suspended.
- **Disabled `netns` in Userspace Mode**:
  - Explicitly disabled `tailscale.com/net/netns` socket binding by default (`netns.SetEnabled(false)`), eliminating `[unexpected] netns: SO_BINDTODEVICE("lo") ... operation not permitted` warnings on Linux and Kubernetes restricted pods without `CAP_NET_RAW`.
  - Added `PROXTAIL_NETNS_ENABLED=true` environment variable override to re-enable `netns` if ever required.
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
