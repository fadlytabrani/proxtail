# proxtail

A high-performance Layer 4 (TCP/UDP) reverse proxy for Tailscale networks, written in Go. `proxtail` forwards local TCP and UDP traffic to services across one or more Tailscale networks (`tailnets`) using userspace networking (`tsnet`) without requiring root privileges or a host Tailscale daemon.

## Features

- **Multi-Tailnet Support**: Connect to multiple Tailscale networks simultaneously, each with its own `tsnet.Server` instance, hostname, auth key, and ephemeral/persistent state configuration.
- **High Performance**: Pooled `OptimizedBuffer` memory management, bounded worker pools, TCP socket tuning (`TCP_NODELAY`, 30s keepalive), and per-client-address UDP session NAT forwarding.
- **Hot Reload**: Zero-downtime configuration reloads via file system watching (`fsnotify`) and `SIGHUP` signals, supporting live port and tailnet additions, removals, and updates without dropping unaffected active connections.
- **TCP & UDP Support**: Unified Layer 4 forwarding for both TCP streams and UDP datagrams (including unidirectional, request-response, and streaming/gaming UDP traffic).
- **Performance Metrics**: Built-in atomic metrics tracking (`TCP_Connections`, `UDP_Packets`, `Bytes_Transferred`, `Errors`).
- **Docker Ready**: Multi-stage Alpine container image running as a non-root user with graceful shutdown and health checks.

## Configuration Path

`proxtail` resolves its configuration file in the following order:
1. `-config` / `-c` command-line flag (`proxtail -c /path/to/config.yaml`)
2. `PROXTAIL_CONFIG` (or `TSNET_PROXY_CONFIG`) environment variable
3. `./config.yaml` in the current working directory (if present)
4. `/etc/proxtail/config.yaml` (default in Docker)

## Hot Reload

`proxtail` supports live reloading of configuration changes via:
1. **Automatic File Watching (`fsnotify`)**: Watches the configuration directory for modifications, creations, or atomic renames of `config.yaml` (with automatic event debouncing).
2. **Manual Signal (`SIGHUP`)**: Send `kill -HUP <pid>` or `docker kill --signal=HUP proxtail` to trigger an immediate reload.

During a reload:
- The new configuration is parsed and validated before applying any changes.
- Newly added tailnets are initialized, modified tailnets (`hostname`, `auth_key`, `control_url`, `ephemeral`) are reinitialized, and removed tailnets are gracefully closed.
- Added, removed, and modified port listeners are updated incrementally while unaffected active connections continue uninterrupted.
- Process-level `proxy` tuning parameters (`buffer_size`, `worker_pool_size`, `metrics_interval`) are initialized at startup and require a process restart to change.

## Configuration

`proxtail` uses convention over configuration so most fields can be omitted:

### Minimal Single-Tailnet Example

```yaml
tailnets:
  - ports:
      - port: 8080
        destination: 'web-server' # Automatically forwards TCP :8080 -> web-server:8080
```
With a single tailnet, set `export TS_AUTHKEY="tskey-auth-xxxxx"` and run `proxtail`.

### Multi-Tailnet & Full Options Example

```yaml
tailnets:
  - name: 'production'
    hostname: 'proxy-prod'             # Optional, defaults to <system-hostname>-<name>
    auth_key: 'TS_AUTHKEY_PRODUCTION'  # Optional, defaults to TS_AUTHKEY_<UPPERCASE_NAME> (or literal tskey-*)
    control_url: ''                    # Optional, custom coordination server URL (e.g. Headscale)
    ephemeral: true                    # Optional, defaults to true
    ports:
      - port: 8080                     # Alias for source_port
        destination: 'web-server'      # Omitting :port reuses source port (web-server:8080)
      - port: 8443
        destination: 'api-server:443'  # Explicit destination port
        timeout: 45                    # Optional, defaults to 30 seconds
      - port: 12345
        destination: 'game-server'
        protocol: 'udp'                # Optional, defaults to 'tcp'

  - name: 'staging'
    ports:
      - port: 9080
        destination: 'staging-web:8080'

proxy:
  buffer_size: 512      # Optional: Buffer size in KB (default: 512, valid: 1-8192)
  worker_pool_size: 200 # Optional: Max concurrent workers (default: 200, valid: 1-10000)
  metrics_interval: 30  # Optional: Log metrics every N seconds (auto-enables metrics; omit to disable)
```

### Environment Variables

```bash
export TS_AUTHKEY="tskey-auth-xxxxx"            # Used automatically in single-tailnet mode
export TS_AUTHKEY_PRODUCTION="tskey-auth-xxxxx" # Per-tailnet auth key (hyphens normalized to '_')
export TS_AUTHKEY_STAGING="tskey-auth-yyyyy"
export TS_STATE_DIR="/var/lib/tsnet"            # Used when ephemeral: false
```

## Building & Testing

```bash
# Build binary
go build -o proxtail ./cmd/proxtail

# Run test suite
go test -v ./...
```

## Docker Deployment

```bash
docker compose -f examples/docker-compose.yaml up -d
```

## License

MIT License - see [LICENSE](LICENSE) for details.
