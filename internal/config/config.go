package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"proxtail/internal/proxy"
)

// TailnetConfig defines settings for a specific Tailscale network.
type TailnetConfig struct {
	Name      string        `yaml:"name"`                // Unique identifier for this tailnet (required)
	Hostname  string        `yaml:"hostname,omitempty"`  // Hostname for this tailnet (optional, defaults to "<system-hostname>-<name>")
	AuthKey   string        `yaml:"auth_key,omitempty"`  // Auth key environment variable name (optional, defaults to "TS_AUTHKEY_<NAME>")
	Ephemeral *bool         `yaml:"ephemeral,omitempty"` // Whether this node is ephemeral (optional, defaults to true)
	Ports     []PortMapping `yaml:"ports"`               // Ports to forward for this tailnet
}

// IsEphemeral returns the configured ephemeral value, defaulting to true if unset.
func (t TailnetConfig) IsEphemeral() bool {
	if t.Ephemeral == nil {
		return true
	}
	return *t.Ephemeral
}

// PortMapping defines a port forwarding rule under a tailnet.
type PortMapping struct {
	Source            int    `yaml:"source_port"`       // Local source port to listen on (1-65535)
	Destination       string `yaml:"destination"`       // Destination address in tailnet (e.g., "backend:8080")
	Tailnet           string `yaml:"tailnet,omitempty"` // Populated automatically from parent TailnetConfig
	Protocol          string `yaml:"protocol"`          // Protocol: "tcp" or "udp"
	ConnectionTimeout int    `yaml:"timeout,omitempty"` // Connection timeout in seconds (default: 30)
}

// ProxySettings defines performance and monitoring settings for the proxy server.
type ProxySettings struct {
	BufferSize      int  `yaml:"buffer_size,omitempty"`      // Buffer size in KB (default: 512)
	QueueSize       int  `yaml:"queue_size,omitempty"`       // Queue size for connections/packets (default: 100)
	WorkerPoolSize  int  `yaml:"worker_pool_size,omitempty"` // Max concurrent workers (default: 2x queue_size)
	EnableMetrics   bool `yaml:"enable_metrics,omitempty"`   // Enable performance metrics logging (default: false)
	MetricsInterval int  `yaml:"metrics_interval,omitempty"` // Metrics logging interval in seconds (default: 60)
}

// Config defines the structure for the YAML configuration file.
type Config struct {
	Tailnets []TailnetConfig `yaml:"tailnets"`
	Proxy    ProxySettings   `yaml:"proxy"`
}

// Load reads, parses, applies defaults, and validates the YAML configuration file.
func Load(file string) (*Config, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", file, err)
	}
	return Parse(data)
}

// Parse unmarshals YAML data, applies defaults, and validates the configuration.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
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
	if cfg.Proxy.QueueSize <= 0 {
		cfg.Proxy.QueueSize = 100 // Default: 100 connections/packets
	}
	if cfg.Proxy.WorkerPoolSize <= 0 {
		cfg.Proxy.WorkerPoolSize = cfg.Proxy.QueueSize * 2 // Default: 2x queue size
	}
	if cfg.Proxy.MetricsInterval <= 0 {
		cfg.Proxy.MetricsInterval = 60 // Default: 60 seconds
	}

	sysHostname, err := os.Hostname()
	if err != nil || sysHostname == "" {
		sysHostname = "proxtail"
	}

	for i := range cfg.Tailnets {
		if cfg.Tailnets[i].Hostname == "" {
			cfg.Tailnets[i].Hostname = fmt.Sprintf("%s-%s", sysHostname, cfg.Tailnets[i].Name)
		}
		if cfg.Tailnets[i].Ephemeral == nil {
			defaultEphemeral := true
			cfg.Tailnets[i].Ephemeral = &defaultEphemeral
		}
		if cfg.Tailnets[i].AuthKey == "" && cfg.Tailnets[i].Name != "" {
			cfg.Tailnets[i].AuthKey = fmt.Sprintf("TS_AUTHKEY_%s", strings.ToUpper(cfg.Tailnets[i].Name))
		}
		for j := range cfg.Tailnets[i].Ports {
			cfg.Tailnets[i].Ports[j].Tailnet = cfg.Tailnets[i].Name
			if cfg.Tailnets[i].Ports[j].ConnectionTimeout <= 0 {
				cfg.Tailnets[i].Ports[j].ConnectionTimeout = 30 // Default: 30 seconds
			}
		}
	}
}

// Validate performs comprehensive validation of the configuration.
func Validate(cfg *Config) error {
	if len(cfg.Tailnets) == 0 {
		return fmt.Errorf("at least one tailnet must be configured")
	}

	tailnetNames := make(map[string]bool)
	for _, tailnet := range cfg.Tailnets {
		if strings.TrimSpace(tailnet.Name) == "" {
			return fmt.Errorf("tailnet name cannot be empty")
		}
		if tailnetNames[tailnet.Name] {
			return fmt.Errorf("duplicate tailnet name: %s", tailnet.Name)
		}
		tailnetNames[tailnet.Name] = true
	}

	portMap := make(map[string]string) // "<port>-<protocol>" -> tailnet name
	totalPorts := 0

	for _, tailnet := range cfg.Tailnets {
		for _, port := range tailnet.Ports {
			totalPorts++
			if port.Source <= 0 || port.Source > 65535 {
				return fmt.Errorf("invalid source port %d in tailnet %s: must be between 1 and 65535", port.Source, tailnet.Name)
			}
			if strings.TrimSpace(port.Destination) == "" {
				return fmt.Errorf("empty destination in tailnet %s, port %d", tailnet.Name, port.Source)
			}
			if strings.TrimSpace(port.Protocol) == "" {
				return fmt.Errorf("empty protocol in tailnet %s, port %d", tailnet.Name, port.Source)
			}

			protocol := strings.ToLower(port.Protocol)
			if protocol != "tcp" && protocol != "udp" {
				return fmt.Errorf("invalid protocol '%s' in tailnet %s, port %d: must be 'tcp' or 'udp'", port.Protocol, tailnet.Name, port.Source)
			}

			portKey := fmt.Sprintf("%d-%s", port.Source, protocol)
			if existingTailnet, exists := portMap[portKey]; exists {
				return fmt.Errorf("port conflict: port %d (%s) is used by both tailnet '%s' and tailnet '%s'",
					port.Source, protocol, existingTailnet, tailnet.Name)
			}
			portMap[portKey] = tailnet.Name

			if port.ConnectionTimeout < 1 || port.ConnectionTimeout > 3600 {
				return fmt.Errorf("invalid timeout %d in tailnet %s, port %d: must be between 1 and 3600 seconds",
					port.ConnectionTimeout, tailnet.Name, port.Source)
			}
		}
	}

	if totalPorts == 0 {
		return fmt.Errorf("at least one port mapping must be configured")
	}

	if cfg.Proxy.BufferSize < 1 || cfg.Proxy.BufferSize > 8192 {
		return fmt.Errorf("invalid buffer_size %d: must be between 1 and 8192 KB", cfg.Proxy.BufferSize)
	}
	if cfg.Proxy.QueueSize < 1 || cfg.Proxy.QueueSize > 10000 {
		return fmt.Errorf("invalid queue_size %d: must be between 1 and 10000", cfg.Proxy.QueueSize)
	}
	if cfg.Proxy.WorkerPoolSize < 1 || cfg.Proxy.WorkerPoolSize > 10000 {
		return fmt.Errorf("invalid worker_pool_size %d: must be between 1 and 10000", cfg.Proxy.WorkerPoolSize)
	}
	if cfg.Proxy.MetricsInterval < 1 || cfg.Proxy.MetricsInterval > 3600 {
		return fmt.Errorf("invalid metrics_interval %d: must be between 1 and 3600 seconds", cfg.Proxy.MetricsInterval)
	}

	return nil
}

// ToProxyPortMappings flattens all nested tailnet port configurations into a slice of proxy.PortMapping.
func (c *Config) ToProxyPortMappings() []proxy.PortMapping {
	var mappings []proxy.PortMapping
	for _, tailnet := range c.Tailnets {
		for _, port := range tailnet.Ports {
			timeout := port.ConnectionTimeout
			if timeout <= 0 {
				timeout = 30
			}
			mappings = append(mappings, proxy.PortMapping{
				Source:            port.Source,
				Destination:       port.Destination,
				Protocol:          strings.ToLower(port.Protocol),
				Tailnet:           tailnet.Name,
				ConnectionTimeout: timeout,
			})
		}
	}
	return mappings
}
