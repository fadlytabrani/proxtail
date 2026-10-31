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

func TestZeroTailnetsStandbyConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{
			name: "empty tailnets slice",
			yaml: `
tailnets: []
proxy:
  listen_address: '10.254.254.254'
  health_port: 9090
`,
		},
		{
			name: "omitted tailnets field",
			yaml: `
proxy:
  health_port: 9090
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("expected 0-tailnet standby config to be valid, got error: %v", err)
			}
			if len(cfg.Tailnets) != 0 {
				t.Fatalf("expected 0 tailnets, got %d", len(cfg.Tailnets))
			}
			if len(cfg.ToProxyPortMappings()) != 0 {
				t.Fatalf("expected 0 port mappings, got %d", len(cfg.ToProxyPortMappings()))
			}
			if cfg.Proxy.HealthPort != 9090 {
				t.Errorf("expected health_port 9090, got %d", cfg.Proxy.HealthPort)
			}
		})
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		errContains string
	}{
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

	diffVIP := base
	diffVIP.AutoProxyVIPs = &fVal
	if !base.ConnectionSettingsEqual(diffVIP) {
		t.Errorf("expected toggling AutoProxyVIPs not to change ConnectionSettingsEqual")
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

func TestProxyEnvOverrides(t *testing.T) {
	t.Setenv("PROXTAIL_HEALTH_PORT", "9091")
	t.Setenv("PROXTAIL_LISTEN_ADDRESS", "127.0.0.1")

	yamlData := []byte(`
tailnets:
  - ports:
      - port: 8080
        destination: 'web-server'
`)
	cfg, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Proxy.HealthPort != 9091 {
		t.Errorf("expected HealthPort=9091 from PROXTAIL_HEALTH_PORT, got %d", cfg.Proxy.HealthPort)
	}
	if cfg.Proxy.ListenAddress != "127.0.0.1" {
		t.Errorf("expected ListenAddress='127.0.0.1' from PROXTAIL_LISTEN_ADDRESS, got %q", cfg.Proxy.ListenAddress)
	}

	// Verify invalid PROXTAIL_HEALTH_PORT fails validation
	t.Setenv("PROXTAIL_HEALTH_PORT", "not-a-port")
	if _, err := Parse(yamlData); err == nil {
		t.Errorf("expected Parse to fail on invalid PROXTAIL_HEALTH_PORT")
	}
}

func TestZeroPortMappingsAllowed(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "single tailnet explicit empty ports",
			yaml: `
tailnets:
  - name: 'discovery-only'
    ports: []
proxy:
  health_port: 9090
`,
		},
		{
			name: "single tailnet omitted ports",
			yaml: `
tailnets:
  - name: 'discovery-only'
proxy:
  health_port: 9090
`,
		},
		{
			name: "multi-tailnet mixed zero and non-zero ports",
			yaml: `
tailnets:
  - name: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
  - name: 'staging'
    ports: []
`,
		},
		{
			name: "all tailnets zero ports without health port",
			yaml: `
tailnets:
  - name: 'prod'
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("expected config to be valid, got error: %v", err)
			}
			if tc.name == "all tailnets zero ports without health port" {
				mappings := cfg.ToProxyPortMappings()
				if len(mappings) != 0 {
					t.Fatalf("expected 0 port mappings, got %d", len(mappings))
				}
			}
		})
	}
}

func TestProbeSettingsAndDefaults(t *testing.T) {
	// 1. Default config: probe is disabled
	defaultYAML := []byte(`
tailnets:
  - name: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
`)
	cfgDefault, err := Parse(defaultYAML)
	if err != nil {
		t.Fatalf("unexpected error parsing default config: %v", err)
	}
	if cfgDefault.Proxy.Probe.Enabled {
		t.Errorf("expected probe to be disabled by default, got enabled")
	}

	// 2. Explicit probe enabled with custom and defaulted fields
	probeYAML := []byte(`
tailnets:
  - name: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
        probe: false
      - port: 9000
        destination: 'api:9000'
proxy:
  probe:
    enabled: true
    interval: 5
`)
	cfgProbe, err := Parse(probeYAML)
	if err != nil {
		t.Fatalf("unexpected error parsing probe config: %v", err)
	}
	if !cfgProbe.Proxy.Probe.Enabled {
		t.Errorf("expected probe enabled=true")
	}
	if cfgProbe.Proxy.Probe.Interval != 5 {
		t.Errorf("expected interval=5, got %d", cfgProbe.Proxy.Probe.Interval)
	}
	if cfgProbe.Proxy.Probe.Timeout != 2 {
		t.Errorf("expected default timeout=2, got %d", cfgProbe.Proxy.Probe.Timeout)
	}
	if cfgProbe.Proxy.Probe.ConsecutiveFailures != 2 {
		t.Errorf("expected default consecutive_failures=2, got %d", cfgProbe.Proxy.Probe.ConsecutiveFailures)
	}
	if cfgProbe.Proxy.Probe.Cooldown != 5 {
		t.Errorf("expected cooldown to default to interval (5), got %d", cfgProbe.Proxy.Probe.Cooldown)
	}

	mappings := cfgProbe.ToProxyPortMappings()
	if len(mappings) != 2 {
		t.Fatalf("expected 2 port mappings, got %d", len(mappings))
	}
	if mappings[0].Probe == nil || *mappings[0].Probe != false {
		t.Errorf("expected port 8080 to have probe override false, got %+v", mappings[0].Probe)
	}
	if mappings[1].Probe != nil {
		t.Errorf("expected port 9000 to inherit global probe, got %+v", mappings[1].Probe)
	}

	probeCfg := cfgProbe.ToProxyProbeConfig()
	if !probeCfg.Enabled || probeCfg.Interval.Seconds() != 5 || probeCfg.Timeout.Seconds() != 2 || probeCfg.ConsecutiveFailures != 2 {
		t.Errorf("unexpected ToProxyProbeConfig output: %+v", probeCfg)
	}

	// 3. Environment variable override
	t.Setenv("PROXTAIL_PROBE_ENABLED", "true")
	cfgEnv, err := Parse(defaultYAML)
	if err != nil {
		t.Fatalf("unexpected error with env probe enabled: %v", err)
	}
	if !cfgEnv.Proxy.Probe.Enabled {
		t.Errorf("expected PROXTAIL_PROBE_ENABLED=true to enable probe")
	}

	t.Setenv("PROXTAIL_PROBE_ENABLED", "not-a-bool")
	if _, err := Parse(defaultYAML); err == nil {
		t.Errorf("expected error for invalid PROXTAIL_PROBE_ENABLED value, got nil")
	}
	t.Setenv("PROXTAIL_PROBE_ENABLED", "")

	// 4. Per-port probe: true with global enabled: false still populates probe defaults
	perPortYAML := []byte(`
tailnets:
  - name: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
        probe: true
`)
	cfgPerPort, err := Parse(perPortYAML)
	if err != nil {
		t.Fatalf("unexpected error parsing per-port probe config: %v", err)
	}
	if cfgPerPort.Proxy.Probe.Enabled {
		t.Errorf("expected global probe.enabled to remain false")
	}
	if cfgPerPort.Proxy.Probe.Interval != 10 || cfgPerPort.Proxy.Probe.Timeout != 2 || cfgPerPort.Proxy.Probe.ConsecutiveFailures != 2 || cfgPerPort.Proxy.Probe.Cooldown != 10 {
		t.Errorf("expected probe defaults to be populated for per-port override, got %+v", cfgPerPort.Proxy.Probe)
	}

	// 5. Invalid validation settings (including negative values and per-port enabled invalid bounds)
	invalidCases := []string{
		`
tailnets:
  - name: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
proxy:
  probe:
    enabled: true
    interval: -5
`,
		`
tailnets:
  - name: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
proxy:
  probe:
    enabled: true
    interval: 5000
`,
		`
tailnets:
  - name: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
proxy:
  probe:
    enabled: true
    timeout: 100
`,
		`
tailnets:
  - name: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
proxy:
  probe:
    enabled: true
    consecutive_failures: 20
`,
		`
tailnets:
  - name: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
        probe: true
proxy:
  probe:
    cooldown: -1
`,
	}
	for idx, inv := range invalidCases {
		if _, err := Parse([]byte(inv)); err == nil {
			t.Errorf("case %d: expected validation error for invalid probe config, got nil", idx)
		}
	}
}

func TestVIPSettingsDefaultsEnvAndValidation(t *testing.T) {
	// 1. Defaults: auto_proxy_vips=true, vip_port_range="30000-30999", vip_refresh_interval=30
	defaultYAML := []byte(`
tailnets:
  - name: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
  - name: 'staging'
    auto_proxy_vips: false
`)
	cfg, err := Parse(defaultYAML)
	if err != nil {
		t.Fatalf("unexpected error parsing default VIP config: %v", err)
	}
	if !cfg.Tailnets[0].IsAutoProxyVIPs() {
		t.Errorf("expected prod tailnet IsAutoProxyVIPs()=true by default, got false")
	}
	if cfg.Tailnets[1].IsAutoProxyVIPs() {
		t.Errorf("expected staging tailnet IsAutoProxyVIPs()=false, got true")
	}
	if cfg.Proxy.VIPPortRange != DefaultVIPPortRange {
		t.Errorf("expected default VIPPortRange %q, got %q", DefaultVIPPortRange, cfg.Proxy.VIPPortRange)
	}
	if cfg.Proxy.VIPRefreshInterval != DefaultVIPRefreshInterval {
		t.Errorf("expected default VIPRefreshInterval %d, got %d", DefaultVIPRefreshInterval, cfg.Proxy.VIPRefreshInterval)
	}

	// 2. Custom vip_port_range and vip_refresh_interval (allowing manual port 30000 when range moved)
	customYAML := []byte(`
tailnets:
  - name: 'prod'
    ports:
      - port: 30000
        destination: 'web:80'
proxy:
  vip_port_range: '40000-40010'
  vip_refresh_interval: 15
`)
	cfgCustom, err := Parse(customYAML)
	if err != nil {
		t.Fatalf("expected manual port 30000 to succeed when vip_port_range is 40000-40010, got: %v", err)
	}
	start, end, err := ParsePortRange(cfgCustom.Proxy.VIPPortRange)
	if err != nil || start != 40000 || end != 40010 {
		t.Errorf("unexpected ParsePortRange result: start=%d end=%d err=%v", start, end, err)
	}
	if cfgCustom.Proxy.VIPRefreshInterval != 15 {
		t.Errorf("expected VIPRefreshInterval=15, got %d", cfgCustom.Proxy.VIPRefreshInterval)
	}

	// 3. Environment variable overrides
	t.Setenv("PROXTAIL_VIP_PORT_RANGE", "35000-35000")
	t.Setenv("PROXTAIL_VIP_REFRESH_INTERVAL", "45")
	cfgEnv, err := Parse(defaultYAML)
	if err != nil {
		t.Fatalf("unexpected error with VIP env overrides: %v", err)
	}
	if cfgEnv.Proxy.VIPPortRange != "35000-35000" {
		t.Errorf("expected VIPPortRange='35000-35000' from env, got %q", cfgEnv.Proxy.VIPPortRange)
	}
	if cfgEnv.Proxy.VIPRefreshInterval != 45 {
		t.Errorf("expected VIPRefreshInterval=45 from env, got %d", cfgEnv.Proxy.VIPRefreshInterval)
	}

	t.Setenv("PROXTAIL_VIP_REFRESH_INTERVAL", "not-an-int")
	if _, err := Parse(defaultYAML); err == nil {
		t.Errorf("expected Parse to fail on invalid PROXTAIL_VIP_REFRESH_INTERVAL")
	}
	t.Setenv("PROXTAIL_VIP_PORT_RANGE", "")
	t.Setenv("PROXTAIL_VIP_REFRESH_INTERVAL", "")

	// 4. Validation failures for reserved VIP port range conflicts and malformed settings
	invalidCases := []struct {
		name        string
		yaml        string
		errContains string
	}{
		{
			name: "manual source_port at start of default vip_port_range",
			yaml: `
tailnets:
  - name: 'prod'
    ports:
      - source_port: 30000
        destination: 'web:80'
`,
			errContains: "falls within reserved vip_port_range 30000-30999",
		},
		{
			name: "manual port alias at end of default vip_port_range when another tailnet has auto_proxy_vips true on overlapping listen_address",
			yaml: `
tailnets:
  - name: 'prod'
    auto_proxy_vips: false
    ports:
      - port: 30999
        destination: 'web:80'
  - name: 'staging'
    auto_proxy_vips: true
`,
			errContains: "falls within reserved vip_port_range 30000-30999",
		},
		{
			name: "health_port inside vip_port_range",
			yaml: `
tailnets:
  - name: 'prod'
proxy:
  health_port: 30500
`,
			errContains: "health_port 30500 falls within reserved vip_port_range 30000-30999",
		},
		{
			name: "malformed vip_port_range missing hyphen",
			yaml: `
tailnets:
  - name: 'prod'
proxy:
  vip_port_range: '30000'
`,
			errContains: "expected 'start-end' format",
		},
		{
			name: "inverted vip_port_range",
			yaml: `
tailnets:
  - name: 'prod'
proxy:
  vip_port_range: '31000-30000'
`,
			errContains: "start port (31000) cannot be greater than end port (30000)",
		},
		{
			name: "out of bounds vip_port_range",
			yaml: `
tailnets:
  - name: 'prod'
proxy:
  vip_port_range: '30000-70000'
`,
			errContains: "ports must be between 1 and 65535",
		},
		{
			name: "negative vip_refresh_interval",
			yaml: `
tailnets:
  - name: 'prod'
proxy:
  vip_refresh_interval: -5
`,
			errContains: "invalid vip_refresh_interval -5",
		},
		{
			name: "excessive vip_refresh_interval",
			yaml: `
tailnets:
  - name: 'prod'
proxy:
  vip_refresh_interval: 5000
`,
			errContains: "invalid vip_refresh_interval 5000",
		},
	}

	for _, tc := range invalidCases {
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

	// 5. Conditional & per-address vip_port_range reservation:
	// Allowed when all tailnets have auto_proxy_vips: false, or when auto_proxy_vips: true is on a non-overlapping listen_address.
	allowedAllDisabledYAML := []byte(`
tailnets:
  - name: 'prod'
    auto_proxy_vips: false
    ports:
      - port: 30000
        destination: 'web:80'
proxy:
  health_port: 30500
`)
	if _, err := Parse(allowedAllDisabledYAML); err != nil {
		t.Fatalf("expected manual port 30000 and health_port 30500 to be allowed when auto_proxy_vips is false on all tailnets, got: %v", err)
	}

	allowedDisjointListenAddrYAML := []byte(`
tailnets:
  - name: 'vip-tailnet'
    listen_address: '127.0.0.1'
    auto_proxy_vips: true
  - name: 'manual-tailnet'
    listen_address: '127.0.0.2'
    auto_proxy_vips: false
    ports:
      - port: 30000
        destination: 'web:80'
proxy:
  health_listen_address: '127.0.0.3'
  health_port: 30500
`)
	if _, err := Parse(allowedDisjointListenAddrYAML); err != nil {
		t.Fatalf("expected manual port 30000 on 127.0.0.2 and health_port 30500 on 127.0.0.3 to be allowed when auto_proxy_vips tailnet is on 127.0.0.1, got: %v", err)
	}
}

func TestListenAddressCanonicalizationAndConflictDetection(t *testing.T) {
	// 1. CanonicalListenAddress normalizes IPv4-mapped IPv6 and expanded IPv6
	canonCases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"127.0.0.1", "127.0.0.1"},
		{"::ffff:127.0.0.1", "127.0.0.1"},
		{"0:0:0:0:0:0:0:1", "::1"},
		{"0:0:0:0:0:0:0:0", "::"},
	}
	for _, tc := range canonCases {
		got, err := CanonicalListenAddress(tc.in)
		if err != nil {
			t.Fatalf("CanonicalListenAddress(%q) unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("CanonicalListenAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// 2. Wildcard vs specific IP conflicts must be rejected
	conflictCases := []struct {
		name        string
		yaml        string
		errContains string
	}{
		{
			name: "wildcard empty vs specific IPv4 across tailnets",
			yaml: `
tailnets:
  - name: 't1'
    ports:
      - port: 8080
        destination: 'a:80'
  - name: 't2'
    ports:
      - port: 8080
        listen_address: '127.0.0.1'
        destination: 'b:80'
`,
			errContains: "port conflict: port 8080 (tcp) is used by both tailnet 't1' and tailnet 't2'",
		},
		{
			name: "0.0.0.0 vs specific IPv4 across tailnets",
			yaml: `
tailnets:
  - name: 't1'
    listen_address: '0.0.0.0'
    ports:
      - port: 8080
        destination: 'a:80'
  - name: 't2'
    listen_address: '127.0.0.1'
    ports:
      - port: 8080
        destination: 'b:80'
`,
			errContains: "port conflict: port 8080 (tcp) is used by both tailnet 't1' and tailnet 't2'",
		},
		{
			name: "IPv6 unspecified :: vs specific IPv4 across tailnets",
			yaml: `
tailnets:
  - name: 't1'
    listen_address: '::'
    ports:
      - port: 8080
        destination: 'a:80'
  - name: 't2'
    listen_address: '127.0.0.1'
    ports:
      - port: 8080
        destination: 'b:80'
`,
			errContains: "port conflict: port 8080 (tcp) is used by both tailnet 't1' and tailnet 't2'",
		},
		{
			name: "IPv4-mapped IPv6 ::ffff:127.0.0.1 vs 127.0.0.1",
			yaml: `
tailnets:
  - name: 't1'
    listen_address: '::ffff:127.0.0.1'
    ports:
      - port: 8080
        destination: 'a:80'
  - name: 't2'
    listen_address: '127.0.0.1'
    ports:
      - port: 8080
        destination: 'b:80'
`,
			errContains: "port conflict: port 8080 (tcp) is used by both tailnet 't1' and tailnet 't2'",
		},
		{
			name: "expanded IPv6 0:0:0:0:0:0:0:1 vs ::1",
			yaml: `
tailnets:
  - name: 't1'
    listen_address: '0:0:0:0:0:0:0:1'
    ports:
      - port: 8080
        destination: 'a:80'
  - name: 't2'
    listen_address: '::1'
    ports:
      - port: 8080
        destination: 'b:80'
`,
			errContains: "port conflict: port 8080 (tcp) is used by both tailnet 't1' and tailnet 't2'",
		},
		{
			name: "same-tailnet duplicate port error wording",
			yaml: `
tailnets:
  - name: 'prod'
    ports:
      - port: 8080
        destination: 'a:80'
      - port: 8080
        destination: 'b:80'
`,
			errContains: "port conflict: duplicate port 8080 (tcp) in tailnet 'prod'",
		},
		{
			name: "health_port on :: conflicts with port on 127.0.0.1",
			yaml: `
tailnets:
  - name: 'prod'
    listen_address: '127.0.0.1'
    ports:
      - port: 9090
        destination: 'a:80'
proxy:
  health_listen_address: '::'
  health_port: 9090
`,
			errContains: "port conflict: health_port 9090 conflicts with TCP source port 9090 in tailnet 'prod'",
		},
	}

	for _, tc := range conflictCases {
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

	// 3. Disjoint listen addresses (e.g., 127.0.0.1 vs 127.0.0.2, or 0.0.0.0 vs ::1) must succeed
	disjointYAML := []byte(`
tailnets:
  - name: 't1'
    listen_address: '127.0.0.1'
    ports:
      - port: 8080
        destination: 'a:80'
  - name: 't2'
    listen_address: '127.0.0.2'
    ports:
      - port: 8080
        destination: 'b:80'
      - port: 8080
        listen_address: '127.0.0.3'
        destination: 'c:80'
proxy:
  listen_address: '127.0.0.9'
  health_listen_address: '127.0.0.4'
  health_port: 8080
`)
	cfg, err := Parse(disjointYAML)
	if err != nil {
		t.Fatalf("expected disjoint listen_address config to succeed, got: %v", err)
	}
	mappings := cfg.ToProxyPortMappings()
	if len(mappings) != 3 {
		t.Fatalf("expected 3 mappings, got %d", len(mappings))
	}
	if mappings[0].ListenAddress != "127.0.0.1" || mappings[1].ListenAddress != "127.0.0.2" || mappings[2].ListenAddress != "127.0.0.3" {
		t.Errorf("unexpected mapping listen addresses: %+v", mappings)
	}
	if cfg.Proxy.EffectiveHealthListenAddress() != "127.0.0.4" {
		t.Errorf("expected EffectiveHealthListenAddress='127.0.0.4', got %q", cfg.Proxy.EffectiveHealthListenAddress())
	}
}

func TestStrictYAMLKnownFieldsAndUDPProbeMode(t *testing.T) {
	// Unknown YAML field must fail
	unknownFieldYAML := []byte(`
tailnets:
  - name: 'prod'
    listen_adress: '127.0.0.1'
`)
	if _, err := Parse(unknownFieldYAML); err == nil {
		t.Fatalf("expected Parse to fail on unknown YAML field 'listen_adress'")
	}

	// UDP probe mode validation & env override
	t.Setenv("PROXTAIL_PROBE_UDP_MODE", "netmap")
	cfg, err := Parse([]byte(`tailnets: []`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Proxy.Probe.UDPProbeMode != "netmap" {
		t.Errorf("expected UDPProbeMode='netmap', got %q", cfg.Proxy.Probe.UDPProbeMode)
	}
	if cfg.ToProxyProbeConfig().UDPProbeMode != "netmap" {
		t.Errorf("expected ToProxyProbeConfig().UDPProbeMode='netmap', got %q", cfg.ToProxyProbeConfig().UDPProbeMode)
	}
	t.Setenv("PROXTAIL_PROBE_UDP_MODE", "")

	if _, err := Parse([]byte(`
proxy:
  probe:
    udp_probe_mode: 'invalid-mode'
`)); err == nil || !strings.Contains(err.Error(), "invalid probe.udp_probe_mode") {
		t.Fatalf("expected invalid probe.udp_probe_mode error, got: %v", err)
	}
}
