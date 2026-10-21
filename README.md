# proxtail

<div align="center">

**High-Performance, Unprivileged Multi-Tailnet Layer 4 Reverse Proxy**

*Expose services across one or multiple Tailscale networks on local TCP and UDP ports—without root privileges, `/dev/net/tun`, or a host Tailscale daemon.*

[![CI & Build](https://github.com/fadlytabrani/proxtail/actions/workflows/docker-publish.yml/badge.svg)](https://github.com/fadlytabrani/proxtail/actions/workflows/docker-publish.yml)
[![GitHub Release](https://img.shields.io/github/v/release/fadlytabrani/proxtail?include_prereleases&style=flat&logo=github)](https://github.com/fadlytabrani/proxtail/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/fadlytabrani/proxtail)](https://goreportcard.com/report/github.com/fadlytabrani/proxtail)
[![Go Version](https://img.shields.io/github/go-mod/go-version/fadlytabrani/proxtail)](https://go.dev/)
[![Docker Image](https://img.shields.io/badge/ghcr.io-proxtail-blue?logo=docker)](https://github.com/fadlytabrani/proxtail/pkgs/container/proxtail)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

</div>

---

`proxtail` is an ultra-lightweight, high-performance Layer 4 (TCP and UDP) reverse proxy powered by Tailscale's userspace networking engine ([`tsnet`](https://pkg.go.dev/tailscale.com/tsnet)). It bridges local applications, Kubernetes ingress controllers (NGINX, Traefik, Envoy, HAProxy), and LAN clients directly to private backends across one or multiple Tailscale tailnets (or self-hosted [Headscale](https://github.com/juanfont/headscale) networks).

Unlike traditional Tailscale setups that require root permissions, kernel `/dev/net/tun` devices, and host-level routing changes, `proxtail` runs **100% in userspace** as an unprivileged process.

---

## Architecture Overview

### Traditional Host Daemon vs. `proxtail`

```
TRADITIONAL HOST TAILSCALED                      PROXTAIL (USERSPACE GATEWAY)
───────────────────────────                      ────────────────────────────
┌─────────────────────────┐                      ┌─────────────────────────┐
│ Host Kernel & iptables  │                      │ Local Ingress / Clients │
│ Requires root / CAP_... │                      │ (cURL, Browser, K8s)    │
│ Host TUN (/dev/net/tun) │                      └───────────┬─────────────┘
└───────────┬─────────────┘                                  │ Local TCP/UDP
            ▼                                                ▼
┌─────────────────────────┐                      ┌─────────────────────────┐
│ Single Host Tailnet     │                      │ proxtail (UID 10001)    │
│ - Modifies host routing │                      │ - No root, no TUN       │
│ - Only 1 tailnet at once│                      │ - Worker Pool & Shards  │
│ - Overlapping IPs clash │                      └──────┬────────────┬─────┘
└─────────────────────────┘                             │            │
                                         tsnet instance │            │ tsnet instance
                                         (isolated net) ▼            ▼ (isolated net)
                                               ┌──────────────┐┌──────────────┐
                                               │ Production   ││ Staging      │
                                               │ Tailnet      ││ Tailnet      │
                                               └──────────────┘└──────────────┘
```

### Visual Traffic Flow

```mermaid
flowchart TD
    subgraph Clients["Local Applications & Ingress"]
        C1["Local Client / Ingress<br/>:8080 (TCP)"]
        C2["Internal DNS Client<br/>:5353 (UDP)"]
        C3["Staging Test Runner<br/>:9080 (TCP)"]
    end

    subgraph Proxtail["proxtail Container (Unprivileged / UID 10001)"]
        L1["TCP Listener :8080"]
        L2["UDP Listener :5353"]
        L3["TCP Listener :9080"]

        WP["Worker Pool & 16-Shard Session Table<br/>(Cacheline-Padded, Lock-Free Metrics)"]

        T1["tsnet Node: 'production'<br/>WireGuard + gVisor Netstack"]
        T2["tsnet Node: 'staging'<br/>WireGuard + gVisor Netstack"]
    end

    subgraph TailnetProd["Tailnet A: Production"]
        P1["web-server:8080<br/>(100.64.0.10)"]
        P2["dns-server:53<br/>(100.64.0.53)"]
    end

    subgraph TailnetStaging["Tailnet B: Staging"]
        S1["staging-web:8080<br/>(100.64.0.10 - Overlapping IP!)"]
    end

    C1 --> L1
    C2 --> L2
    C3 --> L3

    L1 --> WP
    L2 --> WP
    L3 --> WP

    WP --> T1
    WP --> T2

    T1 -- "WireGuard Tunnel" --> P1
    T1 -- "WireGuard Tunnel" --> P2
    T2 -- "WireGuard Tunnel" --> S1
```

---

## Why `proxtail`?

- **Zero Host Privileges Required**: Runs entirely in userspace with non-root user `10001:10001`. No `CAP_NET_ADMIN`, no `/dev/net/tun` device, and zero host network modifications. Complies with the **Kubernetes Pod Security Standards `restricted` profile**.
- **Multi-Tailnet Gateway**: Connect to multiple independent tailnets (`production`, `staging`, partner tailnets) simultaneously from a single container—even when nodes share overlapping `100.64.x.x` CGNAT IPs or identical MagicDNS names.
- **WireGuard Line-Rate Performance**: Achieves **1.0 Gbps** encrypted TCP throughput over userspace WireGuard (ChaCha20-Poly1305 + gVisor), with less than **0.01 ms** proxy latency overhead on raw sockets and **~1.1 ms** round-trip overhead through full WireGuard encryption.
- **Parallel Multi-Core UDP Engine**: Multi-worker reader pool and 16-shard cacheline-padded session table scale parallel UDP traffic by **3.24x** to **~61,000 pkts/s** with zero lock contention.
- **Horizontal Kubernetes Scaling**: Ephemeral nodes scale linearly across replicas (tested **3.12x** TCP and **3.94x** UDP throughput scaling across 3 pods) using dynamic `${HOSTNAME}` node registration.
- **Instant Graceful Teardown**: Intercepts `SIGTERM` and triggers an immediate node logout (`b.Logout()`), instantly purging ephemeral nodes from your Tailscale admin console in **< 150 ms** without waiting for 30–60 minute offline timeouts.
- **Tailscale OAuth & Auto ACL Tag Discovery**: Authenticate seamlessly with reusable non-expiring OAuth client secrets (`tskey-client-...`). When `tags:` are omitted in configuration, `proxtail` queries the Tailscale OAuth API to discover and advertise authorized tags automatically.
- **Zero-Downtime Hot Reload**: Dynamic file watching and Kubernetes atomic symlink detection (`..data`) reload routes in **< 100 ms** on `config.yaml` changes without dropping existing TCP/UDP connections.
- **Cloud-Native Observability**: Built-in Prometheus metrics (`/metrics`), Kubernetes liveness (`/healthz`) and readiness (`/readyz`) probes, and optional periodic traffic summaries.

---

## Performance & Live Benchmark Highlights

All benchmarks were evaluated using both synthetic local loopback tests and end-to-end user-space WireGuard encryption (`tstest/integration/testcontrol`):

| Test Scenario | Direct Connection | `proxtail` Proxied | Impact / Overhead |
| :--- | :--- | :--- | :--- |
| **Synthetic TCP Loopback Throughput** | 14.4 Gbps | **14.2 Gbps** | < 1.4% overhead |
| **TCP Proxy Latency (p50 / p99)** | 0.07 ms / 0.12 ms | **0.08 ms / 0.14 ms** | **< 0.01 ms** proxy overhead |
| **Real-World WireGuard Throughput** | N/A | **1,005 Mbps (1.0 Gbps)** | Full ChaCha20-Poly1305 line rate |
| **Real-World WireGuard Latency** | N/A | **1.1 ms RTT** | Userspace gVisor netstack |
| **UDP Parallel Throughput (4 Cores)** | 18,900 pkts/s (1 core) | **61,200 pkts/s** | **3.24x speedup** via sharding |
| **Kubernetes Scaling (1 $\rightarrow$ 3 Pods)** | 163 req/s (1 pod) | **509 req/s (3 pods)** | **3.12x linear TCP scaling** |
| **Node Deregistration on Pod Exit** | 30–60 min (timeout) | **< 150 ms** | Instant clean teardown |

> [!TIP]
> You can reproduce these benchmarks on your local machine anytime:
> ```bash
> make bench       # Run synthetic TCP/UDP loopback benchmarks
> make bench-real  # Run real end-to-end WireGuard ChaCha20-Poly1305 + gVisor benchmarks
> ```

---

## Target Use Cases

### 1. Multi-Tenant B2B SaaS Gateways
SaaS vendors frequently need to connect their cloud workloads to private databases or internal APIs hosted in different customer tailnets. Because each customer network often allocates overlapping `100.64.0.0/10` address spaces, host-level VPN routing fails. `proxtail` encapsulates each customer tailnet in its own isolated `tsnet` network stack, letting your SaaS bridge them on distinct local ports (`:9001` $\rightarrow$ Customer A, `:9002` $\rightarrow$ Customer B).

### 2. Rootless & Hardened Kubernetes Ingress/Egress
Enterprises enforcing `PodSecurityStandard: restricted` disallow privileged containers, root UIDs, host networking, and `CAP_NET_ADMIN`. `proxtail` runs as UID `10001` with a read-only root filesystem and zero capabilities. Ingress controllers like NGINX or Traefik can point upstream traffic directly at `proxtail`'s ClusterIP service to route incoming traffic into private Tailscale backends.

### 3. Legacy Systems, Industrial & IoT Backhauls
Legacy factory floor PLCs, medical devices, or mainframe servers often cannot install modern VPN clients. By running `proxtail` on an unprivileged edge gateway or jumpbox, legacy systems communicate with simple local TCP/UDP ports, while `proxtail` securely tunnels the traffic across encrypted Tailscale meshes.

### 4. Developer Workstations & Ephemeral CI/CD Runners
Developers and CI/CD pipelines (GitHub Actions, GitLab CI) often need access to staging databases (`postgres:5432`) or internal microservices without having to install and log into the Tailscale desktop client or modify system DNS settings (`/etc/resolv.conf`). A single `docker run` or `proxtail` process immediately bridges the needed ports locally.

---

## 60-Second Quick Start

### 1. Generate an Auth Key or OAuth Client Secret
1. Open [Tailscale Admin Console → Settings → Keys](https://login.tailscale.com/admin/settings/keys).
2. Click **Generate auth key** (enable **Reusable** and **Ephemeral**), or create an **OAuth Client** under **OAuth Clients** (with `devices:core` scope).
3. Copy your `tskey-auth-...` key or `tskey-client-...` secret.

### 2. Create `config.yaml`
Create a minimal `config.yaml` in your working directory:

```yaml
tailnets:
  - ports:
      - port: 8080
        destination: 'web-server' # Forwards local TCP :8080 -> web-server:8080 in your tailnet
```
*(MagicDNS hostnames like `web-server`, full FQDNs, or Tailscale IPs like `100.64.0.10` are all supported.)*

### 3. Run `proxtail`

#### Option A: Prebuilt Multi-Arch Docker Container (`amd64` / `arm64`)
```bash
export TS_AUTHKEY="tskey-auth-xxxxx"

docker run -d --name proxtail \
  -e TS_AUTHKEY="$TS_AUTHKEY" \
  -v "$(pwd)/config.yaml:/etc/proxtail/config.yaml:ro" \
  -p 8080:8080 \
  ghcr.io/fadlytabrani/proxtail:latest
```

#### Option B: Standalone Binary
```bash
go build -o proxtail ./cmd/proxtail
export TS_AUTHKEY="tskey-auth-xxxxx"
./proxtail
```
*(When executed locally, `proxtail` automatically detects `./config.yaml` in the current working directory.)*

### 4. Test Your Connection
```bash
curl http://localhost:8080
```

---

## Common Configuration Recipes

### Recipe 1: Single Tailnet with Mixed TCP & UDP
When proxying into a single tailnet, `name` and `auth_key` can be omitted—`proxtail` automatically reads `TS_AUTHKEY` (or `TS_CLIENT_SECRET`) from the environment:

```yaml
tailnets:
  - ports:
      # TCP forwarding to identical remote port (TCP :8080 -> web-server:8080)
      - port: 8080
        destination: 'web-server'

      # TCP forwarding to different remote port (TCP :8443 -> api-server:443)
      - port: 8443
        destination: 'api-server:443'

      # UDP forwarding (UDP :5353 -> internal-dns:53)
      - port: 5353
        destination: 'internal-dns:53'
        protocol: 'udp'
        timeout: 15
```

### Recipe 2: Multi-Tailnet Gateway (Production + Staging)
Connect to multiple tailnets simultaneously. Each tailnet automatically reads its auth key or OAuth secret from `TS_AUTHKEY_<NAME>` or `TS_CLIENT_SECRET_<NAME>`:

```yaml
tailnets:
  - name: 'production' # Reads TS_AUTHKEY_PRODUCTION or TS_CLIENT_SECRET_PRODUCTION
    ports:
      - port: 8080
        destination: '100.64.0.10:8080'

  - name: 'staging'    # Reads TS_AUTHKEY_STAGING or TS_CLIENT_SECRET_STAGING
    ports:
      - port: 9080
        destination: '100.64.0.10:8080' # Complete isolation! Overlapping IPs never collide.
```

Run with environment variables:
```bash
export TS_AUTHKEY_PRODUCTION="tskey-auth-prod-xxxx"
export TS_AUTHKEY_STAGING="tskey-auth-staging-yyyy"
./proxtail -c config.yaml
```

### Recipe 3: Non-Expiring OAuth Secrets with Automatic Tag Discovery
Tailscale OAuth client secrets (`tskey-client-...`) never expire. `proxtail` automatically calls Tailscale's OAuth endpoint to discover and advertise authorized tags:

```yaml
tailnets:
  - name: 'production'
    # Tags can be explicitly declared or omitted for auto-discovery
    ports:
      - port: 5432
        destination: 'postgres-db:5432'
```

```bash
export TS_CLIENT_SECRET_PRODUCTION="tskey-client-khrEZ4..."
./proxtail -c config.yaml
```

### Recipe 4: Kubernetes Scaling with Dynamic `${HOSTNAME}`
When running multiple replicas under a Kubernetes Deployment, each pod can dynamically name its Tailscale node to prevent hostname collisions:

```yaml
tailnets:
  - name: 'production'
    hostname: 'proxtail-prod-${HOSTNAME}' # Expands pod hostname (e.g. proxtail-prod-7d4b9b94b-xk29)
    ports:
      - port: 8080
        destination: 'web-server'
```

### Recipe 5: Persistent Node Identity (`ephemeral: false`)
By default, `ephemeral: true` keeps state in memory and cleans up on exit. If you want a static, permanent Tailscale IP address across restarts:

```yaml
tailnets:
  - name: 'production'
    hostname: 'proxtail-gateway'
    ephemeral: false # State saved to TS_STATE_DIR (/var/lib/tsnet/production)
    ports:
      - port: 5432
        destination: 'postgres-db'
```

### Recipe 6: Self-Hosted Headscale
Point any tailnet to an open-source [Headscale](https://github.com/juanfont/headscale) server:

```yaml
tailnets:
  - name: 'selfhosted'
    control_url: 'https://headscale.example.com'
    ports:
      - port: 8080
        destination: 'internal-app:8080'
```

---

## Configuration Reference

### Config File Discovery Order
`proxtail` checks the following locations in order:
1. **CLI Flag**: `proxtail -c /path/to/config.yaml` (or `-config`)
2. **Environment Variable**: `PROXTAIL_CONFIG` (or `TSNET_PROXY_CONFIG`)
3. **Current Working Directory**: `./config.yaml`
4. **Default System Path**: `/etc/proxtail/config.yaml` (default inside container image)

### `tailnets[]` Settings

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `name` | `string` | `"default"` *(if 1 tailnet)* | Identifier for the tailnet. Required when multiple tailnets are defined. |
| `ports` | `list` | *(required)* | List of TCP and UDP port mappings for this tailnet. |
| `hostname` | `string` | `<system-hostname>-<name>` | Hostname registered in Tailscale. Supports `${ENV_VAR}` expansion. |
| `auth_key` | `string` | `TS_AUTHKEY_<NAME>` *(or `TS_AUTHKEY`)* | Env var name or literal auth key (`tskey-auth-...`) / OAuth secret (`tskey-client-...`). |
| `tags` | `list` | Auto-discovered from OAuth | Tailscale ACL tags to advertise (e.g., `["tag:proxy"]`). Discovered automatically for OAuth secrets. |
| `ephemeral` | `bool` | `true` | If `true`, state is in-memory and the node deregisters on exit. If `false`, persists to disk. |
| `control_url` | `string` | Tailscale Control Plane | Custom coordination URL (e.g., for Headscale). |
| `wireguard_port` | `int` | `0` *(auto)* | Optional static UDP port for direct peer-to-peer WireGuard traffic. |
| `web_client` | `bool` | `false` | Serve Tailscale's built-in node management web UI on port `5252` inside the tailnet. |

### `tailnets[].ports[]` Settings

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `port` *(or `source_port`)* | `int` | *(required)* | Local port `proxtail` listens on (`1`–`65535`). |
| `destination` | `string` | *(required)* | Destination `host:port` or `host` inside the tailnet. If `:port` is omitted, reuses `port`. Supports `${ENV_VAR}`. |
| `protocol` | `string` | `"tcp"` | Layer 4 protocol: `"tcp"` or `"udp"`. |
| `listen_address` | `string` | `proxy.listen_address` *(all)* | Local IP to bind on (e.g., `"127.0.0.1"` for loopback only). |
| `timeout` | `int` | `30` | Idle connection / session timeout in seconds (`1`–`3600`). |

### `proxy` Settings *(Optional)*

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `listen_address` | `string` | `""` *(all interfaces)* | Default local bind IP for all ports and health probes (e.g., `"127.0.0.1"`). |
| `health_port` | `int` | Disabled (`0`) | HTTP port serving `/healthz`, `/readyz`, and `/metrics`. |
| `metrics_interval` | `int` | Disabled (`60` if enabled) | Frequency (in seconds) to log periodic `METRICS:` summaries. |
| `enable_metrics` | `bool` | `false` | Explicitly enable/disable metrics logging. |
| `buffer_size` | `int` | `512` | Per-connection I/O copy buffer size in KB (`1`–`8192`). |
| `worker_pool_size` | `int` | `200` | Maximum concurrent forwarding workers (`1`–`10000`). |

### Environment Variables & CLI Flags

| Variable / Flag | Description |
| :--- | :--- |
| `-c`, `-config <path>` | Path to the YAML configuration file. |
| `-validate`, `-check` | Validate configuration syntax, port rules, and permissions, then exit (`0` valid, `1` error). |
| `-healthcheck` | Probe a running `proxtail` instance (`/readyz` or TCP listener) and exit (`0` healthy, `1` error). |
| `-v`, `-version` | Print build version and exit. |
| `TS_AUTHKEY` / `TS_CLIENT_SECRET` | Default auth key or OAuth client secret for single-tailnet mode. |
| `TS_AUTHKEY_<NAME>` | Per-tailnet auth key (`<NAME>` is uppercase with hyphens/dots converted to `_`). |
| `TS_CLIENT_SECRET_<NAME>` | Per-tailnet OAuth secret (`tskey-client-...`). |
| `PROXTAIL_CONFIG` | Path to configuration file. |
| `PROXTAIL_HEALTH_PORT` | Sets `proxy.health_port` via environment variable. |
| `PROXTAIL_LISTEN_ADDRESS` | Sets `proxy.listen_address` via environment variable. |
| `TS_STATE_DIR` | Directory used for Tailscale state storage (default in container: `/var/lib/tsnet`). |

---

## Kubernetes Production Deployment

`proxtail` complies out of the box with the **Kubernetes Pod Security Standards `restricted` profile**:
- **Numeric Non-Root User (`10001:10001`)**: Runs as `USER 10001:10001` with `securityContext.runAsNonRoot: true`.
- **Read-Only Root Filesystem**: Operates cleanly with `readOnlyRootFilesystem: true` using an `emptyDir` at `/var/lib/tsnet` and `/tmp`.
- **ConfigMap Hot Reload**: Watches the `/etc/proxtail` directory; swapping the `..data` symlink on `kubectl apply` reloads routes with zero downtime.
- **Liveness & Readiness Probes**:
  - `GET /healthz`: Liveness probe returning node status and Tailscale IP addresses.
  - `GET /readyz`: Readiness probe returning `200 OK` once all listeners are bound and active (`503 Service Unavailable` during startup or `SIGTERM` draining).
  - `GET /metrics`: Prometheus metrics scraper endpoint.
- **Graceful Shutdown**: On pod scale-down or node draining, Kubernetes sends `SIGTERM`. `proxtail` immediately deregisters its ephemeral nodes via `b.Logout()`, eliminating stale or offline nodes in Tailscale Admin.

A ready-to-deploy production manifest is available in [`examples/kubernetes.yaml`](examples/kubernetes.yaml):

```bash
# 1. Apply the production manifest (ConfigMap, Secret, Deployment, Service)
kubectl apply -f examples/kubernetes.yaml

# 2. Check rollout status
kubectl rollout status deployment/proxtail

# 3. Scale horizontally with zero port or hostname conflicts
kubectl scale deployment/proxtail --replicas=3
```

---

## Day-2 Operations & Monitoring

### Validating Configuration in CI/CD
Validate syntax and port rules before deployment:
```bash
proxtail -validate -c config.yaml
```

### Zero-Downtime Hot Reload
Modify `config.yaml` to add, update, or remove ports on the fly:
- **Automatic**: File watcher detects atomic file replacements and Kubernetes `ConfigMap` symlink updates within **100 ms**.
- **Signal**: Trigger manually via `kill -HUP $(pgrep proxtail)` or `docker kill --signal=HUP proxtail`.
- **Safe Rollback**: If an invalid configuration is introduced, `proxtail` rejects it, logs the validation error, and continues serving active traffic without interruption.

### Prometheus Metrics
When `health_port` is enabled (e.g. `PROXTAIL_HEALTH_PORT=9090`):

```bash
curl http://localhost:9090/metrics
```

Key exported metrics:
- `proxtail_tcp_connections_active`: Current active TCP proxy connections.
- `proxtail_udp_packets_total`: Total UDP packets forwarded.
- `proxtail_bytes_transferred_total`: Total network throughput in bytes.
- `proxtail_errors_total`: Count of connection or session errors.
- `proxtail_tailnets_configured`: Number of active tailnet instances.
- `proxtail_listeners_active`: Number of healthy listening ports.

---

## Troubleshooting

- **`environment variable TS_AUTHKEY_... not set`**  
  Ensure you have exported `TS_AUTHKEY` (for single tailnet) or `TS_AUTHKEY_<NAME>` (for multi-tailnet, matching the uppercase tailnet name with hyphens replaced by underscores).
- **Ephemeral node stuck in `NeedsMachineAuth`**  
  If your tailnet has Device Approval enabled, approve the node in the Tailscale Admin Console, or generate auth keys with pre-approval enabled.
- **Docker volume edits not triggering automatic reload**  
  Mounting a single file (`-v ./config.yaml:/etc/proxtail/config.yaml`) locks the file's host inode. Mount the parent directory (`-v ./config:/etc/proxtail:ro`) or send `docker kill --signal=HUP proxtail`.
- **`bind: address already in use`**  
  Another host process or another tailnet in `config.yaml` is already bound to that port. `proxtail` automatically rolls back all listeners and exits cleanly if any port fails to bind.

---

## Building & Testing from Source

```bash
# Build binary with embedded version metadata
make build

# Run formatting, vet, and race-enabled test suite
make check

# Run synthetic TCP & UDP socket throughput benchmarks
make bench

# Run end-to-end WireGuard (ChaCha20-Poly1305 + gVisor) throughput benchmarks
make bench-real

# Validate example configuration
make validate
```

---

## Contributing & Security

- **Contributing**: Please review [CONTRIBUTING.md](CONTRIBUTING.md) for architecture guidelines, code standards, and PR workflows.
- **Security Policy**: See [SECURITY.md](SECURITY.md) for vulnerability disclosure procedures.
- **Changelog**: See [CHANGELOG.md](CHANGELOG.md) for version history.

---

## License

[MIT License](LICENSE) © 2026 Fadly Tabrani
