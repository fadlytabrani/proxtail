package config

import (
	"strings"
	"testing"
)

func TestParseValidConfigAndDefaults(t *testing.T) {
	yamlData := []byte(`
tailnets:
  - profile: 'production'
    hostname: 'proxy-prod'
    auth_key: 'TS_AUTHKEY_PROD'
    ephemeral: false
    ports:
      - protocol: 'tcp'
        port: 8080
        destination: 'web-server:8080'
      - protocol: 'UDP'
        port: 12345
        destination: 'game-server:12345'
        timeout: 15
  - profile: 'staging'
    ports:
      - protocol: 'tcp'
        port: 9080
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
	// - single tailnet with omitted profile (defaults to "default")
	// - omitted protocol (defaults to "tcp")
	// - destination without ":port" (infers port)
	yamlData := []byte(`
tailnets:
  - ports:
      - port: 8080
        destination: 'web-server'
      - port: 5353
        destination: 'dns-server'
        protocol: 'udp'
`)

	cfg, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("expected minimal config to parse and validate, got error: %v", err)
	}

	if cfg.Tailnets[0].Profile != "default" {
		t.Errorf("expected single unnamed tailnet to default to 'default', got %q", cfg.Tailnets[0].Profile)
	}
	if cfg.Tailnets[0].AuthKey != "TS_AUTHKEY" {
		t.Errorf("expected single tailnet with TS_AUTHKEY set to use 'TS_AUTHKEY', got %q", cfg.Tailnets[0].AuthKey)
	}
	resolvedKey, err := cfg.Tailnets[0].ResolveAuthKey(true)
	if err != nil || resolvedKey != "tskey-auth-global-fallback" {
		t.Errorf("expected ResolveAuthKey to return 'tskey-auth-global-fallback', got %q (err=%v)", resolvedKey, err)
	}

	p0 := cfg.Tailnets[0].Ports[0]
	if p0.Port != 8080 || p0.Protocol != "tcp" || p0.Destination != "web-server:8080" || p0.ConnectionTimeout != 30 {
		t.Errorf("unexpected shorthand expansion for port[0]: %+v", p0)
	}

	p1 := cfg.Tailnets[0].Ports[1]
	if p1.Port != 5353 || p1.Protocol != "udp" || p1.Destination != "dns-server:5353" {
		t.Errorf("unexpected shorthand expansion for port[1]: %+v", p1)
	}
}

func TestAuthKeyNormalizationAndLiteralSupport(t *testing.T) {
	if got := DefaultAuthKeyEnv("prod-us-east.1"); got != "TS_AUTHKEY_PROD_US_EAST_1" {
		t.Errorf("expected TS_AUTHKEY_PROD_US_EAST_1, got %q", got)
	}

	literalCfg := TailnetConfig{
		Profile: "prod",
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
  api_listen_address: '10.254.254.254'
  api_port: 9090
`,
		},
		{
			name: "omitted tailnets field",
			yaml: `
proxy:
  api_port: 9090
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
			if cfg.Proxy.APIPort != 9090 {
				t.Errorf("expected api_port 9090, got %d", cfg.Proxy.APIPort)
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
  - profile: 'prod'
    ports:
      - port: 8080
        destination: 'a:80'
  - profile: 'prod'
    ports:
      - port: 8081
        destination: 'b:80'
`,
			errContains: "duplicate tailnet profile: prod",
		},
		{
			name: "duplicate port across tailnets",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - port: 8080
        destination: 'a:80'
  - profile: 'staging'
    ports:
      - port: 8080
        destination: 'b:80'
`,
			errContains: "port conflict: port 8080 (tcp) is used by both tailnet 'prod' and tailnet 'staging'",
		},
		{
			name: "invalid protocol",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - protocol: 'http'
        port: 8080
        destination: 'a:80'
`,
			errContains: "invalid protocol 'http'",
		},
		{
			name: "invalid port range",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - port: 70000
        destination: 'a:80'
`,
			errContains: "invalid port 70000",
		},
		{
			name: "invalid timeout range",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - port: 8080
        destination: 'a:80'
        timeout: 5000
`,
			errContains: "invalid timeout 5000",
		},
		{
			name: "malformed destination missing host",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - port: 8080
        destination: ':8080'
`,
			errContains: "must be in host:port or host format",
		},
		{
			name: "invalid destination port range",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - port: 8080
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
		Profile:   "prod",
		Hostname:  "proxy-prod",
		AuthKey:   "TS_AUTHKEY_PROD",
		Ephemeral: &tVal,
	}

	same := TailnetConfig{
		Profile:  "prod",
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
}

func TestOAuthTagsListenAddressAndAPIPort(t *testing.T) {
	t.Setenv("TS_CLIENT_SECRET_PROD", "tskey-client-oauth-secret-999")

	yamlData := []byte(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
    tags: ['proxy', 'tag:ingress']
    wireguard_port: 41641
    ports:
      - port: 8080
        destination: 'web-server'
      - port: 8443
        listen_address: '0.0.0.0'
        destination: 'api-server:443'
proxy:
  api_port: 9090
`)

	cfg, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}

	tn := cfg.Tailnets[0]
	if len(tn.Tags) != 2 || tn.Tags[0] != "tag:proxy" || tn.Tags[1] != "tag:ingress" {
		t.Errorf("expected normalized ACL tags [tag:proxy tag:ingress], got %v", tn.Tags)
	}
	if tn.WireGuardPort != 41641 {
		t.Errorf("expected WireGuardPort=41641, got %d", tn.WireGuardPort)
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
	if cfg.Proxy.EffectiveAPIListenAddress() != "0.0.0.0" {
		t.Errorf("expected default EffectiveAPIListenAddress='0.0.0.0', got %q", cfg.Proxy.EffectiveAPIListenAddress())
	}

	// Verify api_port conflict with TCP port is rejected
	conflictYAML := []byte(`
tailnets:
  - profile: 'prod'
    ports:
      - port: 9090
        destination: 'web-server'
proxy:
  api_port: 9090
`)
	if _, err := Parse(conflictYAML); err == nil || !strings.Contains(err.Error(), "api_port 9090 conflicts") {
		t.Errorf("expected api_port conflict error, got %v", err)
	}
}

func TestProxyEnvOverrides(t *testing.T) {
	t.Setenv("PROXTAIL_API_PORT", "9091")
	t.Setenv("PROXTAIL_API_LISTEN_ADDRESS", "127.0.0.1")

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
	if cfg.Proxy.APIPort != 9091 {
		t.Errorf("expected APIPort=9091 from PROXTAIL_API_PORT, got %d", cfg.Proxy.APIPort)
	}
	if cfg.Proxy.APIListenAddress != "127.0.0.1" {
		t.Errorf("expected APIListenAddress='127.0.0.1' from PROXTAIL_API_LISTEN_ADDRESS, got %q", cfg.Proxy.APIListenAddress)
	}
	if cfg.Tailnets[0].ListenAddress != "0.0.0.0" {
		t.Errorf("expected tailnet ListenAddress to default to '0.0.0.0' (not inherit api_listen_address), got %q", cfg.Tailnets[0].ListenAddress)
	}

	// Verify invalid PROXTAIL_API_PORT fails validation with explicit env var error
	t.Setenv("PROXTAIL_API_PORT", "not-a-port")
	if _, err := Parse(yamlData); err == nil || !strings.Contains(err.Error(), "invalid PROXTAIL_API_PORT 'not-a-port'") {
		t.Errorf("expected Parse to fail on invalid PROXTAIL_API_PORT with explicit message, got: %v", err)
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
  - profile: 'discovery-only'
    ports: []
proxy:
  api_port: 9090
`,
		},
		{
			name: "single tailnet omitted ports",
			yaml: `
tailnets:
  - profile: 'discovery-only'
proxy:
  api_port: 9090
`,
		},
		{
			name: "multi-tailnet mixed zero and non-zero ports",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
  - profile: 'staging'
    ports: []
`,
		},
		{
			name: "all tailnets zero ports without health port",
			yaml: `
tailnets:
  - profile: 'prod'
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
  - profile: 'prod'
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
  - profile: 'prod'
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
  - profile: 'prod'
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
  - profile: 'prod'
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
  - profile: 'prod'
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
  - profile: 'prod'
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
  - profile: 'prod'
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
  - profile: 'prod'
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
	// 1. Defaults: auto_proxy_vips=true, vip_port_range="30000-30999"
	defaultYAML := []byte(`
tailnets:
  - profile: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
  - profile: 'staging'
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

	// 2. Custom vip_port_range (allowing manual port 30000 when range moved)
	customYAML := []byte(`
tailnets:
  - profile: 'prod'
    ports:
      - port: 30000
        destination: 'web:80'
proxy:
  vip_port_range: '40000-40010'
`)
	cfgCustom, err := Parse(customYAML)
	if err != nil {
		t.Fatalf("expected manual port 30000 to succeed when vip_port_range is 40000-40010, got: %v", err)
	}
	start, end, err := ParsePortRange(cfgCustom.Proxy.VIPPortRange)
	if err != nil || start != 40000 || end != 40010 {
		t.Errorf("unexpected ParsePortRange result: start=%d end=%d err=%v", start, end, err)
	}

	// 3. Environment variable overrides (including overriding explicit YAML values)
	t.Setenv("PROXTAIL_VIP_PORT_RANGE", "35000-35000")
	cfgEnv, err := Parse(customYAML)
	if err != nil {
		t.Fatalf("unexpected error with VIP env overrides: %v", err)
	}
	if cfgEnv.Proxy.VIPPortRange != "35000-35000" {
		t.Errorf("expected VIPPortRange='35000-35000' from env override, got %q", cfgEnv.Proxy.VIPPortRange)
	}
	t.Setenv("PROXTAIL_VIP_PORT_RANGE", "")

	// 4. Validation failures for reserved VIP port range conflicts and malformed settings
	invalidCases := []struct {
		name        string
		yaml        string
		errContains string
	}{
		{
			name: "manual port at start of default vip_port_range",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - port: 30000
        destination: 'web:80'
`,
			errContains: "falls within reserved vip_port_range 30000-30999",
		},
		{
			name: "manual port alias at end of default vip_port_range when another tailnet has auto_proxy_vips true on overlapping listen_address",
			yaml: `
tailnets:
  - profile: 'prod'
    auto_proxy_vips: false
    ports:
      - port: 30999
        destination: 'web:80'
  - profile: 'staging'
    auto_proxy_vips: true
`,
			errContains: "falls within reserved vip_port_range 30000-30999",
		},
		{
			name: "api_port inside vip_port_range",
			yaml: `
tailnets:
  - profile: 'prod'
proxy:
  api_port: 30500
`,
			errContains: "api_port 30500 falls within reserved vip_port_range 30000-30999",
		},
		{
			name: "malformed vip_port_range missing hyphen",
			yaml: `
tailnets:
  - profile: 'prod'
proxy:
  vip_port_range: '30000'
`,
			errContains: "expected 'start-end' format",
		},
		{
			name: "inverted vip_port_range",
			yaml: `
tailnets:
  - profile: 'prod'
proxy:
  vip_port_range: '31000-30000'
`,
			errContains: "start port (31000) cannot be greater than end port (30000)",
		},
		{
			name: "out of bounds vip_port_range",
			yaml: `
tailnets:
  - profile: 'prod'
proxy:
  vip_port_range: '30000-70000'
`,
			errContains: "ports must be between 1 and 65535",
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
  - profile: 'prod'
    auto_proxy_vips: false
    ports:
      - port: 30000
        destination: 'web:80'
proxy:
  api_port: 30500
`)
	if _, err := Parse(allowedAllDisabledYAML); err != nil {
		t.Fatalf("expected manual port 30000 and api_port 30500 to be allowed when auto_proxy_vips is false on all tailnets, got: %v", err)
	}

	allowedDisjointListenAddrYAML := []byte(`
tailnets:
  - profile: 'vip-tailnet'
    listen_address: '127.0.0.1'
    auto_proxy_vips: true
  - profile: 'manual-tailnet'
    listen_address: '127.0.0.2'
    auto_proxy_vips: false
    ports:
      - port: 30000
        destination: 'web:80'
proxy:
  api_listen_address: '127.0.0.3'
  api_port: 30500
`)
	if _, err := Parse(allowedDisjointListenAddrYAML); err != nil {
		t.Fatalf("expected manual port 30000 on 127.0.0.2 and api_port 30500 on 127.0.0.3 to be allowed when auto_proxy_vips tailnet is on 127.0.0.1, got: %v", err)
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
  - profile: 't1'
    ports:
      - port: 8080
        destination: 'a:80'
  - profile: 't2'
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
  - profile: 't1'
    listen_address: '0.0.0.0'
    ports:
      - port: 8080
        destination: 'a:80'
  - profile: 't2'
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
  - profile: 't1'
    listen_address: '::'
    ports:
      - port: 8080
        destination: 'a:80'
  - profile: 't2'
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
  - profile: 't1'
    listen_address: '::ffff:127.0.0.1'
    ports:
      - port: 8080
        destination: 'a:80'
  - profile: 't2'
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
  - profile: 't1'
    listen_address: '0:0:0:0:0:0:0:1'
    ports:
      - port: 8080
        destination: 'a:80'
  - profile: 't2'
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
  - profile: 'prod'
    ports:
      - port: 8080
        destination: 'a:80'
      - port: 8080
        destination: 'b:80'
`,
			errContains: "port conflict: duplicate port 8080 (tcp) in tailnet 'prod'",
		},
		{
			name: "api_port on :: conflicts with port on 127.0.0.1",
			yaml: `
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
    ports:
      - port: 9090
        destination: 'a:80'
proxy:
  api_listen_address: '::'
  api_port: 9090
`,
			errContains: "port conflict: api_port 9090 conflicts with TCP port 9090 in tailnet 'prod'",
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
  - profile: 't1'
    listen_address: '127.0.0.1'
    ports:
      - port: 8080
        destination: 'a:80'
  - profile: 't2'
    listen_address: '127.0.0.2'
    ports:
      - port: 8080
        destination: 'b:80'
      - port: 8080
        listen_address: '127.0.0.3'
        destination: 'c:80'
proxy:
  api_listen_address: '127.0.0.4'
  api_port: 8080
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
	if cfg.Proxy.EffectiveAPIListenAddress() != "127.0.0.4" {
		t.Errorf("expected EffectiveAPIListenAddress='127.0.0.4', got %q", cfg.Proxy.EffectiveAPIListenAddress())
	}
}

func TestStrictYAMLKnownFieldsAndUDPProbeMode(t *testing.T) {
	// Unknown YAML field must fail
	unknownFieldYAML := []byte(`
tailnets:
  - profile: 'prod'
    listen_adress: '127.0.0.1'
`)
	if _, err := Parse(unknownFieldYAML); err == nil {
		t.Fatalf("expected Parse to fail on unknown YAML field 'listen_adress'")
	}

	// Removed web_client field must also fail under KnownFields(true)
	if _, err := Parse([]byte(`
tailnets:
  - profile: 'prod'
    web_client: true
`)); err == nil {
		t.Fatalf("expected Parse to fail on removed YAML field 'web_client'")
	}

	// UDP probe mode validation & env override (even when YAML sets payload)
	t.Setenv("PROXTAIL_PROBE_UDP_MODE", "netmap")
	t.Setenv("PROXTAIL_API_LISTEN_ADDRESS", "127.0.0.5")
	t.Setenv("PROXTAIL_API_PORT", "9095")
	t.Setenv("PROXTAIL_PROBE_ENABLED", "false")
	cfg, err := Parse([]byte(`
tailnets: []
proxy:
  api_listen_address: '127.0.0.1'
  api_port: 9090
  probe:
    enabled: true
    udp_probe_mode: 'payload'
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Proxy.APIListenAddress != "127.0.0.5" {
		t.Errorf("expected PROXTAIL_API_LISTEN_ADDRESS to override YAML, got %q", cfg.Proxy.APIListenAddress)
	}
	if cfg.Proxy.APIPort != 9095 {
		t.Errorf("expected PROXTAIL_API_PORT to override YAML, got %d", cfg.Proxy.APIPort)
	}
	if cfg.Proxy.Probe.Enabled {
		t.Errorf("expected PROXTAIL_PROBE_ENABLED=false to override YAML enabled: true")
	}
	if cfg.Proxy.Probe.UDPProbeMode != "netmap" {
		t.Errorf("expected UDPProbeMode='netmap', got %q", cfg.Proxy.Probe.UDPProbeMode)
	}
	if cfg.ToProxyProbeConfig().UDPProbeMode != "netmap" {
		t.Errorf("expected ToProxyProbeConfig().UDPProbeMode='netmap', got %q", cfg.ToProxyProbeConfig().UDPProbeMode)
	}
	t.Setenv("PROXTAIL_PROBE_UDP_MODE", "")
	t.Setenv("PROXTAIL_API_LISTEN_ADDRESS", "")
	t.Setenv("PROXTAIL_API_PORT", "")
	t.Setenv("PROXTAIL_PROBE_ENABLED", "")

	if _, err := Parse([]byte(`
proxy:
  probe:
    udp_probe_mode: 'invalid-mode'
`)); err == nil || !strings.Contains(err.Error(), "invalid probe.udp_probe_mode") {
		t.Fatalf("expected invalid probe.udp_probe_mode error, got: %v", err)
	}

	// Verify removed Tier 1 fields are rejected by strict YAML decoding
	for _, removedYAML := range []string{
		"tailnets:\n  - profile: 'prod'\n    ports:\n      - source_port: 8080\n        destination: 'web:80'\n",
		"proxy:\n  enable_metrics: true\n",
		"proxy:\n  metrics_interval: 30\n",
		"proxy:\n  listen_address: '127.0.0.1'\n",
		"proxy:\n  health_listen_address: '127.0.0.1'\n",
		"proxy:\n  health_port: 9090\n",
		"proxy:\n  vip_refresh_interval: 30\n",
	} {
		if _, err := Parse([]byte(removedYAML)); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("expected strict YAML error for removed field in:\n%s\ngot: %v", removedYAML, err)
		}
	}

	// Verify proxy.state_dir YAML + PROXTAIL_STATE_DIR / TS_STATE_DIR override precedence
	t.Setenv("PROXTAIL_STATE_DIR", "")
	t.Setenv("TS_STATE_DIR", "")
	cfgStateYAML, err := Parse([]byte(`
tailnets:
  - profile: 'prod'
proxy:
  state_dir: '/var/lib/proxtail-yaml'
`))
	if err != nil {
		t.Fatalf("unexpected error parsing proxy.state_dir: %v", err)
	}
	if cfgStateYAML.Proxy.StateDir != "/var/lib/proxtail-yaml" || cfgStateYAML.Tailnets[0].StateDir != "/var/lib/proxtail-yaml" {
		t.Errorf("expected StateDir '/var/lib/proxtail-yaml', got proxy=%q tailnet=%q", cfgStateYAML.Proxy.StateDir, cfgStateYAML.Tailnets[0].StateDir)
	}

	t.Setenv("TS_STATE_DIR", "/var/lib/ts-state-env")
	cfgStateTS, err := Parse([]byte(`
tailnets:
  - profile: 'prod'
proxy:
  state_dir: '/var/lib/proxtail-yaml'
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfgStateTS.Proxy.StateDir != "/var/lib/ts-state-env" || cfgStateTS.Tailnets[0].StateDir != "/var/lib/ts-state-env" {
		t.Errorf("expected TS_STATE_DIR to override proxy.state_dir, got %q", cfgStateTS.Proxy.StateDir)
	}

	t.Setenv("PROXTAIL_STATE_DIR", "/var/lib/proxtail-state-env")
	cfgStateProx, err := Parse([]byte(`
tailnets:
  - profile: 'prod'
proxy:
  state_dir: '/var/lib/proxtail-yaml'
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfgStateProx.Proxy.StateDir != "/var/lib/proxtail-state-env" || cfgStateProx.Tailnets[0].StateDir != "/var/lib/proxtail-state-env" {
		t.Errorf("expected PROXTAIL_STATE_DIR to take precedence over TS_STATE_DIR and YAML, got %q", cfgStateProx.Proxy.StateDir)
	}
	t.Setenv("PROXTAIL_STATE_DIR", "")
	t.Setenv("TS_STATE_DIR", "")

	// Verify TS_AUTHKEY_<PROFILE> overrides a literal auth_key in YAML
	t.Setenv("TS_AUTHKEY_PROD", "tskey-auth-from-env")
	tcLiteral := TailnetConfig{Profile: "prod", AuthKey: "tskey-auth-from-yaml"}
	ak, cs, err := tcLiteral.ResolveCredentials(false)
	if err != nil || ak != "tskey-auth-from-env" || cs != "" {
		t.Errorf("expected TS_AUTHKEY_PROD to override literal YAML auth_key, got ak=%q cs=%q err=%v", ak, cs, err)
	}
	t.Setenv("TS_AUTHKEY_PROD", "")
}

func TestValidationHardeningPhase4(t *testing.T) {
	// 1. Bare IPv6 destination without brackets or port is auto-bracketed with local port (Item 4.3),
	// and valid control_url (Item 4.5) is accepted.
	validIPv6AndControlYAML := []byte(`
tailnets:
  - profile: 'prod-1'
    hostname: 'proxy-node.prod_1'
    control_url: 'https://headscale.internal.example.com:8443'
    ports:
      - port: 8080
        destination: 'fd7a:115c:a1e0::1'
      - port: 8081
        destination: '[fd7a:115c:a1e0::2]'
      - port: 8082
        destination: '[fd7a:115c:a1e0::3]:9090'
`)
	cfg, err := Parse(validIPv6AndControlYAML)
	if err != nil {
		t.Fatalf("expected valid IPv6 and control_url config to succeed, got: %v", err)
	}
	if got := cfg.Tailnets[0].Ports[0].Destination; got != "[fd7a:115c:a1e0::1]:8080" {
		t.Errorf("expected bare IPv6 destination to expand to '[fd7a:115c:a1e0::1]:8080', got %q", got)
	}
	if got := cfg.Tailnets[0].Ports[1].Destination; got != "[fd7a:115c:a1e0::2]:8081" {
		t.Errorf("expected bracketed IPv6 destination to expand to '[fd7a:115c:a1e0::2]:8081', got %q", got)
	}
	if got := cfg.Tailnets[0].Ports[2].Destination; got != "[fd7a:115c:a1e0::3]:9090" {
		t.Errorf("expected bracketed IPv6 destination with port to remain '[fd7a:115c:a1e0::3]:9090', got %q", got)
	}

	// 2. Validation error cases for Items 4.2 - 4.7
	invalidCases := []struct {
		name        string
		yaml        string
		errContains string
	}{
		{
			name: "4.2 negative buffer_size rejected",
			yaml: `
tailnets: []
proxy:
  buffer_size: -64
`,
			errContains: "invalid buffer_size -64: must be between 1 and 8192 KB",
		},
		{
			name: "4.2 negative worker_pool_size rejected",
			yaml: `
tailnets: []
proxy:
  worker_pool_size: -10
`,
			errContains: "invalid worker_pool_size -10: must be between 1 and 10000",
		},
		{
			name: "4.2 negative port timeout rejected",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - port: 8080
        destination: 'web:80'
        timeout: -5
`,
			errContains: "invalid timeout -5 in tailnet prod, port 8080: must be between 1 and 3600 seconds",
		},
		{
			name: "4.3 destination with URL scheme rejected",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - port: 8080
        destination: 'http://web-server:8080'
`,
			errContains: "without URL scheme",
		},
		{
			name: "4.3 destination with path slash rejected",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - port: 8080
        destination: 'web-server/api:8080'
`,
			errContains: "without path or whitespace",
		},
		{
			name: "4.3 destination with internal whitespace rejected",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - port: 8080
        destination: 'web server:8080'
`,
			errContains: "without path or whitespace",
		},
		{
			name: "4.4 duplicate wireguard_port across tailnets",
			yaml: `
tailnets:
  - profile: 'prod'
    wireguard_port: 41641
  - profile: 'staging'
    wireguard_port: 41641
`,
			errContains: "port conflict: wireguard_port 41641 is used by both tailnet 'prod' and tailnet 'staging'",
		},
		{
			name: "4.4 wireguard_port conflicts with UDP port",
			yaml: `
tailnets:
  - profile: 'prod'
    wireguard_port: 41641
  - profile: 'staging'
    ports:
      - port: 41641
        protocol: 'udp'
        destination: 'dns:53'
`,
			errContains: "port conflict: wireguard_port 41641 in tailnet 'prod' conflicts with UDP port 41641 in tailnet 'staging'",
		},
		{
			name: "4.4 wireguard_port inside reserved vip_port_range",
			yaml: `
tailnets:
  - profile: 'prod'
    wireguard_port: 30500
`,
			errContains: "port conflict: wireguard_port 30500 in tailnet 'prod' falls within reserved vip_port_range 30000-30999",
		},
		{
			name: "4.5 control_url without scheme rejected",
			yaml: `
tailnets:
  - profile: 'prod'
    control_url: 'headscale.internal.example.com'
`,
			errContains: "invalid control_url 'headscale.internal.example.com' in tailnet prod: must be a valid http:// or https:// URL",
		},
		{
			name: "4.5 control_url with unsupported scheme rejected",
			yaml: `
tailnets:
  - profile: 'prod'
    control_url: 'ftp://headscale.internal.example.com'
`,
			errContains: "invalid control_url 'ftp://headscale.internal.example.com' in tailnet prod: must be a valid http:// or https:// URL",
		},
		{
			name: "4.6 profile with path traversal rejected",
			yaml: `
tailnets:
  - profile: '../prod'
`,
			errContains: "invalid tailnet profile '../prod'",
		},
		{
			name: "4.6 profile with slash rejected",
			yaml: `
tailnets:
  - profile: 'prod/us'
`,
			errContains: "invalid tailnet profile 'prod/us'",
		},
		{
			name: "4.6 hostname with invalid characters rejected",
			yaml: `
tailnets:
  - profile: 'prod'
    hostname: 'bad/hostname'
`,
			errContains: "invalid hostname 'bad/hostname' in tailnet prod",
		},
		{
			name: "4.7 missing port (0) error message",
			yaml: `
tailnets:
  - profile: 'prod'
    ports:
      - destination: 'web:80'
`,
			errContains: "missing or invalid port 0 in tailnet prod: must be between 1 and 65535",
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
}
