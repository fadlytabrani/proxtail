package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/net/netmon"
	"tailscale.com/net/netns"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
	"tailscale.com/util/eventbus"

	"proxtail/internal/config"
	"proxtail/internal/proxy"
)

func TestTailnetManagerSyncTailnets(t *testing.T) {
	var mu sync.Mutex
	var initialized []string
	var cleaned []string

	tm := newTailnetManager()
	tm.initFunc = func(cfg config.TailnetConfig) (*tsnet.Server, error) {
		mu.Lock()
		initialized = append(initialized, cfg.Name+":"+cfg.Hostname)
		mu.Unlock()
		return &tsnet.Server{Hostname: cfg.Hostname}, nil
	}
	tm.cleanupFunc = func(srv *tsnet.Server, name string) {
		mu.Lock()
		cleaned = append(cleaned, name+":"+srv.Hostname)
		mu.Unlock()
	}

	tVal := true
	fVal := false

	// 1. Initial sync with two tailnets
	initialCfg := []config.TailnetConfig{
		{Name: "prod", Hostname: "proxy-prod-v1", AuthKey: "TS_AUTHKEY_PROD", Ephemeral: &tVal},
		{Name: "staging", Hostname: "proxy-staging", AuthKey: "TS_AUTHKEY_STAGING", Ephemeral: &tVal},
	}

	servers, restarted, cleanup, err := tm.syncTailnets(initialCfg)
	if err != nil {
		t.Fatalf("syncTailnets failed: %v", err)
	}
	cleanup()

	if len(servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(servers))
	}
	if len(restarted) != 0 {
		t.Fatalf("expected 0 restarted tailnets on initial sync, got %d", len(restarted))
	}

	// 2. Modify prod hostname, remove staging, add dev
	updatedCfg := []config.TailnetConfig{
		{Name: "prod", Hostname: "proxy-prod-v2", AuthKey: "TS_AUTHKEY_PROD", Ephemeral: &fVal},
		{Name: "dev", Hostname: "proxy-dev", AuthKey: "TS_AUTHKEY_DEV", Ephemeral: &tVal},
	}

	servers2, restarted2, cleanup2, err := tm.syncTailnets(updatedCfg)
	if err != nil {
		t.Fatalf("second syncTailnets failed: %v", err)
	}
	cleanup2()

	if len(servers2) != 2 {
		t.Fatalf("expected 2 servers after update, got %d", len(servers2))
	}
	if !restarted2["prod"] {
		t.Errorf("expected 'prod' to be marked as restarted when connection settings changed")
	}
	if servers2["prod"].Hostname != "proxy-prod-v2" {
		t.Errorf("expected updated prod server hostname 'proxy-prod-v2', got %q", servers2["prod"].Hostname)
	}
	if _, exists := servers2["staging"]; exists {
		t.Errorf("expected 'staging' to be removed")
	}
	if len(cleaned) != 2 {
		t.Errorf("expected 2 cleaned up servers (old prod and removed staging), got %v", cleaned)
	}
}

func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestMultiTailnetReloadConfigEndToEnd(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	port1 := getFreePort(t)
	port2 := getFreePort(t)
	port3 := getFreePort(t)

	initialYAML := fmt.Sprintf(`
tailnets:
  - name: 'production'
    hostname: 'prod-v1'
    ports:
      - protocol: 'tcp'
        source_port: %d
        destination: '100.64.0.1:8080'
  - name: 'staging'
    hostname: 'staging-v1'
    ports:
      - protocol: 'tcp'
        source_port: %d
        destination: '100.64.0.1:8080'
`, port1, port2)

	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0o600); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	var cleanedTailnets []string
	tm := newTailnetManager()
	tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) {
		return &tsnet.Server{Hostname: c.Hostname}, nil
	}
	tm.cleanupFunc = func(srv *tsnet.Server, name string) {
		cleanedTailnets = append(cleanedTailnets, name+":"+srv.Hostname)
	}
	defer tm.closeAll()

	servers, _, _, err := tm.syncTailnets(cfg.Tailnets)
	if err != nil {
		t.Fatalf("syncTailnets failed: %v", err)
	}

	proxyServer := proxy.NewProxyWithMultiTailnet(
		cfg.ToProxyPortMappings(),
		servers,
		64*1024,
		100,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = proxyServer.Start(ctx) }()
	defer proxyServer.Stop()

	time.Sleep(50 * time.Millisecond)

	// Reload 1: Add 'development' tailnet on port3, modify 'production' hostname to 'prod-v2', remove 'staging'
	updatedYAML := fmt.Sprintf(`
tailnets:
  - name: 'production'
    hostname: 'prod-v2'
    ports:
      - protocol: 'tcp'
        source_port: %d
        destination: '100.64.0.1:8080'
  - name: 'development'
    hostname: 'dev-v1'
    ports:
      - protocol: 'tcp'
        source_port: %d
        destination: '100.64.0.1:9000'
`, port1, port3)

	if err := os.WriteFile(cfgPath, []byte(updatedYAML), 0o600); err != nil {
		t.Fatalf("failed to write updated config: %v", err)
	}

	if err := reloadConfig(cfgPath, proxyServer, tm); err != nil {
		t.Fatalf("reloadConfig failed: %v", err)
	}

	if len(cleanedTailnets) != 2 {
		t.Fatalf("expected 2 cleaned tailnet servers (old production + removed staging), got %v", cleanedTailnets)
	}

	// Reload 2: Attempt invalid reload with conflicting port across two tailnets; verify it fails cleanly
	invalidYAML := fmt.Sprintf(`
tailnets:
  - name: 'production'
    ports:
      - protocol: 'tcp'
        source_port: %d
        destination: '100.64.0.1:8080'
  - name: 'development'
    ports:
      - protocol: 'tcp'
        source_port: %d
        destination: '100.64.0.2:8080'
`, port1, port1)

	if err := os.WriteFile(cfgPath, []byte(invalidYAML), 0o600); err != nil {
		t.Fatalf("failed to write invalid config: %v", err)
	}

	if err := reloadConfig(cfgPath, proxyServer, tm); err == nil {
		t.Fatalf("expected reloadConfig to fail on cross-tailnet port conflict, got nil")
	}
}

func TestConfigWatcherDebouncedReloadSignal(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("initial: true\n"), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reloadChan := make(chan struct{}, 1)
	go startConfigWatcher(ctx, cfgPath, reloadChan)

	time.Sleep(100 * time.Millisecond)

	// Trigger multiple rapid writes
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(cfgPath, []byte("updated: true\n"), 0o600); err != nil {
			t.Fatalf("failed to update temp config: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case <-reloadChan:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for debounced reload signal from startConfigWatcher")
	}
}

func TestResolveConfigPath(t *testing.T) {
	t.Setenv("PROXTAIL_CONFIG", "")
	t.Setenv("TSNET_PROXY_CONFIG", "")
	if got := resolveConfigPath(""); got != "/etc/proxtail/config.yaml" {
		t.Errorf("expected default path, got %q", got)
	}

	t.Setenv("TSNET_PROXY_CONFIG", "/legacy/config.yaml")
	if got := resolveConfigPath(""); got != "/legacy/config.yaml" {
		t.Errorf("expected legacy path, got %q", got)
	}

	t.Setenv("PROXTAIL_CONFIG", "/custom/config.yaml")
	if got := resolveConfigPath(""); got != "/custom/config.yaml" {
		t.Errorf("expected custom path, got %q", got)
	}

	if got := resolveConfigPath("/flag/config.yaml"); got != "/flag/config.yaml" {
		t.Errorf("expected CLI flag path to take precedence, got %q", got)
	}
}

func TestHealthAndMetricsEndpoints(t *testing.T) {
	tm := newTailnetManager()
	tm.initFunc = func(cfg config.TailnetConfig) (*tsnet.Server, error) {
		return &tsnet.Server{Hostname: cfg.Hostname}, nil
	}
	tm.cleanupFunc = func(*tsnet.Server, string) {}

	servers, _, _, err := tm.syncTailnets([]config.TailnetConfig{
		{Name: "production", Hostname: "proxy-prod"},
		{Name: "staging", Hostname: "proxy-staging"},
	})
	if err != nil {
		t.Fatalf("syncTailnets failed: %v", err)
	}

	proxyServer := proxy.NewProxyWithMultiTailnet(nil, servers, 64*1024, 50)
	handler := newHealthHandler(proxyServer, tm)

	// 1. Verify /healthz returns JSON with active tailnets
	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recHealth := httptest.NewRecorder()
	handler.ServeHTTP(recHealth, reqHealth)

	if recHealth.Code != http.StatusOK {
		t.Fatalf("expected /healthz 200 OK, got %d", recHealth.Code)
	}
	var hr healthResponse
	if err := json.Unmarshal(recHealth.Body.Bytes(), &hr); err != nil {
		t.Fatalf("failed to decode /healthz JSON: %v", err)
	}
	if hr.Status != "ok" || len(hr.Tailnets) != 2 || hr.Tailnets[0].Name != "production" || hr.Tailnets[1].Name != "staging" {
		t.Errorf("unexpected /healthz response: %+v", hr)
	}

	// 2. Verify /metrics returns Prometheus exposition format
	reqMetrics := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	recMetrics := httptest.NewRecorder()
	handler.ServeHTTP(recMetrics, reqMetrics)

	if recMetrics.Code != http.StatusOK {
		t.Fatalf("expected /metrics 200 OK, got %d", recMetrics.Code)
	}
	body := recMetrics.Body.String()
	if !strings.Contains(body, "proxtail_tailnets_configured 2") || !strings.Contains(body, "proxtail_tcp_connections_active 0") {
		t.Errorf("unexpected /metrics output:\n%s", body)
	}

	// 3. Verify /readyz lifecycle (503 before Start when ports configured, 200 after Start, 503 after Stop)
	freePort := getFreePort(t)
	readyProxy := proxy.NewProxyWithMultiTailnet(
		[]proxy.PortMapping{{Source: freePort, ListenAddress: "127.0.0.1", Destination: "100.64.0.1:8080", Protocol: "tcp", Tailnet: "production"}},
		servers,
		64*1024,
		50,
	)
	readyHandler := newHealthHandler(readyProxy, tm)

	// Before Start: not ready (503)
	recNotReady := httptest.NewRecorder()
	readyHandler.ServeHTTP(recNotReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recNotReady.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected /readyz 503 before listeners start, got %d", recNotReady.Code)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = readyProxy.Start(ctx) }()
	defer readyProxy.Stop()

	time.Sleep(50 * time.Millisecond)

	// After Start: ready (200)
	recReady := httptest.NewRecorder()
	readyHandler.ServeHTTP(recReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recReady.Code != http.StatusOK {
		t.Fatalf("expected /readyz 200 OK after listeners start, got %d", recReady.Code)
	}

	// Verify /healthz exposes ports array
	recHealthWithPorts := httptest.NewRecorder()
	readyHandler.ServeHTTP(recHealthWithPorts, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var hrPorts healthResponse
	if err := json.Unmarshal(recHealthWithPorts.Body.Bytes(), &hrPorts); err != nil {
		t.Fatalf("failed to decode /healthz JSON with ports: %v", err)
	}
	if len(hrPorts.Ports) != 1 || hrPorts.Ports[0].Source != freePort || !hrPorts.Ports[0].Up {
		t.Errorf("expected /healthz to expose 1 active port mapping on %d, got %+v", freePort, hrPorts.Ports)
	}

	// Verify runHealthCheck against running HTTP health server and TCP listener
	healthPort := getFreePort(t)
	if err := startHealthServer(ctx, "127.0.0.1", healthPort, readyProxy, tm); err != nil {
		t.Fatalf("startHealthServer failed: %v", err)
	}
	cfgWithHealth := &config.Config{
		Proxy: config.ProxySettings{ListenAddress: "127.0.0.1", HealthPort: healthPort},
	}
	if err := runHealthCheck(cfgWithHealth); err != nil {
		t.Errorf("runHealthCheck with HealthPort failed: %v", err)
	}

	// After Stop: not ready (503)
	readyProxy.Stop()
	recStopped := httptest.NewRecorder()
	readyHandler.ServeHTTP(recStopped, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recStopped.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected /readyz 503 after Stop, got %d", recStopped.Code)
	}
}

func TestZeroPortsDiscoveryModeLifecycle(t *testing.T) {
	tm := newTailnetManager()
	tm.initFunc = func(cfg config.TailnetConfig) (*tsnet.Server, error) {
		return &tsnet.Server{Hostname: cfg.Hostname}, nil
	}
	tm.cleanupFunc = func(*tsnet.Server, string) {}

	servers, _, _, err := tm.syncTailnets([]config.TailnetConfig{
		{Name: "discovery-tailnet", Hostname: "discovery-node"},
	})
	if err != nil {
		t.Fatalf("syncTailnets failed: %v", err)
	}

	// Zero port mappings
	zeroProxy := proxy.NewProxyWithMultiTailnet(nil, servers, 64*1024, 50)
	handler := newHealthHandler(zeroProxy, tm)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = zeroProxy.Start(ctx) }()
	defer zeroProxy.Stop()

	time.Sleep(50 * time.Millisecond)

	// 1. /healthz returns 200 OK in discovery mode
	recHealth := httptest.NewRecorder()
	handler.ServeHTTP(recHealth, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recHealth.Code != http.StatusOK {
		t.Fatalf("expected /healthz 200 OK in discovery mode, got %d", recHealth.Code)
	}
	var hr healthResponse
	if err := json.Unmarshal(recHealth.Body.Bytes(), &hr); err != nil {
		t.Fatalf("failed decoding /healthz: %v", err)
	}
	if len(hr.Tailnets) != 1 || hr.Tailnets[0].Name != "discovery-tailnet" {
		t.Errorf("unexpected healthResponse tailnets: %+v", hr.Tailnets)
	}

	// 2. /readyz returns 200 OK with active_listeners: 0
	recReady := httptest.NewRecorder()
	handler.ServeHTTP(recReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recReady.Code != http.StatusOK {
		t.Fatalf("expected /readyz 200 OK with 0 ports, got %d", recReady.Code)
	}
	var rr readyResponse
	if err := json.Unmarshal(recReady.Body.Bytes(), &rr); err != nil {
		t.Fatalf("failed decoding /readyz: %v", err)
	}
	if rr.Status != "ready" || rr.ActiveListeners != 0 || rr.Tailnets != 1 {
		t.Errorf("unexpected readyResponse: %+v", rr)
	}

	// 3. runHealthCheck with HealthPort succeeds
	healthPort := getFreePort(t)
	if err := startHealthServer(ctx, "127.0.0.1", healthPort, zeroProxy, tm); err != nil {
		t.Fatalf("startHealthServer failed: %v", err)
	}
	cfgWithHealth := &config.Config{
		Proxy: config.ProxySettings{ListenAddress: "127.0.0.1", HealthPort: healthPort},
	}
	if err := runHealthCheck(cfgWithHealth); err != nil {
		t.Errorf("runHealthCheck failed: %v", err)
	}

	// 4. runHealthCheck without HealthPort and with 0 TCP listeners fails with informative error
	cfgWithoutHealth := &config.Config{
		Tailnets: []config.TailnetConfig{{Name: "discovery-tailnet"}},
	}
	if err := runHealthCheck(cfgWithoutHealth); err == nil {
		t.Errorf("expected runHealthCheck without health_port and 0 TCP listeners to fail, but succeeded")
	}
}

func TestConfigWatcherKubernetesConfigMapSymlink(t *testing.T) {
	tmpDir := t.TempDir()
	v1Dir := filepath.Join(tmpDir, "..2026_10_17_01")
	v2Dir := filepath.Join(tmpDir, "..2026_10_17_02")
	if err := os.MkdirAll(v1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(v2Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1Dir, "config.yaml"), []byte("v: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2Dir, "config.yaml"), []byte("v: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dataLink := filepath.Join(tmpDir, "..data")
	cfgLink := filepath.Join(tmpDir, "config.yaml")
	if err := os.Symlink(v1Dir, dataLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "config.yaml"), cfgLink); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reloadChan := make(chan struct{}, 1)
	go startConfigWatcher(ctx, cfgLink, reloadChan)

	time.Sleep(100 * time.Millisecond)

	// Simulate Kubernetes AtomicWriter: create ..data_tmp, rename over ..data, and remove old version dir
	tmpLink := filepath.Join(tmpDir, "..data_tmp")
	if err := os.Symlink(v2Dir, tmpLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpLink, dataLink); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(v1Dir); err != nil {
		t.Fatal(err)
	}

	select {
	case <-reloadChan:
		// Success: Kubernetes ConfigMap ..data atomic swap triggered reload
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reload signal after Kubernetes ..data symlink swap")
	}
}

func TestResolveTailnetDirReadOnlyFallback(t *testing.T) {
	// Point TS_STATE_DIR at a non-writable path (e.g. a file instead of a directory) to simulate readOnlyRootFilesystem
	tmpFile := filepath.Join(t.TempDir(), "unwritable-file")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TS_STATE_DIR", tmpFile)

	// Ephemeral mode should gracefully fall back to os.TempDir()
	dir, hasState, err := resolveTailnetDir("prod", true)
	if err != nil {
		t.Fatalf("expected ephemeral resolveTailnetDir to fall back to TempDir, got error: %v", err)
	}
	if hasState || dir == "" {
		t.Errorf("unexpected result from resolveTailnetDir: dir=%q hasState=%v", dir, hasState)
	}

	// Non-ephemeral mode must fail when TS_STATE_DIR is not writable
	if _, _, err := resolveTailnetDir("prod", false); err == nil {
		t.Errorf("expected non-ephemeral resolveTailnetDir to fail when TS_STATE_DIR is unwritable")
	}
}

func TestNetnsDisabledByDefault(t *testing.T) {
	mon, err := netmon.New(eventbus.New(), logger.Discard)
	if err != nil {
		t.Fatalf("failed to create netmon: %v", err)
	}
	defer mon.Close()

	d := &net.Dialer{}
	dialer := netns.FromDialer(logger.Discard, mon, d)
	if dialer == nil {
		t.Fatalf("expected non-nil dialer")
	}
	// By default in proxtail init(), netns is disabled, so d.Control is nil (no SO_BINDTODEVICE/SO_MARK hooks)
	if d.Control != nil {
		t.Errorf("expected d.Control to be nil when netns is disabled")
	}

	// Verify that toggling netns works and restore disabled state via Cleanup
	t.Cleanup(func() {
		netns.SetEnabled(false)
	})

	netns.SetEnabled(true)
	dEnabled := &net.Dialer{}
	_ = netns.FromDialer(logger.Discard, mon, dEnabled)
	if dEnabled.Control == nil {
		t.Errorf("expected dEnabled.Control to be non-nil when netns is enabled")
	}

	netns.SetEnabled(false)
	dDisabled := &net.Dialer{}
	_ = netns.FromDialer(logger.Discard, mon, dDisabled)
	if dDisabled.Control != nil {
		t.Errorf("expected dDisabled.Control to be nil when netns is disabled")
	}
}

type mockPeerDialer struct {
	mu    sync.Mutex
	peers map[string]bool
}

func (d *mockPeerDialer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, network, address)
}

func (d *mockPeerDialer) PeerOnlineMap(ctx context.Context) (map[string]bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := make(map[string]bool, len(d.peers))
	for k, v := range d.peers {
		cp[k] = v
	}
	return cp, nil
}

func TestHealthzAndReadyzWhenListenersSuspended(t *testing.T) {
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend listener: %v", err)
	}
	defer backendLn.Close()

	tm := newTailnetManager()
	tm.initFunc = func(cfg config.TailnetConfig) (*tsnet.Server, error) {
		return &tsnet.Server{Hostname: cfg.Hostname}, nil
	}
	tm.cleanupFunc = func(*tsnet.Server, string) {}
	if _, _, _, err := tm.syncTailnets([]config.TailnetConfig{{Name: "prod", Hostname: "proxy-prod"}}); err != nil {
		t.Fatalf("syncTailnets failed: %v", err)
	}

	dialer := &mockPeerDialer{
		peers: map[string]bool{
			"100.64.0.99": false, // offline peer
		},
	}

	portHealthy := getFreePort(t)
	portOffline := getFreePort(t)
	ports := []proxy.PortMapping{
		{
			Source:        portHealthy,
			ListenAddress: "127.0.0.1",
			Destination:   backendLn.Addr().String(),
			Protocol:      "tcp",
			Tailnet:       "prod",
		},
		{
			Source:        portOffline,
			ListenAddress: "127.0.0.1",
			Destination:   "100.64.0.99:5432",
			Protocol:      "tcp",
			Tailnet:       "prod",
		},
	}

	p := proxy.NewProxyWithDialers(ports, map[string]proxy.Dialer{"prod": dialer}, 32*1024, 50)
	p.SetProbeConfig(proxy.ProbeConfig{
		Enabled:             true,
		Interval:            40 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 1,
		Cooldown:            30 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	handler := newHealthHandler(p, tm)

	// 1. Wait for offline port to suspend while healthy port remains UP
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.ActiveListenerCount() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Partial outage (1 of 2 ports active): /readyz stays 200 OK
	recPartialReady := httptest.NewRecorder()
	handler.ServeHTTP(recPartialReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recPartialReady.Code != http.StatusOK {
		t.Fatalf("expected /readyz 200 OK when 1 of 2 ports remains active, got %d", recPartialReady.Code)
	}

	// Verify /healthz reports per-port up status and last_error
	recHealth := httptest.NewRecorder()
	handler.ServeHTTP(recHealth, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var hr healthResponse
	if err := json.Unmarshal(recHealth.Body.Bytes(), &hr); err != nil {
		t.Fatalf("failed decoding /healthz: %v", err)
	}
	if len(hr.Ports) != 2 || !hr.Ports[0].Up || hr.Ports[1].Up || !strings.Contains(hr.Ports[1].LastError, "is offline") {
		t.Fatalf("unexpected /healthz ports status during partial outage: %+v", hr.Ports)
	}

	// 2. Close remaining backend -> all ports suspended -> /readyz returns 503 Service Unavailable
	_ = backendLn.Close()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.ActiveListenerCount() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	recAllDown := httptest.NewRecorder()
	handler.ServeHTTP(recAllDown, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recAllDown.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected /readyz 503 when all configured port listeners are suspended, got %d", recAllDown.Code)
	}
}
