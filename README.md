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

By default, `proxtail` loads its configuration from `/etc/proxtail/config.yaml`.

To specify a custom configuration file path, set `PROXTAIL_CONFIG` (or `TSNET_PROXY_CONFIG` for backward compatibility):

```bash
docker run -d \
  -e PROXTAIL_CONFIG=/custom/path/config.yaml \
  -p 8080:8080 -p 8081:8081 \
  proxtail
```

## Hot Reload

`proxtail` supports live reloading of configuration changes via:
1. **Automatic File Watching (`fsnotify`)**: Watches the configuration directory for modifications, creations, or atomic renames of `config.yaml` (with automatic event debouncing).
2. **Manual Signal (`SIGHUP`)**: Send `kill -HUP <pid>` or `docker kill --signal=HUP proxtail` to trigger an immediate reload.

During a reload:
- The new configuration is parsed and validated before applying any changes.
- Newly added tailnets are initialized, modified tailnets (`hostname`, `auth_key`, `ephemeral`) are reinitialized, and removed tailnets are gracefully closed.
- Added, removed, and modified port listeners are updated incrementally while unaffected active connections continue uninterrupted.
- Process-level `proxy` tuning parameters (`buffer_size`, `queue_size`, `worker_pool_size`, `enable_metrics`, `metrics_interval`) are initialized at startup and require a process restart to change.

## Configuration

```yaml
tailnets:
  - name: 'production'
    hostname: 'proxy-prod'       # Optional, defaults to <system-hostname>-<name>
    auth_key: 'TS_AUTHKEY_PROD'  # Optional, defaults to TS_AUTHKEY_<UPPERCASE_NAME>
    ephemeral: true              # Optional, defaults to true
    ports:
      - protocol: 'tcp'
        source_port: 8080
        destination: 'web-server:8080'
        timeout: 30              # Optional, defaults to 30 seconds

  - name: 'staging'
    ports:
      - protocol: 'udp'
        source_port: 12345
        destination: 'game-server:12345'

proxy:
  buffer_size: 512      # Buffer size in KB (default: 512, valid: 1-8192)
  queue_size: 100       # Queue size for connections/packets (default: 100, valid: 1-10000)
  worker_pool_size: 200 # Max concurrent workers (default: 2x queue_size, valid: 1-10000)
  enable_metrics: true  # Enable performance metrics logging (default: false)
  metrics_interval: 30  # Metrics interval in seconds (default: 60, valid: 1-3600)
```

### Environment Variables

```bash
export PROXTAIL_CONFIG="/path/to/config.yaml"
export TS_AUTHKEY_PRODUCTION="tskey-auth-xxxxx"
export TS_AUTHKEY_STAGING="tskey-auth-yyyyy"
export TS_STATE_DIR="/var/lib/tsnet" # Used when ephemeral: false
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
