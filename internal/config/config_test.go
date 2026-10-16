package config

import (
	"strings"
	"testing"
)

func TestParseValidConfigAndDefaults(t *testing.T) {
	yamlData := []byte(`
tailnets:
  - name: 'production'
    hostname: 'proxy-prod'
    auth_key: 'TS_AUTHKEY_PROD'
    ephemeral: false
    ports:
      - protocol: 'tcp'
        source_port: 8080
        destination: 'web-server:8080'
      - protocol: 'UDP'
        source_port: 12345
        destination: 'game-server:12345'
        timeout: 15
  - name: 'staging'
    ports:
      - protocol: 'tcp'
        source_port: 9080
        destination: 'staging-web:8080'
`)

	cfg, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}

	if len(cfg.Tailnets) != 2 {
		t.Fatalf("expected 2 tailnets, got %d", len(cfg.Tailnets))
	}

	// Verify explicit ephemeral: false is preserved
	if cfg.Tailnets[0].IsEphemeral() {
		t.Errorf("expected production tailnet ephemeral=false, got true")
	}

	// Verify omitted ephemeral defaults to true
	if !cfg.Tailnets[1].IsEphemeral() {
		t.Errorf("expected staging tailnet ephemeral=true by default, got false")
	}

	// Verify omitted port timeout defaults to 30s
	if cfg.Tailnets[0].Ports[0].ConnectionTimeout != 30 {
		t.Errorf("expected default timeout 30, got %d", cfg.Tailnets[0].Ports[0].ConnectionTimeout)
	}
	if cfg.Tailnets[0].Ports[1].ConnectionTimeout != 15 {
		t.Errorf("expected explicit timeout 15, got %d", cfg.Tailnets[0].Ports[1].ConnectionTimeout)
	}

	// Verify proxy defaults
	if cfg.Proxy.BufferSize != 512 {
		t.Errorf("expected default buffer_size 512, got %d", cfg.Proxy.BufferSize)
	}
	if cfg.Proxy.QueueSize != 100 {
		t.Errorf("expected default queue_size 100, got %d", cfg.Proxy.QueueSize)
	}
	if cfg.Proxy.WorkerPoolSize != 200 {
		t.Errorf("expected default worker_pool_size 200, got %d", cfg.Proxy.WorkerPoolSize)
	}
	if cfg.Proxy.MetricsInterval != 60 {
		t.Errorf("expected default metrics_interval 60, got %d", cfg.Proxy.MetricsInterval)
	}

	mappings := cfg.ToProxyPortMappings()
	if len(mappings) != 3 {
		t.Fatalf("expected 3 flattened port mappings, got %d", len(mappings))
	}
	if mappings[0].Tailnet != "production" || mappings[0].ConnectionTimeout != 30 {
		t.Errorf("unexpected mapping[0]: %+v", mappings[0])
	}
	if mappings[1].Protocol != "udp" {
		t.Errorf("expected normalized lowercase protocol 'udp', got %q", mappings[1].Protocol)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		errContains string
	}{
		{
			name:        "no tailnets",
			yaml:        `tailnets: []`,
			errContains: "at least one tailnet must be configured",
		},
		{
			name: "duplicate tailnet names",
			yaml: `
tailnets:
  - name: 'prod'
    ports:
      - protocol: 'tcp'
        source_port: 8080
        destination: 'a:80'
  - name: 'prod'
    ports:
      - protocol: 'tcp'
        source_port: 8081
        destination: 'b:80'
`,
			errContains: "duplicate tailnet name: prod",
		},
		{
			name: "duplicate port across tailnets",
			yaml: `
tailnets:
  - name: 'prod'
    ports:
      - protocol: 'tcp'
        source_port: 8080
        destination: 'a:80'
  - name: 'staging'
    ports:
      - protocol: 'tcp'
        source_port: 8080
        destination: 'b:80'
`,
			errContains: "port conflict: port 8080 (tcp) is used by both tailnet 'prod' and tailnet 'staging'",
		},
		{
			name: "invalid protocol",
			yaml: `
tailnets:
  - name: 'prod'
    ports:
      - protocol: 'http'
        source_port: 8080
        destination: 'a:80'
`,
			errContains: "invalid protocol 'http'",
		},
		{
			name: "invalid port range",
			yaml: `
tailnets:
  - name: 'prod'
    ports:
      - protocol: 'tcp'
        source_port: 70000
        destination: 'a:80'
`,
			errContains: "invalid source port 70000",
		},
		{
			name: "invalid timeout range",
			yaml: `
tailnets:
  - name: 'prod'
    ports:
      - protocol: 'tcp'
        source_port: 8080
        destination: 'a:80'
        timeout: 5000
`,
			errContains: "invalid timeout 5000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errContains)
			}
			if !strings.Contains(err.Error(), tc.errContains) {
				t.Fatalf("expected error containing %q, got %q", tc.errContains, err.Error())
			}
		})
	}
}
