package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"proxtail/internal/proxy"
)

const (
	// DefaultListenAddress is the default local bind IP for tailnet profiles and the HTTP API server.
	DefaultListenAddress = "0.0.0.0"
	// DefaultVIPPortRange is the default reserved local port range for auto-proxied Tailscale VIP services.
	DefaultVIPPortRange = "30000-30999"
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
	Profile       string        `yaml:"profile"`                   // Unique local profile identifier for this tailnet (defaults to "default" if single tailnet)
	Hostname      string        `yaml:"hostname,omitempty"`        // Hostname for this tailnet (optional, defaults to "<system-hostname>-<profile>")
	AuthKey       string        `yaml:"auth_key,omitempty"`        // Auth key or OAuth secret env var / literal (optional, defaults to "TS_AUTHKEY_<PROFILE>")
	ControlURL    string        `yaml:"control_url,omitempty"`     // Coordination server URL (optional, defaults to Tailscale control plane)
	ListenAddress string        `yaml:"listen_address,omitempty"`  // Local bind IP for all ports and auto-proxied VIPs in this tailnet (optional, defaults to "0.0.0.0")
	StateDir      string        `yaml:"-"`                         // Populated automatically from ProxySettings.StateDir
	Ephemeral     *bool         `yaml:"ephemeral,omitempty"`       // Whether this node is ephemeral (optional, defaults to true)
	AutoProxyVIPs *bool         `yaml:"auto_proxy_vips,omitempty"` // Automatically discover and proxy Tailscale VIP services (optional, defaults to true)
	Tags          []string      `yaml:"tags,omitempty"`            // Tailscale ACL tags to advertise (e.g., ["tag:proxy"])
	WireGuardPort uint16        `yaml:"wireguard_port,omitempty"`  // Optional UDP port for direct peer-to-peer WireGuard traffic
	Ports         []PortMapping `yaml:"ports"`                     // Ports to forward for this tailnet
}

// EffectiveListenAddress returns the tailnet's canonical listen_address, defaulting to DefaultListenAddress ("0.0.0.0").
func (t TailnetConfig) EffectiveListenAddress() string {
	if addr := normalizeListenAddressIfValid(t.ListenAddress); addr != "" {
		return addr
	}
	return DefaultListenAddress
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
	return t.Profile == other.Profile &&
		t.Hostname == other.Hostname &&
		t.AuthKey == other.AuthKey &&
		t.ControlURL == other.ControlURL &&
		t.StateDir == other.StateDir &&
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
//  1. A custom environment variable named by AuthKey (when AuthKey is set and is not a literal "tskey-..." key),
//  2. The profile environment variables TS_AUTHKEY_<PROFILE> and TS_CLIENT_SECRET_<PROFILE> (which also override literal YAML keys),
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
	isLiteral := strings.HasPrefix(keySpec, "tskey-")

	if keySpec != "" && !isLiteral {
		if val := strings.TrimSpace(os.Getenv(keySpec)); val != "" {
			ak, cs := classify(val)
			return ak, cs, nil
		}
	}

	if strings.TrimSpace(t.Profile) != "" {
		if val := strings.TrimSpace(os.Getenv(DefaultAuthKeyEnv(t.Profile))); val != "" {
			ak, cs := classify(val)
			return ak, cs, nil
		}
		if val := strings.TrimSpace(os.Getenv(DefaultClientSecretEnv(t.Profile))); val != "" {
			ak, cs := classify(val)
			return ak, cs, nil
		}
	}

	if isLiteral {
		ak, cs := classify(keySpec)
		return ak, cs, nil
	}

	if allowGlobalFallback {
		if val := strings.TrimSpace(os.Getenv("TS_AUTHKEY")); val != "" {
			ak, cs := classify(val)
			return ak, cs, nil
		}
		if val := strings.TrimSpace(os.Getenv("TS_CLIENT_SECRET")); val != "" {
			ak, cs := classify(val)
			return ak, cs, nil
		}
	}

	expectedEnv := keySpec
	if expectedEnv == "" {
		expectedEnv = DefaultAuthKeyEnv(t.Profile)
	}
	return "", "", fmt.Errorf("environment variable %s not set", expectedEnv)
}

// PortMapping defines a port forwarding rule under a tailnet.
type PortMapping struct {
	Port              int    `yaml:"port"`                     // Local port to listen on (1-65535)
	ListenAddress     string `yaml:"listen_address,omitempty"` // Optional local bind IP (e.g., "127.0.0.1"; default: inherits from tailnet profile)
	Destination       string `yaml:"destination"`              // Destination in tailnet ("host:port" or "host" to reuse port)
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

// ProxySettings defines performance, HTTP API, state storage, VIP discovery, and probing settings for the proxy server.
type ProxySettings struct {
	APIListenAddress string        `yaml:"api_listen_address,omitempty"` // Optional local bind IP for /healthz and /metrics (default: "0.0.0.0")
	APIPort          int           `yaml:"api_port,omitempty"`           // Optional HTTP port for /healthz and Prometheus /metrics endpoints
	StateDir         string        `yaml:"state_dir,omitempty"`          // Optional base directory for Tailscale node state storage; each tailnet uses "<state_dir>/<profile>"
	BufferSize       int           `yaml:"buffer_size,omitempty"`        // Buffer size in KB (default: 512)
	WorkerPoolSize   int           `yaml:"worker_pool_size,omitempty"`   // Max concurrent workers (default: 200)
	VIPPortRange     string        `yaml:"vip_port_range,omitempty"`     // Reserved local port range for auto-proxied VIP services (default: "30000-30999")
	Probe            ProbeSettings `yaml:"probe,omitempty"`              // Active upstream reachability probing and listener suspension
}

// EffectiveAPIListenAddress returns the canonical HTTP API server bind IP, defaulting to DefaultListenAddress ("0.0.0.0").
func (p ProxySettings) EffectiveAPIListenAddress() string {
	if addr := normalizeListenAddressIfValid(p.APIListenAddress); addr != "" {
		return addr
	}
	return DefaultListenAddress
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

// isValidIdentifier reports whether s is a non-empty identifier containing only
// safe ASCII characters ([a-zA-Z0-9._-]) and no ".." path traversal segments.
func isValidIdentifier(s string) bool {
	if s == "" || s == "." || s == ".." || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// ApplyDefaults populates default values for omitted fields in Config and applies PROXTAIL_* environment variable overrides.
func ApplyDefaults(cfg *Config) {
	if cfg.Proxy.BufferSize == 0 {
		cfg.Proxy.BufferSize = 512 // Default: 512 KB
	}
	if cfg.Proxy.WorkerPoolSize == 0 {
		cfg.Proxy.WorkerPoolSize = 200 // Default: 200 concurrent workers
	}
	if envAddr := strings.TrimSpace(os.Getenv("PROXTAIL_API_LISTEN_ADDRESS")); envAddr != "" {
		cfg.Proxy.APIListenAddress = envAddr
	}
	cfg.Proxy.APIListenAddress = strings.TrimSpace(cfg.Proxy.APIListenAddress)
	if cfg.Proxy.APIListenAddress == "" {
		cfg.Proxy.APIListenAddress = DefaultListenAddress
	}
	cfg.Proxy.APIListenAddress = normalizeListenAddressIfValid(cfg.Proxy.APIListenAddress)

	if apStr := strings.TrimSpace(os.Getenv("PROXTAIL_API_PORT")); apStr != "" {
		if ap, err := strconv.Atoi(apStr); err == nil {
			cfg.Proxy.APIPort = ap
		} else {
			cfg.Proxy.APIPort = -1 // Trigger validation error in Validate
		}
	}

	if envStateDir := strings.TrimSpace(os.Getenv("PROXTAIL_STATE_DIR")); envStateDir != "" {
		cfg.Proxy.StateDir = envStateDir
	} else if tsStateDir := strings.TrimSpace(os.Getenv("TS_STATE_DIR")); tsStateDir != "" {
		cfg.Proxy.StateDir = tsStateDir
	}
	cfg.Proxy.StateDir = strings.TrimSpace(cfg.Proxy.StateDir)

	if envRange := strings.TrimSpace(os.Getenv("PROXTAIL_VIP_PORT_RANGE")); envRange != "" {
		cfg.Proxy.VIPPortRange = envRange
	}
	cfg.Proxy.VIPPortRange = strings.TrimSpace(cfg.Proxy.VIPPortRange)
	if cfg.Proxy.VIPPortRange == "" {
		cfg.Proxy.VIPPortRange = DefaultVIPPortRange
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
	if envMode := strings.ToLower(strings.TrimSpace(os.Getenv("PROXTAIL_PROBE_UDP_MODE"))); envMode != "" {
		cfg.Proxy.Probe.UDPProbeMode = envMode
	}
	cfg.Proxy.Probe.UDPProbeMode = strings.ToLower(strings.TrimSpace(cfg.Proxy.Probe.UDPProbeMode))
	if cfg.Proxy.Probe.UDPProbeMode == "" {
		cfg.Proxy.Probe.UDPProbeMode = "payload"
	}

	sysHostname, err := os.Hostname()
	if err != nil || sysHostname == "" {
		sysHostname = "proxtail"
	}

	// If only a single tailnet is configured and its profile is omitted, default to "default".
	if len(cfg.Tailnets) == 1 && strings.TrimSpace(cfg.Tailnets[0].Profile) == "" {
		cfg.Tailnets[0].Profile = "default"
	}

	for i := range cfg.Tailnets {
		cfg.Tailnets[i].Profile = strings.TrimSpace(cfg.Tailnets[i].Profile)
		cfg.Tailnets[i].Hostname = strings.TrimSpace(cfg.Tailnets[i].Hostname)
		cfg.Tailnets[i].ControlURL = strings.TrimSpace(cfg.Tailnets[i].ControlURL)
		cfg.Tailnets[i].StateDir = cfg.Proxy.StateDir
		if cfg.Tailnets[i].Hostname == "" && cfg.Tailnets[i].Profile != "" {
			cfg.Tailnets[i].Hostname = fmt.Sprintf("%s-%s", sysHostname, cfg.Tailnets[i].Profile)
		}
		cfg.Tailnets[i].ListenAddress = strings.TrimSpace(cfg.Tailnets[i].ListenAddress)
		if cfg.Tailnets[i].ListenAddress == "" {
			cfg.Tailnets[i].ListenAddress = DefaultListenAddress
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
		if cfg.Tailnets[i].AuthKey == "" && cfg.Tailnets[i].Profile != "" {
			defaultEnv := DefaultAuthKeyEnv(cfg.Tailnets[i].Profile)
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
			p.Tailnet = cfg.Tailnets[i].Profile
			p.ListenAddress = strings.TrimSpace(p.ListenAddress)
			if p.ListenAddress == "" {
				p.ListenAddress = cfg.Tailnets[i].ListenAddress
			}
			p.ListenAddress = normalizeListenAddressIfValid(p.ListenAddress)
			if strings.TrimSpace(p.Protocol) == "" {
				p.Protocol = "tcp"
			} else {
				p.Protocol = strings.ToLower(strings.TrimSpace(p.Protocol))
			}
			dest := strings.TrimSpace(p.Destination)
			if dest != "" && p.Port > 0 && p.Port <= 65535 {
				// If destination is a bare IPv6 literal without brackets or port (e.g., "fd7a:115c:a1e0::1"),
				// bracket it and default the destination port to the local port.
				if addr, err := netip.ParseAddr(dest); err == nil && addr.Is6() {
					p.Destination = fmt.Sprintf("[%s]:%d", dest, p.Port)
				} else if !strings.Contains(dest, ":") || (strings.HasPrefix(dest, "[") && strings.HasSuffix(dest, "]")) {
					// If destination is a hostname/IPv4 without a ":port" suffix (or bracketed IPv6 without port),
					// default the destination port to the local port.
					p.Destination = fmt.Sprintf("%s:%d", dest, p.Port)
				} else {
					p.Destination = dest
				}
			}
			if p.ConnectionTimeout == 0 {
				p.ConnectionTimeout = 30 // Default: 30 seconds
			}
		}
	}
}

// Validate performs comprehensive validation of the configuration.
// Zero tailnets (len(cfg.Tailnets) == 0) is permitted for standby/unconfigured deployments.
func Validate(cfg *Config) error {
	if _, err := CanonicalListenAddress(cfg.Proxy.APIListenAddress); err != nil {
		return fmt.Errorf("invalid proxy api_listen_address '%s': must be a valid IP address", cfg.Proxy.APIListenAddress)
	}

	if apStr := strings.TrimSpace(os.Getenv("PROXTAIL_API_PORT")); apStr != "" {
		if ap, err := strconv.Atoi(apStr); err != nil || ap < 0 || ap > 65535 {
			return fmt.Errorf("invalid PROXTAIL_API_PORT '%s': must be an integer between 0 and 65535", apStr)
		}
	}
	if cfg.Proxy.APIPort < 0 || cfg.Proxy.APIPort > 65535 {
		return fmt.Errorf("invalid api_port %d: must be between 1 and 65535", cfg.Proxy.APIPort)
	}

	vipRangeSpec := strings.TrimSpace(cfg.Proxy.VIPPortRange)
	if vipRangeSpec == "" {
		vipRangeSpec = DefaultVIPPortRange
	}
	vipStart, vipEnd, err := ParsePortRange(vipRangeSpec)
	if err != nil {
		return err
	}

	tailnetProfiles := make(map[string]bool)
	wireguardPorts := make(map[uint16]string)
	var autoVIPListenAddrs []string
	for _, tailnet := range cfg.Tailnets {
		if strings.TrimSpace(tailnet.Profile) == "" {
			return fmt.Errorf("tailnet profile cannot be empty")
		}
		if !isValidIdentifier(tailnet.Profile) {
			return fmt.Errorf("invalid tailnet profile '%s': must contain only alphanumeric characters, '.', '_', or '-'", tailnet.Profile)
		}
		if tailnetProfiles[tailnet.Profile] {
			return fmt.Errorf("duplicate tailnet profile: %s", tailnet.Profile)
		}
		tailnetProfiles[tailnet.Profile] = true

		if tailnet.Hostname != "" && !isValidIdentifier(tailnet.Hostname) {
			return fmt.Errorf("invalid hostname '%s' in tailnet %s: must contain only alphanumeric characters, '.', '_', or '-'", tailnet.Hostname, tailnet.Profile)
		}

		if tailnet.ControlURL != "" {
			u, err := url.Parse(tailnet.ControlURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(tailnet.ControlURL, " \t\n") {
				return fmt.Errorf("invalid control_url '%s' in tailnet %s: must be a valid http:// or https:// URL", tailnet.ControlURL, tailnet.Profile)
			}
		}

		if _, err := CanonicalListenAddress(tailnet.ListenAddress); err != nil {
			return fmt.Errorf("invalid listen_address '%s' in tailnet %s: must be a valid IP address", tailnet.ListenAddress, tailnet.Profile)
		}

		if tailnet.WireGuardPort > 0 {
			if existingProfile, exists := wireguardPorts[tailnet.WireGuardPort]; exists {
				return fmt.Errorf("port conflict: wireguard_port %d is used by both tailnet '%s' and tailnet '%s'",
					tailnet.WireGuardPort, existingProfile, tailnet.Profile)
			}
			wireguardPorts[tailnet.WireGuardPort] = tailnet.Profile
		}

		if tailnet.IsAutoProxyVIPs() {
			autoVIPListenAddrs = append(autoVIPListenAddrs, tailnet.EffectiveListenAddress())
		}

		for _, tag := range tailnet.Tags {
			if tag == "" || tag == "tag:" || strings.ContainsAny(tag, " \t\n") {
				return fmt.Errorf("invalid ACL tag '%s' in tailnet %s", tag, tailnet.Profile)
			}
		}
	}

	if len(autoVIPListenAddrs) > 0 {
		for _, tailnet := range cfg.Tailnets {
			if tailnet.WireGuardPort > 0 && int(tailnet.WireGuardPort) >= vipStart && int(tailnet.WireGuardPort) <= vipEnd {
				return fmt.Errorf("port conflict: wireguard_port %d in tailnet '%s' falls within reserved vip_port_range %d-%d",
					tailnet.WireGuardPort, tailnet.Profile, vipStart, vipEnd)
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

	if cfg.Proxy.APIPort >= vipStart && cfg.Proxy.APIPort <= vipEnd && overlapsAutoVIPListenAddr(cfg.Proxy.EffectiveAPIListenAddress()) {
		return fmt.Errorf("port conflict: api_port %d falls within reserved vip_port_range %d-%d",
			cfg.Proxy.APIPort, vipStart, vipEnd)
	}

	type boundPortEntry struct {
		tailnet    string
		listenAddr string
	}
	portBindings := make(map[string][]boundPortEntry) // "<port>-<protocol>" -> entries
	totalPorts := 0

	for _, tailnet := range cfg.Tailnets {
		tailnetListenAddr := tailnet.EffectiveListenAddress()
		for _, port := range tailnet.Ports {
			totalPorts++
			if port.Port == 0 {
				return fmt.Errorf("missing or invalid port 0 in tailnet %s: must be between 1 and 65535", tailnet.Profile)
			}
			if port.Port < 0 || port.Port > 65535 {
				return fmt.Errorf("invalid port %d in tailnet %s: must be between 1 and 65535", port.Port, tailnet.Profile)
			}
			portListenAddr, err := CanonicalListenAddress(port.ListenAddress)
			if err != nil {
				return fmt.Errorf("invalid listen_address '%s' in tailnet %s, port %d: must be a valid IP address",
					port.ListenAddress, tailnet.Profile, port.Port)
			}
			if portListenAddr == "" {
				portListenAddr = tailnetListenAddr
			}
			if port.Port >= vipStart && port.Port <= vipEnd && overlapsAutoVIPListenAddr(portListenAddr) {
				return fmt.Errorf("port conflict: manual port %d in tailnet '%s' falls within reserved vip_port_range %d-%d",
					port.Port, tailnet.Profile, vipStart, vipEnd)
			}
			dest := strings.TrimSpace(port.Destination)
			if dest == "" {
				return fmt.Errorf("empty destination in tailnet %s, port %d", tailnet.Profile, port.Port)
			}
			if strings.Contains(dest, "://") {
				return fmt.Errorf("invalid destination '%s' in tailnet %s, port %d: must be in host:port or host format without URL scheme", port.Destination, tailnet.Profile, port.Port)
			}
			if strings.ContainsAny(dest, " \t\n/") {
				return fmt.Errorf("invalid destination '%s' in tailnet %s, port %d: must be in host:port or host format without path or whitespace", port.Destination, tailnet.Profile, port.Port)
			}
			destHost, destPortStr, err := net.SplitHostPort(dest)
			if err != nil || strings.TrimSpace(destHost) == "" {
				return fmt.Errorf("invalid destination '%s' in tailnet %s, port %d: must be in host:port or host format", port.Destination, tailnet.Profile, port.Port)
			}
			destPort, err := strconv.Atoi(destPortStr)
			if err != nil || destPort <= 0 || destPort > 65535 {
				return fmt.Errorf("invalid destination port '%s' in tailnet %s, port %d: must be between 1 and 65535", destPortStr, tailnet.Profile, port.Port)
			}
			if strings.TrimSpace(port.Protocol) == "" {
				return fmt.Errorf("empty protocol in tailnet %s, port %d", tailnet.Profile, port.Port)
			}

			protocol := strings.ToLower(port.Protocol)
			if protocol != "tcp" && protocol != "udp" {
				return fmt.Errorf("invalid protocol '%s' in tailnet %s, port %d: must be 'tcp' or 'udp'", port.Protocol, tailnet.Profile, port.Port)
			}

			if protocol == "udp" {
				if wgTailnet, exists := wireguardPorts[uint16(port.Port)]; exists {
					return fmt.Errorf("port conflict: wireguard_port %d in tailnet '%s' conflicts with UDP port %d in tailnet '%s'",
						port.Port, wgTailnet, port.Port, tailnet.Profile)
				}
			}

			if cfg.Proxy.APIPort > 0 && protocol == "tcp" && port.Port == cfg.Proxy.APIPort {
				apiBind := cfg.Proxy.EffectiveAPIListenAddress()
				if ListenAddressesConflict(portListenAddr, apiBind) {
					return fmt.Errorf("port conflict: api_port %d conflicts with TCP port %d in tailnet '%s'",
						cfg.Proxy.APIPort, port.Port, tailnet.Profile)
				}
			}

			portProtoKey := fmt.Sprintf("%d-%s", port.Port, protocol)
			for _, existing := range portBindings[portProtoKey] {
				if ListenAddressesConflict(existing.listenAddr, portListenAddr) {
					if existing.tailnet == tailnet.Profile {
						return fmt.Errorf("port conflict: duplicate port %d (%s) in tailnet '%s'",
							port.Port, protocol, tailnet.Profile)
					}
					return fmt.Errorf("port conflict: port %d (%s) is used by both tailnet '%s' and tailnet '%s'",
						port.Port, protocol, existing.tailnet, tailnet.Profile)
				}
			}
			portBindings[portProtoKey] = append(portBindings[portProtoKey], boundPortEntry{
				tailnet:    tailnet.Profile,
				listenAddr: portListenAddr,
			})

			if port.ConnectionTimeout < 1 || port.ConnectionTimeout > 3600 {
				return fmt.Errorf("invalid timeout %d in tailnet %s, port %d: must be between 1 and 3600 seconds",
					port.ConnectionTimeout, tailnet.Profile, port.Port)
			}
		}
	}

	if cfg.Proxy.BufferSize < 1 || cfg.Proxy.BufferSize > 8192 {
		return fmt.Errorf("invalid buffer_size %d: must be between 1 and 8192 KB", cfg.Proxy.BufferSize)
	}
	if cfg.Proxy.WorkerPoolSize < 1 || cfg.Proxy.WorkerPoolSize > 10000 {
		return fmt.Errorf("invalid worker_pool_size %d: must be between 1 and 10000", cfg.Proxy.WorkerPoolSize)
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
		tailnetListenAddr := tailnet.EffectiveListenAddress()
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
				Source:            port.Port,
				ListenAddress:     listenAddr,
				Destination:       port.Destination,
				Protocol:          protocol,
				Tailnet:           tailnet.Profile,
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
