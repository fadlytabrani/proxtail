package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"proxtail/internal/proxy"
)

const (
	// DefaultVIPPortRange is the default reserved local port range for auto-proxied Tailscale VIP services.
	DefaultVIPPortRange = "30000-30999"
	// DefaultVIPRefreshInterval is the default fallback VIP service discovery polling interval in seconds.
	DefaultVIPRefreshInterval = 30
)

// ParsePortRange parses and validates a "start-end" port range specification (inclusive),
// requiring 1 <= start <= end <= 65535.
func ParsePortRange(spec string) (int, int, error) {
	trimmed := strings.TrimSpace(spec)
	startStr, endStr, ok := strings.Cut(trimmed, "-")
	if !ok {
		return 0, 0, fmt.Errorf("invalid vip_port_range '%s': expected 'start-end' format (e.g., '30000-30999')", spec)
	}
	start, err1 := strconv.Atoi(strings.TrimSpace(startStr))
	end, err2 := strconv.Atoi(strings.TrimSpace(endStr))
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("invalid vip_port_range '%s': start and end must be integers", spec)
	}
	if start < 1 || start > 65535 || end < 1 || end > 65535 {
		return 0, 0, fmt.Errorf("invalid vip_port_range '%s': ports must be between 1 and 65535", spec)
	}
	if start > end {
		return 0, 0, fmt.Errorf("invalid vip_port_range '%s': start port (%d) cannot be greater than end port (%d)", spec, start, end)
	}
	return start, end, nil
}

// CanonicalListenAddress normalizes a local bind IP string (unmapping IPv4-in-IPv6
// and compressing IPv6 representations). An empty string is returned as-is.
func CanonicalListenAddress(addr string) (string, error) {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return "", nil
	}
	if net.ParseIP(trimmed) == nil {
		return trimmed, fmt.Errorf("must be a valid IP address")
	}
	parsed, err := netip.ParseAddr(trimmed)
	if err != nil {
		return trimmed, fmt.Errorf("must be a valid IP address")
	}
	return parsed.Unmap().String(), nil
}

// normalizeListenAddressIfValid canonicalizes addr if it is a valid IP address,
// or returns the trimmed input unchanged so Validate can report the exact invalid value.
func normalizeListenAddressIfValid(addr string) string {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return ""
	}
	if canon, err := CanonicalListenAddress(trimmed); err == nil {
		return canon
	}
	return trimmed
}

// ListenAddressesConflict reports whether two local bind addresses overlap on the same
// OS socket namespace for dual-stack "tcp"/"udp" listeners:
//   - Empty ("") or "::" (unspecified IPv6 on dual-stack sockets) conflicts with every address.
//   - "0.0.0.0" (unspecified IPv4) conflicts with "", "::", "0.0.0.0", and any IPv4 address.
//   - Two specific IPs conflict iff their canonical unmapped addresses are equal.
func ListenAddressesConflict(a, b string) bool {
	ca, errA := CanonicalListenAddress(a)
	cb, errB := CanonicalListenAddress(b)
	if errA != nil || errB != nil {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	if ca == cb {
		return true
	}
	if ca == "" || cb == "" || ca == "::" || cb == "::" {
		return true
	}
	if ca == "0.0.0.0" {
		pb, err := netip.ParseAddr(cb)
		return err == nil && pb.Unmap().Is4()
	}
	if cb == "0.0.0.0" {
		pa, err := netip.ParseAddr(ca)
		return err == nil && pa.Unmap().Is4()
	}
	return false
}

// TailnetConfig defines settings for a specific Tailscale network.
type TailnetConfig struct {
	Name          string        `yaml:"name"`                      // Unique identifier for this tailnet (defaults to "default" if single tailnet)
	Hostname      string        `yaml:"hostname,omitempty"`        // Hostname for this tailnet (optional, defaults to "<system-hostname>-<name>")
	AuthKey       string        `yaml:"auth_key,omitempty"`        // Auth key or OAuth secret env var / literal (optional, defaults to "TS_AUTHKEY_<NAME>")
	ControlURL    string        `yaml:"control_url,omitempty"`     // Coordination server URL (optional, defaults to Tailscale control plane)
	ListenAddress string        `yaml:"listen_address,omitempty"`  // Optional default local bind IP for all ports and auto-proxied VIPs in this tailnet
	Ephemeral     *bool         `yaml:"ephemeral,omitempty"`       // Whether this node is ephemeral (optional, defaults to true)
	AutoProxyVIPs *bool         `yaml:"auto_proxy_vips,omitempty"` // Automatically discover and proxy Tailscale VIP services (optional, defaults to true)
	Tags          []string      `yaml:"tags,omitempty"`            // Tailscale ACL tags to advertise (e.g., ["tag:proxy"])
	WireGuardPort uint16        `yaml:"wireguard_port,omitempty"`  // Optional UDP port for direct peer-to-peer WireGuard traffic
	Ports         []PortMapping `yaml:"ports"`                     // Ports to forward for this tailnet
}

// EffectiveListenAddress returns the tailnet's canonical listen_address, falling back to proxyListenAddress.
func (t TailnetConfig) EffectiveListenAddress(proxyListenAddress string) string {
	if addr := normalizeListenAddressIfValid(t.ListenAddress); addr != "" {
		return addr
	}
	return normalizeListenAddressIfValid(proxyListenAddress)
}

// IsEphemeral returns the configured ephemeral value, defaulting to true if unset.
func (t TailnetConfig) IsEphemeral() bool {
	if t.Ephemeral == nil {
		return true
	}
	return *t.Ephemeral
}

// IsAutoProxyVIPs returns whether automatic Tailscale VIP service discovery and proxying
// is enabled for this tailnet, defaulting to true if unset.
func (t TailnetConfig) IsAutoProxyVIPs() bool {
	if t.AutoProxyVIPs == nil {
		return true
	}
	return *t.AutoProxyVIPs
}

// ConnectionSettingsEqual reports whether two TailnetConfig values have identical
// connection-level settings.
func (t TailnetConfig) ConnectionSettingsEqual(other TailnetConfig) bool {
	return t.Name == other.Name &&
		t.Hostname == other.Hostname &&
		t.AuthKey == other.AuthKey &&
		t.ControlURL == other.ControlURL &&
		t.IsEphemeral() == other.IsEphemeral() &&
		t.WireGuardPort == other.WireGuardPort &&
		slices.Equal(t.Tags, other.Tags)
}

// ResolveAuthKey resolves the Tailscale credential for this tailnet.
// Kept for compatibility; see ResolveCredentials for separated authKey vs OAuth clientSecret resolution.
func (t TailnetConfig) ResolveAuthKey(allowGlobalFallback bool) (string, error) {
	authKey, clientSecret, err := t.ResolveCredentials(allowGlobalFallback)
	if err != nil {
		return "", err
	}
	if clientSecret != "" {
		return clientSecret, nil
	}
	return authKey, nil
}

// ResolveCredentials resolves either a Tailscale AuthKey ("tskey-auth-...") or an OAuth ClientSecret ("tskey-client-...")
// from environment variables or literal configuration values:
//  1. The environment variable named by AuthKey (or TS_AUTHKEY_<NAME> if unset),
//  2. The environment variable TS_CLIENT_SECRET_<NAME>,
//  3. A literal "tskey-..." string if AuthKey was provided directly in YAML, or
//  4. Standard TS_AUTHKEY / TS_CLIENT_SECRET environment variables when allowGlobalFallback is true.
func (t TailnetConfig) ResolveCredentials(allowGlobalFallback bool) (authKey string, clientSecret string, err error) {
	classify := func(val string) (string, string) {
		if strings.HasPrefix(val, "tskey-client-") {
			return "", val
		}
		return val, ""
	}

	keySpec := strings.TrimSpace(t.AuthKey)
	if keySpec == "" {
		keySpec = DefaultAuthKeyEnv(t.Name)
	}
	if val := strings.TrimSpace(os.Getenv(keySpec)); val != "" {
		ak, cs := classify(val)
		return ak, cs, nil
	}
	clientSecretEnv := DefaultClientSecretEnv(t.Name)
	if val := strings.TrimSpace(os.Getenv(clientSecretEnv)); val != "" {
		return "", val, nil
	}
	if strings.HasPrefix(keySpec, "tskey-") {
		ak, cs := classify(keySpec)
		return ak, cs, nil
	}
	if allowGlobalFallback {
		if val := strings.TrimSpace(os.Getenv("TS_AUTHKEY")); val != "" {
			ak, cs := classify(val)
			return ak, cs, nil
		}
		if val := strings.TrimSpace(os.Getenv("TS_CLIENT_SECRET")); val != "" {
			return "", val, nil
		}
	}
	return "", "", fmt.Errorf("environment variable %s not set", keySpec)
}

// PortMapping defines a port forwarding rule under a tailnet.
type PortMapping struct {
	Source            int    `yaml:"source_port,omitempty"`    // Local source port to listen on (1-65535)
	Port              int    `yaml:"port,omitempty"`           // Alias for source_port
	ListenAddress     string `yaml:"listen_address,omitempty"` // Optional local bind IP (e.g., "127.0.0.1"; default: all interfaces)
	Destination       string `yaml:"destination"`              // Destination in tailnet ("host:port" or "host" to reuse source_port)
	Tailnet           string `yaml:"-"`                        // Populated automatically from parent TailnetConfig
	Protocol          string `yaml:"protocol,omitempty"`       // Protocol: "tcp" or "udp" (default: "tcp")
	ConnectionTimeout int    `yaml:"timeout,omitempty"`        // Connection timeout in seconds (default: 30)
	Probe             *bool  `yaml:"probe,omitempty"`          // Optional per-port override for active upstream reachability probing
}

// ProbeSettings defines active upstream reachability probing and listener suspension options.
type ProbeSettings struct {
	Enabled             bool   `yaml:"enabled,omitempty"`              // Enable active upstream reachability probing and listener suspension (default: false)
	Interval            int    `yaml:"interval,omitempty"`             // Background probe interval in seconds (default: 10)
	Timeout             int    `yaml:"timeout,omitempty"`              // Probe dial/read timeout in seconds (default: 2)
	ConsecutiveFailures int    `yaml:"consecutive_failures,omitempty"` // Consecutive failures before suspending listener (default: 2)
	Cooldown            int    `yaml:"cooldown,omitempty"`             // Traffic-aware activity cooldown in seconds (default: 10)
	UDPProbeMode        string `yaml:"udp_probe_mode,omitempty"`       // UDP probe strategy: "payload" (default) or "netmap"
}

// ProxySettings defines performance, binding, health check, and monitoring settings for the proxy server.
type ProxySettings struct {
	ListenAddress       string        `yaml:"listen_address,omitempty"`        // Default local bind IP for all ports (default: all interfaces)
	HealthListenAddress string        `yaml:"health_listen_address,omitempty"` // Optional local bind IP for /healthz and /metrics (defaults to listen_address)
	BufferSize          int           `yaml:"buffer_size,omitempty"`           // Buffer size in KB (default: 512)
	WorkerPoolSize      int           `yaml:"worker_pool_size,omitempty"`      // Max concurrent workers (default: 200)
	EnableMetrics       bool          `yaml:"enable_metrics,omitempty"`        // Enable performance metrics logging (auto-enabled if metrics_interval > 0)
	MetricsInterval     int           `yaml:"metrics_interval,omitempty"`      // Metrics logging interval in seconds (default: 60 when metrics enabled)
	HealthPort          int           `yaml:"health_port,omitempty"`           // Optional HTTP port for /healthz and Prometheus /metrics endpoints
	VIPPortRange        string        `yaml:"vip_port_range,omitempty"`        // Reserved local port range for auto-proxied VIP services (default: "30000-30999")
	VIPRefreshInterval  int           `yaml:"vip_refresh_interval,omitempty"`  // Fallback VIP discovery refresh interval in seconds (default: 30)
	Probe               ProbeSettings `yaml:"probe,omitempty"`                 // Active upstream reachability probing and listener suspension
}

// EffectiveHealthListenAddress returns the canonical health server bind IP, falling back to ListenAddress.
func (p ProxySettings) EffectiveHealthListenAddress() string {
	if addr := normalizeListenAddressIfValid(p.HealthListenAddress); addr != "" {
		return addr
	}
	return normalizeListenAddressIfValid(p.ListenAddress)
}

// Config defines the structure for the YAML configuration file.
type Config struct {
	Tailnets []TailnetConfig `yaml:"tailnets"`
	Proxy    ProxySettings   `yaml:"proxy"`
}

func sanitizeEnvSuffix(tailnetName string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(tailnetName)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// DefaultAuthKeyEnv returns the canonical environment variable name for a tailnet's auth key,
// normalizing hyphens and non-alphanumeric characters to underscores (e.g., "prod-us" -> "TS_AUTHKEY_PROD_US").
func DefaultAuthKeyEnv(tailnetName string) string {
	return "TS_AUTHKEY_" + sanitizeEnvSuffix(tailnetName)
}

// DefaultClientSecretEnv returns the canonical environment variable name for a tailnet's OAuth client secret
// (e.g., "prod-us" -> "TS_CLIENT_SECRET_PROD_US").
func DefaultClientSecretEnv(tailnetName string) string {
	return "TS_CLIENT_SECRET_" + sanitizeEnvSuffix(tailnetName)
}

// Load reads, parses, applies defaults, and validates the YAML configuration file.
func Load(file string) (*Config, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", file, err)
	}
	return Parse(data)
}

// Parse unmarshals YAML data, expands environment variables, applies defaults, and validates the configuration.
func Parse(data []byte) (*Config, error) {
	expanded := os.ExpandEnv(string(data))
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config YAML: %w", err)
	}

	ApplyDefaults(&cfg)

	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return &cfg, nil
}

// ApplyDefaults populates default values for omitted fields in Config.
func ApplyDefaults(cfg *Config) {
	if cfg.Proxy.BufferSize <= 0 {
		cfg.Proxy.BufferSize = 512 // Default: 512 KB
	}
	if cfg.Proxy.WorkerPoolSize <= 0 {
		cfg.Proxy.WorkerPoolSize = 200 // Default: 200 concurrent workers
	}
	if cfg.Proxy.MetricsInterval > 0 {
		cfg.Proxy.EnableMetrics = true
	} else {
		cfg.Proxy.MetricsInterval = 60 // Default: 60 seconds
	}
	cfg.Proxy.ListenAddress = strings.TrimSpace(cfg.Proxy.ListenAddress)
	if cfg.Proxy.ListenAddress == "" {
		cfg.Proxy.ListenAddress = strings.TrimSpace(os.Getenv("PROXTAIL_LISTEN_ADDRESS"))
	}
	cfg.Proxy.ListenAddress = normalizeListenAddressIfValid(cfg.Proxy.ListenAddress)

	cfg.Proxy.HealthListenAddress = strings.TrimSpace(cfg.Proxy.HealthListenAddress)
	if cfg.Proxy.HealthListenAddress == "" {
		cfg.Proxy.HealthListenAddress = strings.TrimSpace(os.Getenv("PROXTAIL_HEALTH_LISTEN_ADDRESS"))
	}
	if cfg.Proxy.HealthListenAddress == "" && cfg.Proxy.ListenAddress != "" {
		cfg.Proxy.HealthListenAddress = cfg.Proxy.ListenAddress
	}
	cfg.Proxy.HealthListenAddress = normalizeListenAddressIfValid(cfg.Proxy.HealthListenAddress)

	if cfg.Proxy.HealthPort == 0 {
		if hpStr := strings.TrimSpace(os.Getenv("PROXTAIL_HEALTH_PORT")); hpStr != "" {
			if hp, err := strconv.Atoi(hpStr); err == nil && hp > 0 {
				cfg.Proxy.HealthPort = hp
			} else {
				cfg.Proxy.HealthPort = -1 // Trigger validation error in Validate
			}
		}
	}

	cfg.Proxy.VIPPortRange = strings.TrimSpace(cfg.Proxy.VIPPortRange)
	if cfg.Proxy.VIPPortRange == "" {
		cfg.Proxy.VIPPortRange = strings.TrimSpace(os.Getenv("PROXTAIL_VIP_PORT_RANGE"))
	}
	if cfg.Proxy.VIPPortRange == "" {
		cfg.Proxy.VIPPortRange = DefaultVIPPortRange
	}
	if cfg.Proxy.VIPRefreshInterval == 0 {
		if vrStr := strings.TrimSpace(os.Getenv("PROXTAIL_VIP_REFRESH_INTERVAL")); vrStr != "" {
			if vr, err := strconv.Atoi(vrStr); err == nil && vr > 0 {
				cfg.Proxy.VIPRefreshInterval = vr
			} else {
				cfg.Proxy.VIPRefreshInterval = -1 // Trigger validation error in Validate
			}
		}
	}
	if cfg.Proxy.VIPRefreshInterval == 0 {
		cfg.Proxy.VIPRefreshInterval = DefaultVIPRefreshInterval
	}

	if val := strings.TrimSpace(os.Getenv("PROXTAIL_PROBE_ENABLED")); val != "" {
		if b, err := strconv.ParseBool(val); err == nil {
			cfg.Proxy.Probe.Enabled = b
		}
	}
	if cfg.Proxy.Probe.Interval == 0 {
		cfg.Proxy.Probe.Interval = 10
	}
	if cfg.Proxy.Probe.Timeout == 0 {
		cfg.Proxy.Probe.Timeout = 2
	}
	if cfg.Proxy.Probe.ConsecutiveFailures == 0 {
		cfg.Proxy.Probe.ConsecutiveFailures = 2
	}
	if cfg.Proxy.Probe.Cooldown == 0 {
		if cfg.Proxy.Probe.Interval > 0 {
			cfg.Proxy.Probe.Cooldown = cfg.Proxy.Probe.Interval
		} else {
			cfg.Proxy.Probe.Cooldown = 10
		}
	}
	cfg.Proxy.Probe.UDPProbeMode = strings.ToLower(strings.TrimSpace(cfg.Proxy.Probe.UDPProbeMode))
	if cfg.Proxy.Probe.UDPProbeMode == "" {
		cfg.Proxy.Probe.UDPProbeMode = strings.ToLower(strings.TrimSpace(os.Getenv("PROXTAIL_PROBE_UDP_MODE")))
	}
	if cfg.Proxy.Probe.UDPProbeMode == "" {
		cfg.Proxy.Probe.UDPProbeMode = "payload"
	}

	sysHostname, err := os.Hostname()
	if err != nil || sysHostname == "" {
		sysHostname = "proxtail"
	}

	// If only a single tailnet is configured and its name is omitted, default to "default".
	if len(cfg.Tailnets) == 1 && strings.TrimSpace(cfg.Tailnets[0].Name) == "" {
		cfg.Tailnets[0].Name = "default"
	}

	for i := range cfg.Tailnets {
		if cfg.Tailnets[i].Hostname == "" && cfg.Tailnets[i].Name != "" {
			cfg.Tailnets[i].Hostname = fmt.Sprintf("%s-%s", sysHostname, cfg.Tailnets[i].Name)
		}
		cfg.Tailnets[i].ListenAddress = strings.TrimSpace(cfg.Tailnets[i].ListenAddress)
		if cfg.Tailnets[i].ListenAddress == "" && cfg.Proxy.ListenAddress != "" {
			cfg.Tailnets[i].ListenAddress = cfg.Proxy.ListenAddress
		}
		cfg.Tailnets[i].ListenAddress = normalizeListenAddressIfValid(cfg.Tailnets[i].ListenAddress)
		if cfg.Tailnets[i].Ephemeral == nil {
			defaultEphemeral := true
			cfg.Tailnets[i].Ephemeral = &defaultEphemeral
		}
		if cfg.Tailnets[i].AutoProxyVIPs == nil {
			defaultAutoVIPs := true
			cfg.Tailnets[i].AutoProxyVIPs = &defaultAutoVIPs
		}
		if cfg.Tailnets[i].AuthKey == "" && cfg.Tailnets[i].Name != "" {
			defaultEnv := DefaultAuthKeyEnv(cfg.Tailnets[i].Name)
			if len(cfg.Tailnets) == 1 && os.Getenv(defaultEnv) == "" && os.Getenv("TS_AUTHKEY") != "" {
				cfg.Tailnets[i].AuthKey = "TS_AUTHKEY"
			} else {
				cfg.Tailnets[i].AuthKey = defaultEnv
			}
		}
		for tIdx, tag := range cfg.Tailnets[i].Tags {
			trimmed := strings.TrimSpace(tag)
			if trimmed != "" && !strings.HasPrefix(trimmed, "tag:") {
				cfg.Tailnets[i].Tags[tIdx] = "tag:" + trimmed
			} else {
				cfg.Tailnets[i].Tags[tIdx] = trimmed
			}
		}
		for j := range cfg.Tailnets[i].Ports {
			p := &cfg.Tailnets[i].Ports[j]
			p.Tailnet = cfg.Tailnets[i].Name
			if p.Source == 0 && p.Port != 0 {
				p.Source = p.Port
			}
			p.ListenAddress = strings.TrimSpace(p.ListenAddress)
			if p.ListenAddress == "" && cfg.Tailnets[i].ListenAddress != "" {
				p.ListenAddress = cfg.Tailnets[i].ListenAddress
			} else if p.ListenAddress == "" && cfg.Proxy.ListenAddress != "" {
				p.ListenAddress = cfg.Proxy.ListenAddress
			}
			p.ListenAddress = normalizeListenAddressIfValid(p.ListenAddress)
			if strings.TrimSpace(p.Protocol) == "" {
				p.Protocol = "tcp"
			} else {
				p.Protocol = strings.ToLower(strings.TrimSpace(p.Protocol))
			}
			dest := strings.TrimSpace(p.Destination)
			if dest != "" && p.Source > 0 && p.Source <= 65535 {
				// If destination is a hostname/IP without a ":port" suffix (or bracketed IPv6 without port),
				// default the destination port to the source port.
				if !strings.Contains(dest, ":") || (strings.HasPrefix(dest, "[") && strings.HasSuffix(dest, "]")) {
					p.Destination = fmt.Sprintf("%s:%d", dest, p.Source)
				} else {
					p.Destination = dest
				}
			}
			if p.ConnectionTimeout <= 0 {
				p.ConnectionTimeout = 30 // Default: 30 seconds
			}
		}
	}
}

// Validate performs comprehensive validation of the configuration.
// Zero tailnets (len(cfg.Tailnets) == 0) is permitted for standby/unconfigured deployments.
func Validate(cfg *Config) error {
	if _, err := CanonicalListenAddress(cfg.Proxy.ListenAddress); err != nil {
		return fmt.Errorf("invalid proxy listen_address '%s': must be a valid IP address", cfg.Proxy.ListenAddress)
	}
	if _, err := CanonicalListenAddress(cfg.Proxy.HealthListenAddress); err != nil {
		return fmt.Errorf("invalid proxy health_listen_address '%s': must be a valid IP address", cfg.Proxy.HealthListenAddress)
	}

	if cfg.Proxy.HealthPort < 0 || cfg.Proxy.HealthPort > 65535 {
		return fmt.Errorf("invalid health_port %d: must be between 1 and 65535", cfg.Proxy.HealthPort)
	}

	vipRangeSpec := strings.TrimSpace(cfg.Proxy.VIPPortRange)
	if vipRangeSpec == "" {
		vipRangeSpec = DefaultVIPPortRange
	}
	vipStart, vipEnd, err := ParsePortRange(vipRangeSpec)
	if err != nil {
		return err
	}
	if cfg.Proxy.VIPRefreshInterval != 0 && (cfg.Proxy.VIPRefreshInterval < 1 || cfg.Proxy.VIPRefreshInterval > 3600) {
		return fmt.Errorf("invalid vip_refresh_interval %d: must be between 1 and 3600 seconds", cfg.Proxy.VIPRefreshInterval)
	}

	tailnetNames := make(map[string]bool)
	var autoVIPListenAddrs []string
	for _, tailnet := range cfg.Tailnets {
		if strings.TrimSpace(tailnet.Name) == "" {
			return fmt.Errorf("tailnet name cannot be empty")
		}
		if tailnetNames[tailnet.Name] {
			return fmt.Errorf("duplicate tailnet name: %s", tailnet.Name)
		}
		tailnetNames[tailnet.Name] = true

		if _, err := CanonicalListenAddress(tailnet.ListenAddress); err != nil {
			return fmt.Errorf("invalid listen_address '%s' in tailnet %s: must be a valid IP address", tailnet.ListenAddress, tailnet.Name)
		}

		if tailnet.IsAutoProxyVIPs() {
			autoVIPListenAddrs = append(autoVIPListenAddrs, tailnet.EffectiveListenAddress(cfg.Proxy.ListenAddress))
		}

		for _, tag := range tailnet.Tags {
			if tag == "" || tag == "tag:" || strings.ContainsAny(tag, " \t\n") {
				return fmt.Errorf("invalid ACL tag '%s' in tailnet %s", tag, tailnet.Name)
			}
		}
	}

	overlapsAutoVIPListenAddr := func(addr string) bool {
		for _, vipAddr := range autoVIPListenAddrs {
			if ListenAddressesConflict(addr, vipAddr) {
				return true
			}
		}
		return false
	}

	if cfg.Proxy.HealthPort >= vipStart && cfg.Proxy.HealthPort <= vipEnd && overlapsAutoVIPListenAddr(cfg.Proxy.EffectiveHealthListenAddress()) {
		return fmt.Errorf("port conflict: health_port %d falls within reserved vip_port_range %d-%d",
			cfg.Proxy.HealthPort, vipStart, vipEnd)
	}

	type boundPortEntry struct {
		tailnet    string
		listenAddr string
	}
	portBindings := make(map[string][]boundPortEntry) // "<port>-<protocol>" -> entries
	totalPorts := 0

	for _, tailnet := range cfg.Tailnets {
		tailnetListenAddr := tailnet.EffectiveListenAddress(cfg.Proxy.ListenAddress)
		for _, port := range tailnet.Ports {
			totalPorts++
			if port.Port != 0 && port.Source != port.Port {
				return fmt.Errorf("conflicting source_port (%d) and port (%d) in tailnet %s", port.Source, port.Port, tailnet.Name)
			}
			if port.Source <= 0 || port.Source > 65535 {
				return fmt.Errorf("invalid source port %d in tailnet %s: must be between 1 and 65535", port.Source, tailnet.Name)
			}
			portListenAddr, err := CanonicalListenAddress(port.ListenAddress)
			if err != nil {
				return fmt.Errorf("invalid listen_address '%s' in tailnet %s, port %d: must be a valid IP address",
					port.ListenAddress, tailnet.Name, port.Source)
			}
			if portListenAddr == "" {
				portListenAddr = tailnetListenAddr
			}
			if port.Source >= vipStart && port.Source <= vipEnd && overlapsAutoVIPListenAddr(portListenAddr) {
				return fmt.Errorf("port conflict: manual source port %d in tailnet '%s' falls within reserved vip_port_range %d-%d",
					port.Source, tailnet.Name, vipStart, vipEnd)
			}
			dest := strings.TrimSpace(port.Destination)
			if dest == "" {
				return fmt.Errorf("empty destination in tailnet %s, port %d", tailnet.Name, port.Source)
			}
			destHost, destPortStr, err := net.SplitHostPort(dest)
			if err != nil || strings.TrimSpace(destHost) == "" {
				return fmt.Errorf("invalid destination '%s' in tailnet %s, port %d: must be in host:port or host format", port.Destination, tailnet.Name, port.Source)
			}
			destPort, err := strconv.Atoi(destPortStr)
			if err != nil || destPort <= 0 || destPort > 65535 {
				return fmt.Errorf("invalid destination port '%s' in tailnet %s, port %d: must be between 1 and 65535", destPortStr, tailnet.Name, port.Source)
			}
			if strings.TrimSpace(port.Protocol) == "" {
				return fmt.Errorf("empty protocol in tailnet %s, port %d", tailnet.Name, port.Source)
			}

			protocol := strings.ToLower(port.Protocol)
			if protocol != "tcp" && protocol != "udp" {
				return fmt.Errorf("invalid protocol '%s' in tailnet %s, port %d: must be 'tcp' or 'udp'", port.Protocol, tailnet.Name, port.Source)
			}

			if cfg.Proxy.HealthPort > 0 && protocol == "tcp" && port.Source == cfg.Proxy.HealthPort {
				healthBind := cfg.Proxy.EffectiveHealthListenAddress()
				if ListenAddressesConflict(portListenAddr, healthBind) {
					return fmt.Errorf("port conflict: health_port %d conflicts with TCP source port %d in tailnet '%s'",
						cfg.Proxy.HealthPort, port.Source, tailnet.Name)
				}
			}

			portProtoKey := fmt.Sprintf("%d-%s", port.Source, protocol)
			for _, existing := range portBindings[portProtoKey] {
				if ListenAddressesConflict(existing.listenAddr, portListenAddr) {
					if existing.tailnet == tailnet.Name {
						return fmt.Errorf("port conflict: duplicate port %d (%s) in tailnet '%s'",
							port.Source, protocol, tailnet.Name)
					}
					return fmt.Errorf("port conflict: port %d (%s) is used by both tailnet '%s' and tailnet '%s'",
						port.Source, protocol, existing.tailnet, tailnet.Name)
				}
			}
			portBindings[portProtoKey] = append(portBindings[portProtoKey], boundPortEntry{
				tailnet:    tailnet.Name,
				listenAddr: portListenAddr,
			})

			if port.ConnectionTimeout < 1 || port.ConnectionTimeout > 3600 {
				return fmt.Errorf("invalid timeout %d in tailnet %s, port %d: must be between 1 and 3600 seconds",
					port.ConnectionTimeout, tailnet.Name, port.Source)
			}
		}
	}

	if cfg.Proxy.BufferSize < 1 || cfg.Proxy.BufferSize > 8192 {
		return fmt.Errorf("invalid buffer_size %d: must be between 1 and 8192 KB", cfg.Proxy.BufferSize)
	}
	if cfg.Proxy.WorkerPoolSize < 1 || cfg.Proxy.WorkerPoolSize > 10000 {
		return fmt.Errorf("invalid worker_pool_size %d: must be between 1 and 10000", cfg.Proxy.WorkerPoolSize)
	}
	if cfg.Proxy.MetricsInterval < 1 || cfg.Proxy.MetricsInterval > 3600 {
		return fmt.Errorf("invalid metrics_interval %d: must be between 1 and 3600 seconds", cfg.Proxy.MetricsInterval)
	}

	if val := strings.TrimSpace(os.Getenv("PROXTAIL_PROBE_ENABLED")); val != "" {
		if _, err := strconv.ParseBool(val); err != nil {
			return fmt.Errorf("invalid PROXTAIL_PROBE_ENABLED '%s': must be a boolean", val)
		}
	}

	udpMode := strings.ToLower(strings.TrimSpace(cfg.Proxy.Probe.UDPProbeMode))
	if udpMode != "" && udpMode != "payload" && udpMode != "netmap" {
		return fmt.Errorf("invalid probe.udp_probe_mode '%s': must be 'payload' or 'netmap'", cfg.Proxy.Probe.UDPProbeMode)
	}

	probeEnabled := cfg.Proxy.Probe.Enabled
	if !probeEnabled {
		for _, tailnet := range cfg.Tailnets {
			for _, port := range tailnet.Ports {
				if port.Probe != nil && *port.Probe {
					probeEnabled = true
					break
				}
			}
			if probeEnabled {
				break
			}
		}
	}

	if (probeEnabled || cfg.Proxy.Probe.Interval != 0) && (cfg.Proxy.Probe.Interval < 1 || cfg.Proxy.Probe.Interval > 3600) {
		return fmt.Errorf("invalid probe.interval %d: must be between 1 and 3600 seconds", cfg.Proxy.Probe.Interval)
	}
	if (probeEnabled || cfg.Proxy.Probe.Timeout != 0) && (cfg.Proxy.Probe.Timeout < 1 || cfg.Proxy.Probe.Timeout > 60) {
		return fmt.Errorf("invalid probe.timeout %d: must be between 1 and 60 seconds", cfg.Proxy.Probe.Timeout)
	}
	if (probeEnabled || cfg.Proxy.Probe.ConsecutiveFailures != 0) && (cfg.Proxy.Probe.ConsecutiveFailures < 1 || cfg.Proxy.Probe.ConsecutiveFailures > 10) {
		return fmt.Errorf("invalid probe.consecutive_failures %d: must be between 1 and 10", cfg.Proxy.Probe.ConsecutiveFailures)
	}
	if (probeEnabled || cfg.Proxy.Probe.Cooldown != 0) && (cfg.Proxy.Probe.Cooldown < 1 || cfg.Proxy.Probe.Cooldown > 3600) {
		return fmt.Errorf("invalid probe.cooldown %d: must be between 1 and 3600 seconds", cfg.Proxy.Probe.Cooldown)
	}

	return nil
}

// ToProxyPortMappings flattens all nested tailnet port configurations into a slice of proxy.PortMapping.
func (c *Config) ToProxyPortMappings() []proxy.PortMapping {
	var mappings []proxy.PortMapping
	for _, tailnet := range c.Tailnets {
		tailnetListenAddr := tailnet.EffectiveListenAddress(c.Proxy.ListenAddress)
		for _, port := range tailnet.Ports {
			timeout := port.ConnectionTimeout
			if timeout <= 0 {
				timeout = 30
			}
			protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
			if protocol == "" {
				protocol = "tcp"
			}
			listenAddr := normalizeListenAddressIfValid(port.ListenAddress)
			if listenAddr == "" {
				listenAddr = tailnetListenAddr
			}
			mappings = append(mappings, proxy.PortMapping{
				Source:            port.Source,
				ListenAddress:     listenAddr,
				Destination:       port.Destination,
				Protocol:          protocol,
				Tailnet:           tailnet.Name,
				ConnectionTimeout: timeout,
				Probe:             port.Probe,
			})
		}
	}
	return mappings
}

// ToProxyProbeConfig converts Config.Proxy.Probe into a proxy.ProbeConfig.
func (c *Config) ToProxyProbeConfig() proxy.ProbeConfig {
	interval := c.Proxy.Probe.Interval
	if interval <= 0 {
		interval = 10
	}
	timeout := c.Proxy.Probe.Timeout
	if timeout <= 0 {
		timeout = 2
	}
	consecutiveFailures := c.Proxy.Probe.ConsecutiveFailures
	if consecutiveFailures <= 0 {
		consecutiveFailures = 2
	}
	cooldown := c.Proxy.Probe.Cooldown
	if cooldown <= 0 {
		cooldown = interval
	}
	udpMode := strings.ToLower(strings.TrimSpace(c.Proxy.Probe.UDPProbeMode))
	if udpMode == "" {
		udpMode = "payload"
	}
	return proxy.ProbeConfig{
		Enabled:             c.Proxy.Probe.Enabled,
		Interval:            time.Duration(interval) * time.Second,
		Timeout:             time.Duration(timeout) * time.Second,
		ConsecutiveFailures: consecutiveFailures,
		Cooldown:            time.Duration(cooldown) * time.Second,
		UDPProbeMode:        udpMode,
	}
}
