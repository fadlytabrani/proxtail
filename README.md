# proxtail

<div align="center">

**Forward local TCP/UDP ports into one or more Tailscale networks—without root privileges, `/dev/net/tun`, or a host Tailscale daemon.**

[![CI & Build](https://github.com/fadlytabrani/proxtail/actions/workflows/docker-publish.yml/badge.svg)](https://github.com/fadlytabrani/proxtail/actions/workflows/docker-publish.yml)
[![GitHub Release](https://img.shields.io/github/v/release/fadlytabrani/proxtail?color=blue&logo=github)](https://github.com/fadlytabrani/proxtail/releases)
[![Go Version](https://img.shields.io/github/go-mod/go-version/fadlytabrani/proxtail?logo=go&logoColor=white)](https://go.dev/)
[![Go Reference](https://pkg.go.dev/badge/github.com/fadlytabrani/proxtail.svg)](https://pkg.go.dev/github.com/fadlytabrani/proxtail)
[![License: BSD-3-Clause](https://img.shields.io/github/license/fadlytabrani/proxtail?color=blue)](LICENSE)

</div>

---

### The Problem
Running a standard Tailscale daemon requires root privileges, access to the host kernel `/dev/net/tun` device, and modifications to host network routing tables. In locked-down environments—such as **Kubernetes `restricted` pods**, **unprivileged Docker containers**, **AWS ECS/Fargate**, or **shared developer bastion hosts**—this is either impossible or strictly forbidden by security policies.

Furthermore, if you need to bridge services across **multiple independent tailnets** (such as Production, Staging, and customer VPCs) that share overlapping `100.64.0.0/10` CGNAT address spaces, a single host Tailscale daemon cannot isolate them without IP conflicts.

### The Solution: `proxtail`
`proxtail` is a lightweight, high-performance Layer 4 (TCP & UDP) reverse proxy powered by Tailscale's userspace networking engine ([`tsnet`](https://pkg.go.dev/tailscale.com/tsnet)). It runs entirely as an unprivileged process (UID `10001`), creates isolated userspace WireGuard network stacks for each configured tailnet, and maps local ports directly to private tailnet backends—delivering **1.0 Gbps WireGuard line-rate throughput** with zero kernel modifications.

---

## ⚡ Instant Demo

Forward local port `8080` to `web-server:8080` in your tailnet in three commands:

```bash
# 1. Create a 4-line configuration
cat << 'EOF' > config.yaml
tailnets:
  - ports:
      - port: 8080
        destination: 'web-server'
EOF

# 2. Run proxtail with your Tailscale auth key (or OAuth client secret)
export TS_AUTHKEY="tskey-auth-xxxxx"
proxtail
```

```text
2026/10/22 03:20:00 [default] tsnet node started (Tailscale IP: 100.64.1.42, hostname: proxtail-default)
2026/10/22 03:20:00 [default] TCP listening on :8080 and forwarding to web-server:8080
2026/10/22 03:20:01 Proxtail is ready to accept traffic (/readyz OK)
```

```bash
# 3. Access your private tailnet service directly through localhost
curl http://localhost:8080
```

---

## 🎯 Who Is It For?

- **Kubernetes Operators & Platform Engineers**: Route traffic from cluster workloads or ingress controllers (NGINX, Traefik, Kong) into private Tailscale services under the strictest `PodSecurityStandard: restricted` profile.
- **B2B SaaS Architects**: Bridge your cloud application to multiple customer tailnets simultaneously without IP collisions or cross-tenant traffic leakage.
- **DevOps & CI/CD Teams**: Enable ephemeral test runners (GitHub Actions, GitLab CI) to connect directly to staging databases (`postgres:5432`) without installing desktop VPN clients or changing host DNS.
- **Edge & IoT Engineers**: Provide legacy equipment, microcontrollers, or factory PLCs that cannot run modern WireGuard with secure access to cloud tailnets through simple local ports.

---

## 📖 Contents

- [Architecture](#architecture)
- [Key Features](#key-features)
- [Performance & Benchmarks](#performance--benchmarks)
- [60-Second Quick Start](#60-second-quick-start)
- [Configuration Recipes](#configuration-recipes)
- [Configuration Reference](#configuration-reference)
- [Kubernetes Production Guide](#kubernetes-production-guide)
- [Day-2 Operations & Observability](#day-2-operations--observability)
- [Troubleshooting](#troubleshooting)
- [Building from Source](#building-from-source)
- [Acknowledgements](#acknowledgements)
- [Contributing & License](#contributing--license)

---

## Architecture

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

### End-to-End Traffic Flow

```mermaid
flowchart TD
    subgraph Clients["Local Applications & Ingress Controllers"]
        C1["Local Ingress / App<br/>:8080 (TCP)"]
        C2["Internal DNS Client<br/>:5353 (UDP)"]
        C3["Staging Test Suite<br/>:9080 (TCP)"]
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

    T1 -- "Encrypted WireGuard" --> P1
    T1 -- "Encrypted WireGuard" --> P2
    T2 -- "Encrypted WireGuard" --> S1
```

---

## Key Features

- **Zero Host Privileges**: Runs 100% in userspace with non-root user `10001:10001`. No `CAP_NET_ADMIN`, no `/dev/net/tun` device, and zero host network changes. Fully compliant with Kubernetes Pod Security Standards `restricted` profile.
- **Multi-Tailnet Isolation**: Connect to multiple independent tailnets (`production`, `staging`, partner networks) simultaneously. Backends with overlapping `100.64.x.x` CGNAT IPs or identical MagicDNS names are completely isolated.
- **WireGuard Line-Rate Performance**: Up to **1.0 Gbps** encrypted TCP throughput over userspace WireGuard (ChaCha20-Poly1305 + gVisor), with less than **0.01 ms** raw socket proxy overhead and **~1.1 ms** round-trip overhead through full encryption.
- **Parallel Multi-Core UDP Engine**: Multi-worker reader pool and 16-shard cacheline-padded session table scale parallel UDP traffic by **3.24x** to **~61,000 pkts/s** with zero lock contention.
- **Horizontal Kubernetes Scaling**: Scales linearly across pod replicas (tested **3.12x** TCP and **3.94x** UDP throughput scaling across 3 pods) using dynamic `${HOSTNAME}` node registration.
- **Instant Clean Teardown**: Ephemeral nodes are deregistered immediately on `SIGTERM` via `b.Logout()`, purging nodes from the Tailscale admin console in **< 150 ms** without waiting for 30–60 minute timeouts.
- **Tailscale OAuth & Auto ACL Tag Discovery**: Authenticate with non-expiring OAuth client secrets (`tskey-client-...`). When `tags:` are omitted in YAML, `proxtail` automatically queries the Tailscale API to discover and advertise authorized tags.
- **Zero-Downtime Hot Reload**: Dynamic file watching and Kubernetes atomic symlink detection (`..data`) reload routes in **< 100 ms** without dropping active connections.
- **Native Observability**: Prometheus metrics (`/metrics`), Kubernetes liveness (`/healthz`) and readiness (`/readyz`) probes, and optional periodic traffic summaries.

---

## Performance & Benchmarks

All benchmarks were evaluated using both synthetic local loopback tests and end-to-end userspace WireGuard encryption (`tstest/integration/testcontrol`):

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
> Run benchmarks locally:
> ```bash
> make bench       # Synthetic TCP/UDP loopback benchmarks
> make bench-real  # End-to-end WireGuard ChaCha20-Poly1305 + gVisor benchmarks
> ```

---

## 60-Second Quick Start

### 1. Generate an Auth Key or OAuth Client Secret
1. Open [Tailscale Admin Console → Settings → Keys](https://login.tailscale.com/admin/settings/keys).
2. Click **Generate auth key** (enable **Reusable** and **Ephemeral**), or create an **OAuth Client** under **OAuth Clients** (with `devices:core` scope).
3. Copy your `tskey-auth-...` key or `tskey-client-...` secret.

### 2. Create `config.yaml`
```yaml
tailnets:
  - ports:
      - port: 8080
        destination: 'web-server' # Forwards local TCP :8080 -> web-server:8080
```
*(Supports MagicDNS names like `web-server`, FQDNs, or Tailscale IPs like `100.64.0.10`.)*

### 3. Run `proxtail`

#### Option A: Prebuilt Docker Container (`amd64` / `arm64`)
```bash
export TS_AUTHKEY="tskey-auth-xxxxx"

docker run -d --name proxtail \
  -e TS_AUTHKEY="$TS_AUTHKEY" \
  -v "$(pwd)/config.yaml:/etc/proxtail/config.yaml:ro" \
  -p 8080:8080 \
  ghcr.io/fadlytabrani/proxtail:latest
```

#### Option B: Standalone Binary
Download prebuilt standalone executables for Linux, macOS, or Windows from [GitHub Releases](https://github.com/fadlytabrani/proxtail/releases), or compile from source:
```bash
# Example: Download Linux amd64 standalone executable directly
curl -Lo proxtail https://github.com/fadlytabrani/proxtail/releases/latest/download/proxtail-linux-amd64
chmod +x proxtail

# Or compile from source:
# go build -o proxtail ./cmd/proxtail

export TS_AUTHKEY="tskey-auth-xxxxx"
./proxtail
```

#### Option C: Kubernetes Deployment
```bash
kubectl apply -f examples/kubernetes.yaml
```

---

## Configuration Recipes

### Recipe 1: Single Tailnet with Mixed TCP & UDP
In single-tailnet mode, `name` and `auth_key` can be omitted—`proxtail` automatically reads `TS_AUTHKEY` (or `TS_CLIENT_SECRET`) from the environment:

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
Connect to multiple tailnets simultaneously. Each tailnet automatically reads its credentials from `TS_AUTHKEY_<NAME>` or `TS_CLIENT_SECRET_<NAME>`:

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
    # 'tags:' omitted: proxtail automatically discovers authorized ACL tags
    ports:
      - port: 5432
        destination: 'postgres-db:5432'
```

```bash
export TS_CLIENT_SECRET_PRODUCTION="tskey-client-khrEZ4..."
./proxtail -c config.yaml
```

### Recipe 4: Kubernetes Scaling with Dynamic `${HOSTNAME}`
When running multiple pod replicas in Kubernetes, each pod dynamically names its Tailscale node to prevent hostname collisions:

```yaml
tailnets:
  - name: 'production'
    hostname: 'proxtail-prod-${HOSTNAME}' # Expands pod hostname (e.g. proxtail-prod-7d4b9b94b-xk29)
    ports:
      - port: 8080
        destination: 'web-server'
```

### Recipe 5: Persistent Node Identity (`ephemeral: false`)
By default, `ephemeral: true` keeps state in memory and cleans up on exit. To maintain a static, permanent Tailscale IP address across restarts:

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
Point any tailnet to an open-source [Headscale](https://github.com/juanfont/headscale) coordination server:

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
4. **Default System Path**: `/etc/proxtail/config.yaml` (default in container)

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

## Kubernetes Production Guide

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

## Day-2 Operations & Observability

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

## Building from Source

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

## Acknowledgements

This project was built and optimized with the assistance of LLMs:
- **Gemini 4 Argon**
- **Gemini 3.8 Flash**

---

## Contributing & License

- **Contributing**: Please review [CONTRIBUTING.md](CONTRIBUTING.md) for architecture guidelines, code standards, and PR workflows.
- **Security Policy**: See [SECURITY.md](SECURITY.md) for vulnerability disclosure procedures.
- **Changelog**: See [CHANGELOG.md](CHANGELOG.md) for version history.
- **License**: [BSD 3-Clause License](LICENSE) © 2026 Fadly Tabrani
