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
	if cfg.Proxy.WorkerPoolSize != 200 {
		t.Errorf("expected default worker_pool_size 200, got %d", cfg.Proxy.WorkerPoolSize)
	}
	if cfg.Proxy.EnableMetrics {
		t.Errorf("expected default enable_metrics false, got true")
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

func TestMinimalOperatorConfigAndShorthands(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "tskey-auth-global-fallback")

	// Minimal config:
	// - single tailnet with omitted name (defaults to "default")
	// - omitted protocol (defaults to "tcp")
	// - "port" alias for "source_port"
	// - destination without ":port" (infers source_port)
	// - metrics_interval set without enable_metrics (auto-enables metrics)
	yamlData := []byte(`
tailnets:
  - ports:
      - port: 8080
        destination: 'web-server'
      - source_port: 5353
        destination: 'dns-server'
        protocol: 'udp'
proxy:
  metrics_interval: 15
`)

	cfg, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("expected minimal config to parse and validate, got error: %v", err)
	}

	if cfg.Tailnets[0].Name != "default" {
		t.Errorf("expected single unnamed tailnet to default to 'default', got %q", cfg.Tailnets[0].Name)
	}
	if cfg.Tailnets[0].AuthKey != "TS_AUTHKEY" {
		t.Errorf("expected single tailnet with TS_AUTHKEY set to use 'TS_AUTHKEY', got %q", cfg.Tailnets[0].AuthKey)
	}
	resolvedKey, err := cfg.Tailnets[0].ResolveAuthKey(true)
	if err != nil || resolvedKey != "tskey-auth-global-fallback" {
		t.Errorf("expected ResolveAuthKey to return 'tskey-auth-global-fallback', got %q (err=%v)", resolvedKey, err)
	}

	p0 := cfg.Tailnets[0].Ports[0]
	if p0.Source != 8080 || p0.Protocol != "tcp" || p0.Destination != "web-server:8080" || p0.ConnectionTimeout != 30 {
		t.Errorf("unexpected shorthand expansion for port[0]: %+v", p0)
	}

	p1 := cfg.Tailnets[0].Ports[1]
	if p1.Source != 5353 || p1.Protocol != "udp" || p1.Destination != "dns-server:5353" {
		t.Errorf("unexpected shorthand expansion for port[1]: %+v", p1)
	}

	if !cfg.Proxy.EnableMetrics || cfg.Proxy.MetricsInterval != 15 {
		t.Errorf("expected metrics_interval: 15 to auto-enable metrics, got EnableMetrics=%v, MetricsInterval=%d",
			cfg.Proxy.EnableMetrics, cfg.Proxy.MetricsInterval)
	}
}

func TestAuthKeyNormalizationAndLiteralSupport(t *testing.T) {
	if got := DefaultAuthKeyEnv("prod-us-east.1"); got != "TS_AUTHKEY_PROD_US_EAST_1" {
		t.Errorf("expected TS_AUTHKEY_PROD_US_EAST_1, got %q", got)
	}

	literalCfg := TailnetConfig{
		Name:    "prod",
		AuthKey: "tskey-auth-direct-123",
	}
	key, err := literalCfg.ResolveAuthKey(false)
	if err != nil || key != "tskey-auth-direct-123" {
		t.Errorf("expected literal tskey-* to resolve directly, got %q (err=%v)", key, err)
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
      - source_port: 8080
        destination: 'a:80'
  - name: 'prod'
    ports:
      - source_port: 8081
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
      - source_port: 8080
        destination: 'a:80'
  - name: 'staging'
    ports:
      - source_port: 8080
        destination: 'b:80'
`,
			errContains: "port conflict: port 8080 (tcp) is used by both tailnet 'prod' and tailnet 'staging'",
		},
		{
			name: "conflicting source_port and port alias",
			yaml: `
tailnets:
  - name: 'prod'
    ports:
      - source_port: 8080
        port: 9090
        destination: 'a:80'
`,
			errContains: "conflicting source_port (8080) and port (9090)",
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
      - source_port: 70000
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
      - source_port: 8080
        destination: 'a:80'
        timeout: 5000
`,
			errContains: "invalid timeout 5000",
		},
		{
			name: "malformed destination missing host",
			yaml: `
tailnets:
  - name: 'prod'
    ports:
      - source_port: 8080
        destination: ':8080'
`,
			errContains: "must be in host:port or host format",
		},
		{
			name: "invalid destination port range",
			yaml: `
tailnets:
  - name: 'prod'
    ports:
      - source_port: 8080
        destination: 'web-server:70000'
`,
			errContains: "invalid destination port '70000'",
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

func TestConnectionSettingsEqual(t *testing.T) {
	tVal := true
	fVal := false
	base := TailnetConfig{
		Name:      "prod",
		Hostname:  "proxy-prod",
		AuthKey:   "TS_AUTHKEY_PROD",
		Ephemeral: &tVal,
	}

	same := TailnetConfig{
		Name:     "prod",
		Hostname: "proxy-prod",
		AuthKey:  "TS_AUTHKEY_PROD",
		// nil Ephemeral defaults to true
	}
	if !base.ConnectionSettingsEqual(same) {
		t.Errorf("expected base and same to be equal")
	}

	diffHost := base
	diffHost.Hostname = "proxy-prod-2"
	if base.ConnectionSettingsEqual(diffHost) {
		t.Errorf("expected different hostname to not be equal")
	}

	diffEph := base
	diffEph.Ephemeral = &fVal
	if base.ConnectionSettingsEqual(diffEph) {
		t.Errorf("expected different ephemeral to not be equal")
	}
}

func TestExampleConfigFileValid(t *testing.T) {
	cfg, err := Load("../../examples/config.yaml")
	if err != nil {
		t.Fatalf("examples/config.yaml failed to load and validate: %v", err)
	}
	if len(cfg.Tailnets) != 3 {
		t.Fatalf("expected 3 tailnets in examples/config.yaml, got %d", len(cfg.Tailnets))
	}
	if !cfg.Proxy.EnableMetrics || cfg.Proxy.MetricsInterval != 30 {
		t.Errorf("expected examples/config.yaml metrics enabled at 30s, got EnableMetrics=%v, Interval=%d",
			cfg.Proxy.EnableMetrics, cfg.Proxy.MetricsInterval)
	}
}

func TestOAuthTagsListenAddressAndHealthPort(t *testing.T) {
	t.Setenv("TS_CLIENT_SECRET_PROD", "tskey-client-oauth-secret-999")

	yamlData := []byte(`
tailnets:
  - name: 'prod'
    tags: ['proxy', 'tag:ingress']
    wireguard_port: 41641
    web_client: true
    ports:
      - port: 8080
        destination: 'web-server'
      - port: 8443
        listen_address: '0.0.0.0'
        destination: 'api-server:443'
proxy:
  listen_address: '127.0.0.1'
  health_port: 9090
`)

	cfg, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}

	tn := cfg.Tailnets[0]
	if len(tn.Tags) != 2 || tn.Tags[0] != "tag:proxy" || tn.Tags[1] != "tag:ingress" {
		t.Errorf("expected normalized ACL tags [tag:proxy tag:ingress], got %v", tn.Tags)
	}
	if tn.WireGuardPort != 41641 || !tn.WebClient {
		t.Errorf("expected WireGuardPort=41641 and WebClient=true, got %d, %v", tn.WireGuardPort, tn.WebClient)
	}

	authKey, clientSecret, err := tn.ResolveCredentials(false)
	if err != nil || authKey != "" || clientSecret != "tskey-client-oauth-secret-999" {
		t.Errorf("expected OAuth clientSecret resolution, got authKey=%q clientSecret=%q err=%v", authKey, clientSecret, err)
	}

	mappings := cfg.ToProxyPortMappings()
	if mappings[0].ListenAddress != "127.0.0.1" || mappings[0].ListenAddr() != "127.0.0.1:8080" {
		t.Errorf("expected inherited listen_address 127.0.0.1:8080, got %q", mappings[0].ListenAddr())
	}
	if mappings[1].ListenAddress != "0.0.0.0" || mappings[1].ListenAddr() != "0.0.0.0:8443" {
		t.Errorf("expected overridden listen_address 0.0.0.0:8443, got %q", mappings[1].ListenAddr())
	}

	// Verify health_port conflict with TCP source port is rejected
	conflictYAML := []byte(`
tailnets:
  - name: 'prod'
    ports:
      - port: 9090
        destination: 'web-server'
proxy:
  health_port: 9090
`)
	if _, err := Parse(conflictYAML); err == nil || !strings.Contains(err.Error(), "health_port 9090 conflicts") {
		t.Errorf("expected health_port conflict error, got %v", err)
	}
}

