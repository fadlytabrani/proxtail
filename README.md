# proxtail

**Expose services from one or more Tailscale networks on local TCP and UDP ports—without root privileges, `/dev/net/tun`, or a host Tailscale daemon.**

`proxtail` is a lightweight, high-performance Layer 4 (TCP/UDP) reverse proxy built on Tailscale's userspace networking library (`tsnet`). Drop it into Docker, Kubernetes, or a Linux/macOS host to bridge local applications, ingress controllers (NGINX, Traefik, Caddy, HAProxy), or LAN clients directly to backends across one or more Tailscale tailnets (or self-hosted Headscale networks).

---

## Why `proxtail`?

- **Zero Host Privileges Required**: Runs entirely in userspace as an unprivileged user. No `CAP_NET_ADMIN`, no `/dev/net/tun` device, and no host `tailscaled` installation needed.
- **Multi-Tailnet Gateway**: Connect to multiple independent tailnets (`production`, `staging`, partner networks) from a single container—even when backends across tailnets have overlapping `100.64.x.x` Tailscale IPs or identical hostnames.
- **Convention Over Configuration**: Forward a port in 4 lines of YAML. Protocol defaults to TCP, target ports default to the local port, and auth keys are discovered automatically from environment variables.
- **Zero-Downtime Hot Reload**: Add, modify, or remove ports and tailnets on the fly via file watching or `SIGHUP` without dropping existing connections on unaffected ports.
- **Full TCP & UDP Support**: High-throughput TCP streaming (`TCP_NODELAY`, pooled buffers) and zero-allocation per-client UDP NAT session forwarding (DNS, gaming, telemetry, VoIP).

```
Local Clients / Ingress                 proxtail (unprivileged)                 Tailscale / Headscale
───────────────────────            ─────────────────────────────────           ───────────────────────
localhost:8080 (TCP)  ──────────►  :8080 ──► tsnet [production]   ──────────►  web-server:8080
localhost:5353 (UDP)  ──────────►  :5353 ──► tsnet [production]   ──────────►  internal-dns:5353
localhost:9080 (TCP)  ──────────►  :9080 ──► tsnet [staging]      ──────────►  staging-web:8080
```

---

## 60-Second Quick Start

### 1. Generate a Tailscale Auth Key
1. Open the [Tailscale Admin Console → Settings → Keys](https://login.tailscale.com/admin/settings/keys).
2. Click **Generate auth key**.
3. *(Recommended for containers)* Enable **Reusable** and **Ephemeral**, then copy your `tskey-auth-...` key.

### 2. Create `config.yaml`
Create a minimal `config.yaml` in your current directory:

```yaml
tailnets:
  - ports:
      - port: 8080
        destination: 'web-server' # Forwards local TCP :8080 -> web-server:8080 in your tailnet
```
*(You can use a Tailscale MagicDNS hostname like `web-server`, a full FQDN, or a Tailscale IP like `100.64.0.10`.)*

### 3. Run `proxtail`

**Option A: Prebuilt Docker Image (`amd64` / `arm64`)**
```bash
export TS_AUTHKEY="tskey-auth-xxxxx"
docker run -d --name proxtail \
  -e TS_AUTHKEY="$TS_AUTHKEY" \
  -v "$(pwd)/config.yaml:/etc/proxtail/config.yaml:ro" \
  -p 8080:8080 \
  ghcr.io/fadlytabrani/proxtail:latest
```

**Option B: Standalone Binary**
```bash
go build -o proxtail ./cmd/proxtail
export TS_AUTHKEY="tskey-auth-xxxxx"
./proxtail
```
*(When run locally, `proxtail` automatically loads `./config.yaml` from the current directory.)*

### 4. Test Your Connection
```bash
curl http://localhost:8080
```

---

## Common Configuration Recipes

### Recipe 1: Single Tailnet with Multiple TCP & UDP Ports
When only one tailnet is configured, you don't need to specify `name` or `auth_key`—just set `TS_AUTHKEY` in your environment:

```yaml
tailnets:
  - ports:
      # Same local and remote port (TCP :8080 -> web-server:8080)
      - port: 8080
        destination: 'web-server'

      # Different local and remote port (TCP :8443 -> api-server:443)
      - port: 8443
        destination: 'api-server:443'

      # UDP port forwarding (UDP :5353 -> dns-server:53)
      - port: 5353
        destination: 'dns-server:53'
        protocol: 'udp'
```

### Recipe 2: Multi-Tailnet Gateway (Production + Staging)
Connect to multiple tailnets simultaneously. By convention, each tailnet reads its auth key from `TS_AUTHKEY_<NAME>` (uppercase, with hyphens converted to underscores):

```yaml
tailnets:
  - name: 'production' # Reads TS_AUTHKEY_PRODUCTION
    ports:
      - port: 8080
        destination: '100.64.0.10:8080'

  - name: 'staging'    # Reads TS_AUTHKEY_STAGING
    ports:
      - port: 9080
        destination: '100.64.0.10:8080' # Overlapping Tailscale IPs are completely isolated!
```

Run with environment variables for each tailnet:
```bash
export TS_AUTHKEY_PRODUCTION="tskey-auth-prod-xxxx"
export TS_AUTHKEY_STAGING="tskey-auth-staging-yyyy"
./proxtail -c config.yaml
```

### Recipe 3: Persistent Node Identity (`ephemeral: false`)
By default, `proxtail` creates ephemeral nodes (`ephemeral: true`) stored in memory so containers leave no stale devices behind on exit. If you want `proxtail` to keep a permanent Tailscale IP and node identity across restarts:

```yaml
tailnets:
  - name: 'production'
    hostname: 'proxtail-gateway'
    ephemeral: false # Persist node state in TS_STATE_DIR (/var/lib/tsnet/production)
    ports:
      - port: 5432
        destination: 'postgres-db'
```

### Recipe 4: Self-Hosted Headscale (`control_url`)
Point any tailnet to a self-hosted [Headscale](https://github.com/juanfont/headscale) coordination server:

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
`proxtail` looks for its configuration file in the following order:
1. **CLI Flag**: `proxtail -c /path/to/config.yaml` (or `-config`)
2. **Environment Variable**: `PROXTAIL_CONFIG` (or `TSNET_PROXY_CONFIG`)
3. **Current Working Directory**: `./config.yaml` (if the file exists)
4. **Default System Path**: `/etc/proxtail/config.yaml` (default inside the Docker image)

### `tailnets[]` Settings

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `name` | `string` | `"default"` *(if 1 tailnet)* | Unique identifier for the tailnet. Required when configuring multiple tailnets. |
| `ports` | `list` | *(required)* | List of TCP/UDP port forwarding rules for this tailnet. |
| `hostname` | `string` | `<system-hostname>-<name>` | Hostname registered in the Tailscale admin console for this node. |
| `auth_key` | `string` | `TS_AUTHKEY_<NAME>` *(or `TS_AUTHKEY`)* | Env var name (or literal `tskey-auth-...` / OAuth `tskey-client-...` secret). Also checks `TS_CLIENT_SECRET_<NAME>`. |
| `tags` | `list` | `[]` | Tailscale ACL tags to advertise (e.g., `["tag:proxy"]` or `["proxy"]`). Required when using OAuth client secrets. |
| `ephemeral` | `bool` | `true` | If `true`, keeps state in memory and removes the node when disconnected. If `false`, persists state to `TS_STATE_DIR`. |
| `control_url` | `string` | Tailscale Control Plane | Custom coordination server URL (e.g., for Headscale). |
| `wireguard_port` | `int` | `0` *(auto)* | Optional fixed UDP port for direct peer-to-peer WireGuard traffic. |
| `web_client` | `bool` | `false` | Expose Tailscale's built-in node management web UI on port `5252` inside the tailnet. |

### `tailnets[].ports[]` Settings

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `port` *(or `source_port`)* | `int` | *(required)* | Local port for `proxtail` to listen on (`1`–`65535`). |
| `destination` | `string` | *(required)* | Target `host:port` or `host` inside the tailnet. If `:port` is omitted, reuses `port`. |
| `protocol` | `string` | `"tcp"` | Layer 4 protocol: `"tcp"` or `"udp"`. |
| `listen_address` | `string` | `proxy.listen_address` *(all interfaces)* | Optional local IP to bind on (e.g., `"127.0.0.1"` for localhost-only). |
| `timeout` | `int` | `30` | Idle connection/session timeout in seconds (`1`–`3600`). |

### `proxy` Settings *(Optional)*

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `listen_address` | `string` | `""` *(all interfaces)* | Default local bind IP for all forwarded ports and the health server (e.g., `"127.0.0.1"`). |
| `health_port` | `int` | Disabled (`0`) | HTTP port serving `/healthz` (liveness), `/readyz` (readiness), and `/metrics` (Prometheus). |
| `metrics_interval` | `int` | Disabled (`60` if enabled) | Log `METRICS:` summary every `N` seconds (`1`–`3600`). Setting this automatically enables metrics. |
| `enable_metrics` | `bool` | `false` | Explicitly enable/disable periodic metrics logging. |
| `buffer_size` | `int` | `512` | Per-connection I/O buffer size in KB (`1`–`8192`). |
| `worker_pool_size` | `int` | `200` | Maximum concurrent forwarding workers (`1`–`10000`). |

### Environment Variables & CLI Flags

| Variable / Flag | Description |
| :--- | :--- |
| `-c`, `-config <path>` | Path to the YAML configuration file. |
| `-validate`, `-check` | Validate configuration syntax and port rules, then exit (`0` if valid, `1` on error). |
| `-healthcheck` | Probe the running `proxtail` instance (`/readyz` or TCP listener) and exit (`0` if healthy, `1` on error). |
| `-v`, `-version` | Print the `proxtail` build version and exit. |
| `TS_AUTHKEY` / `TS_CLIENT_SECRET` | Default Tailscale auth key or OAuth client secret used in single-tailnet mode. |
| `TS_AUTHKEY_<NAME>` | Per-tailnet auth key (`<NAME>` is uppercase with hyphens/dots replaced by `_`). |
| `TS_CLIENT_SECRET_<NAME>` | Per-tailnet OAuth client secret (`tskey-client-...`) for non-expiring node registration. |
| `PROXTAIL_CONFIG` | Path to the YAML configuration file. |
| `PROXTAIL_HEALTH_PORT` | Enables `/healthz`, `/readyz`, and `/metrics` on the specified TCP port when `health_port` is omitted in YAML. |
| `PROXTAIL_LISTEN_ADDRESS` | Sets the default `proxy.listen_address` when omitted in YAML. |
| `TS_STATE_DIR` | Directory used for Tailscale state/logs (default in container: `/var/lib/tsnet`). |

---

## Kubernetes Deployment

`proxtail` is purpose-built for Kubernetes and complies out of the box with the **Kubernetes Pod Security Standards `restricted` profile**:
- **Numeric Non-Root User (`10001:10001`)**: Runs with `USER 10001:10001` so `securityContext.runAsNonRoot: true` works without requiring root privileges or `CAP_NET_ADMIN`.
- **Read-Only Root Filesystem (`readOnlyRootFilesystem: true`)**: Works cleanly with an `emptyDir` mounted at `/var/lib/tsnet` and `/tmp` (or a `PersistentVolumeClaim` when `ephemeral: false`).
- **ConfigMap Hot Reload**: Mounting a `ConfigMap` at `/etc/proxtail` automatically triggers a zero-downtime configuration reload whenever Kubernetes swaps the `..data` symlink after a `kubectl apply`.
- **Native Liveness, Readiness & Prometheus Endpoints**:
  - `GET /healthz`: Liveness probe returning JSON status and Tailscale IPs.
  - `GET /readyz`: Readiness probe returning `200 OK` only when all configured listeners are bound and active (`503 Service Unavailable` during startup and graceful `SIGTERM` drain).
  - `GET /metrics`: Prometheus metrics endpoint (`proxtail_tcp_connections_active`, `proxtail_udp_packets_total`, `proxtail_bytes_transferred_total`, `proxtail_errors_total`, `proxtail_tailnets_configured`, `proxtail_listeners_active`).

A complete production manifest (`ConfigMap`, `Secret`, `Deployment`, and `Service`) is included in [`examples/kubernetes.yaml`](examples/kubernetes.yaml):

```bash
# 1. Edit examples/kubernetes.yaml or create your Secret with Tailscale auth keys / OAuth secrets
kubectl apply -f examples/kubernetes.yaml

# 2. Check rollout status and readiness
kubectl rollout status deployment/proxtail

# 3. Update port mappings at any time — Kubernetes ConfigMap sync triggers automatic hot reload
kubectl apply -f examples/kubernetes.yaml
```

---

## Day-2 Operations

### Validating Config Before Deployment
Validate `config.yaml` in CI/CD or before triggering a reload:
```bash
proxtail -validate -c config.yaml
```

### Zero-Downtime Hot Reload
You can add, remove, or update ports and tailnets without restarting `proxtail`:
- **Automatic**: `proxtail` watches the configuration directory and automatically reloads within 100ms when `config.yaml` is saved, atomically replaced, or updated via a Kubernetes `ConfigMap` volume.
- **Manual Signal (`SIGHUP`)**:
  ```bash
  # Docker
  docker kill --signal=HUP proxtail

  # Host process
  kill -HUP $(pgrep proxtail)
  ```
- **Safe Rollback**: If a modified `config.yaml` contains a syntax or validation error, `proxtail` logs the error and keeps the existing configuration and active connections running uninterrupted.

### Health Checks, Readiness & Prometheus Metrics
When `health_port` is configured (in `config.yaml` or via `PROXTAIL_HEALTH_PORT=9090`):
- **`GET /healthz`**: Returns JSON liveness status, active counters, and each tailnet's assigned Tailscale IPv4/IPv6 addresses.
- **`GET /readyz`**: Returns `200 OK` (`{"status":"ready",...}`) when all listeners are bound and ready to accept traffic, or `503 Service Unavailable` during startup/shutdown.
- **`GET /metrics`**: Exposes Prometheus metrics (`proxtail_tcp_connections_active`, `proxtail_udp_packets_total`, `proxtail_bytes_transferred_total`, `proxtail_errors_total`, `proxtail_tailnets_configured`, `proxtail_listeners_active`).

When `metrics_interval` is set (e.g., `metrics_interval: 30`), `proxtail` also logs periodic traffic summaries:
```text
METRICS: TCP_Connections=4, UDP_Packets=1280, Bytes_Transferred=10485760, Errors=0
```

---

## Troubleshooting

- **`environment variable TS_AUTHKEY_... not set`**
  Ensure you have exported either `TS_AUTHKEY` (for a single tailnet) or `TS_AUTHKEY_<NAME>` matching your tailnet's `name` in uppercase (with hyphens replaced by underscores, e.g., `name: my-net` requires `TS_AUTHKEY_MY_NET`).
- **Container restarts fail to authenticate with single-use Auth Key**
  When `ephemeral: true` (default), `proxtail` registers a fresh ephemeral node on each container start. Either generate a **Reusable** auth key in the Tailscale Admin Console, or set `ephemeral: false` and mount a persistent volume at `/var/lib/tsnet`.
- **File edits not triggering automatic reload in Docker**
  Mounting a single file (`-v ./config.yaml:/etc/proxtail/config.yaml`) in Docker binds the file's inode; editors that save via atomic rename (`vim`, `sed -i`) change the host inode. Either mount the parent directory (`-v ./config:/etc/proxtail:ro`) or send `docker kill --signal=HUP proxtail` after editing.
- **`listen tcp :8080: bind: address already in use`**
  Another process on the host or another tailnet entry in `config.yaml` is already using port `8080`. If startup fails to bind any port, `proxtail` cleanly rolls back all listeners and exits.

---

## Building & Testing from Source

```bash
# Build binary with embedded version metadata
make build

# Run formatting, go vet, and race-enabled test suite
make check

# Run throughput benchmarks
make bench

# Validate example configuration
make validate
```

## Contributing & Security

- **Contributing**: See [CONTRIBUTING.md](CONTRIBUTING.md) for architecture details, development setup, and pull request guidelines.
- **Security Policy**: See [SECURITY.md](SECURITY.md) for supported versions and how to report vulnerabilities privately.
- **Changelog**: See [CHANGELOG.md](CHANGELOG.md) for release history.

## License

MIT License - see [LICENSE](LICENSE) for details.
