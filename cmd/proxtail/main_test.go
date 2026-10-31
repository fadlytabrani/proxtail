package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	var held []net.Listener
	defer func() {
		for _, ln := range held {
			_ = ln.Close()
		}
	}()
	for range 20 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to get free port: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		if port < 30000 || port > 30999 {
			_ = l.Close()
			return port
		}
		held = append(held, l)
	}
	t.Fatal("failed to allocate free port outside reserved default vip_port_range 30000-30999")
	return 0
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

func TestZeroTailnetsStandbyModeAndHotReloadLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	standbyYAML := `
tailnets: []
proxy:
  listen_address: '127.0.0.1'
  probe:
    enabled: false
`
	if err := os.WriteFile(cfgPath, []byte(standbyYAML), 0o600); err != nil {
		t.Fatalf("failed to write initial standby config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("expected standby config to load cleanly, got: %v", err)
	}

	var mu sync.Mutex
	var cleanedTailnets []string

	tm := newTailnetManager()
	tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) {
		return &tsnet.Server{Hostname: c.Hostname}, nil
	}
	tm.cleanupFunc = func(srv *tsnet.Server, name string) {
		mu.Lock()
		cleanedTailnets = append(cleanedTailnets, name+":"+srv.Hostname)
		mu.Unlock()
	}
	defer tm.closeAll()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	servers, _, _, err := tm.syncTailnets(cfg.Tailnets)
	if err != nil {
		t.Fatalf("syncTailnets with 0 tailnets failed: %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("expected 0 servers in standby mode, got %d", len(servers))
	}

	initialPorts, err := tm.prepareInitialPorts(ctx, cfg, servers)
	if err != nil {
		t.Fatalf("prepareInitialPorts in standby mode failed: %v", err)
	}

	proxyServer := proxy.NewProxyWithMultiTailnet(
		initialPorts,
		servers,
		64*1024,
		50,
	)
	proxyServer.SetProbeConfig(cfg.ToProxyProbeConfig())

	go func() { _ = proxyServer.Start(ctx) }()
	defer proxyServer.Stop()

	select {
	case <-proxyServer.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxyServer.Started() in standby mode")
	}

	handler := newHealthHandler(proxyServer, tm)

	// 1. Standby state (0 tailnets, 0 ports): /healthz and /readyz return 200 OK
	recHealth0 := httptest.NewRecorder()
	handler.ServeHTTP(recHealth0, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recHealth0.Code != http.StatusOK {
		t.Fatalf("expected /healthz 200 OK in standby mode, got %d", recHealth0.Code)
	}
	var hr0 healthResponse
	if err := json.Unmarshal(recHealth0.Body.Bytes(), &hr0); err != nil {
		t.Fatalf("failed decoding /healthz: %v", err)
	}
	if hr0.Status != "ok" || len(hr0.Tailnets) != 0 || len(hr0.Ports) != 0 {
		t.Fatalf("unexpected standby /healthz response: %+v", hr0)
	}

	recReady0 := httptest.NewRecorder()
	handler.ServeHTTP(recReady0, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recReady0.Code != http.StatusOK {
		t.Fatalf("expected /readyz 200 OK in standby mode, got %d", recReady0.Code)
	}
	var rr0 readyResponse
	if err := json.Unmarshal(recReady0.Body.Bytes(), &rr0); err != nil {
		t.Fatalf("failed decoding /readyz: %v", err)
	}
	if rr0.Status != "ready" || rr0.ActiveListeners != 0 || rr0.Tailnets != 0 {
		t.Fatalf("unexpected standby /readyz response: %+v", rr0)
	}

	// 2. Reload 0 -> 1 tailnet with a TCP port mapping
	port1 := getFreePort(t)
	activeYAML := fmt.Sprintf(`
tailnets:
  - name: 'production'
    hostname: 'prod-node'
    auto_proxy_vips: false
    ports:
      - protocol: 'tcp'
        source_port: %d
        destination: '100.64.0.1:8080'
proxy:
  listen_address: '127.0.0.1'
  probe:
    enabled: false
`, port1)
	if err := os.WriteFile(cfgPath, []byte(activeYAML), 0o600); err != nil {
		t.Fatalf("failed to write active config: %v", err)
	}
	if err := reloadConfig(cfgPath, proxyServer, tm); err != nil {
		t.Fatalf("reloadConfig (0 -> 1 tailnet) failed: %v", err)
	}

	recReady1 := httptest.NewRecorder()
	handler.ServeHTTP(recReady1, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recReady1.Code != http.StatusOK {
		t.Fatalf("expected /readyz 200 OK after reload (0 -> 1), got %d", recReady1.Code)
	}
	var rr1 readyResponse
	if err := json.Unmarshal(recReady1.Body.Bytes(), &rr1); err != nil {
		t.Fatalf("failed decoding /readyz: %v", err)
	}
	if rr1.Status != "ready" || rr1.ActiveListeners != 1 || rr1.Tailnets != 1 {
		t.Fatalf("unexpected /readyz after 0 -> 1 reload: %+v", rr1)
	}

	// 3. Reload 1 -> 0 tailnets back to standby mode
	if err := os.WriteFile(cfgPath, []byte(standbyYAML), 0o600); err != nil {
		t.Fatalf("failed to write standby config for 1 -> 0 reload: %v", err)
	}
	if err := reloadConfig(cfgPath, proxyServer, tm); err != nil {
		t.Fatalf("reloadConfig (1 -> 0 tailnets) failed: %v", err)
	}

	mu.Lock()
	cleanedCount := len(cleanedTailnets)
	mu.Unlock()
	if cleanedCount != 1 {
		t.Fatalf("expected 1 cleaned tailnet server after 1 -> 0 reload, got %v", cleanedTailnets)
	}

	recReady2 := httptest.NewRecorder()
	handler.ServeHTTP(recReady2, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recReady2.Code != http.StatusOK {
		t.Fatalf("expected /readyz 200 OK after reload back to standby (1 -> 0), got %d", recReady2.Code)
	}
	var rr2 readyResponse
	if err := json.Unmarshal(recReady2.Body.Bytes(), &rr2); err != nil {
		t.Fatalf("failed decoding /readyz: %v", err)
	}
	if rr2.Status != "ready" || rr2.ActiveListeners != 0 || rr2.Tailnets != 0 {
		t.Fatalf("unexpected /readyz after 1 -> 0 reload: %+v", rr2)
	}
}

func TestReloadConfigTransactionalRollbackPreservesRemovedTailnet(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	port1 := getFreePort(t)
	occupiedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind occupied listener: %v", err)
	}
	defer occupiedLn.Close()
	occupiedPort := occupiedLn.Addr().(*net.TCPAddr).Port

	initialYAML := fmt.Sprintf(`
tailnets:
  - name: 'alpha'
    hostname: 'alpha-node'
    auto_proxy_vips: false
    ports:
      - protocol: 'tcp'
        source_port: %d
        destination: '100.64.0.1:8080'
proxy:
  listen_address: '127.0.0.1'
  probe:
    enabled: false
`, port1)
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0o600); err != nil {
		t.Fatalf("failed writing initial config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("failed loading initial config: %v", err)
	}

	var mu sync.Mutex
	cleanedTailnets := make(map[string]int)

	tm := newTailnetManager()
	tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) {
		return &tsnet.Server{Hostname: c.Hostname}, nil
	}
	tm.cleanupFunc = func(srv *tsnet.Server, name string) {
		mu.Lock()
		cleanedTailnets[name]++
		mu.Unlock()
	}
	defer tm.closeAll()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	servers, _, _, err := tm.syncTailnets(cfg.Tailnets)
	if err != nil {
		t.Fatalf("initial syncTailnets failed: %v", err)
	}
	initialPorts, err := tm.prepareInitialPorts(ctx, cfg, servers)
	if err != nil {
		t.Fatalf("prepareInitialPorts failed: %v", err)
	}

	proxyServer := proxy.NewProxyWithMultiTailnet(initialPorts, servers, 64*1024, 50)
	proxyServer.SetProbeConfig(cfg.ToProxyProbeConfig())

	go func() { _ = proxyServer.Start(ctx) }()
	defer proxyServer.Stop()

	select {
	case <-proxyServer.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxyServer.Started()")
	}

	// Attempt to reload to a config that removes 'alpha', adds 'beta', and binds occupiedPort (which fails)
	failingYAML := fmt.Sprintf(`
tailnets:
  - name: 'beta'
    hostname: 'beta-node'
    auto_proxy_vips: false
    ports:
      - protocol: 'tcp'
        source_port: %d
        destination: '100.64.0.2:8080'
proxy:
  listen_address: '127.0.0.1'
  probe:
    enabled: false
`, occupiedPort)
	if err := os.WriteFile(cfgPath, []byte(failingYAML), 0o600); err != nil {
		t.Fatalf("failed writing failing config: %v", err)
	}

	if err := reloadConfig(cfgPath, proxyServer, tm); err == nil {
		t.Fatal("expected reloadConfig with occupied port to fail")
	}

	mu.Lock()
	alphaCleaned := cleanedTailnets["alpha"]
	betaCleaned := cleanedTailnets["beta"]
	mu.Unlock()

	if alphaCleaned != 0 {
		t.Fatalf("expected removed tailnet 'alpha' NOT to be cleaned up on rollback, got %d cleanups", alphaCleaned)
	}
	if betaCleaned != 1 {
		t.Fatalf("expected newly added tailnet 'beta' to be cleaned up on rollback, got %d cleanups", betaCleaned)
	}

	snap := tm.snapshotServers()
	if _, ok := snap["alpha"]; !ok {
		t.Fatalf("expected 'alpha' to remain in tm.servers after rollback, got %v", snap)
	}
	if _, ok := snap["beta"]; ok {
		t.Fatalf("expected 'beta' to be removed from tm.servers after rollback, got %v", snap)
	}

	if proxyServer.ActiveListenerCount() != 1 {
		t.Fatalf("expected 1 active listener on 'alpha' after rollback, got %d", proxyServer.ActiveListenerCount())
	}
}

func TestFormatVersion(t *testing.T) {
	origVersion, origBuildDate := version, buildDate
	defer func() {
		version = origVersion
		buildDate = origBuildDate
	}()

	version = "v0.3.1"
	buildDate = ""
	if got := formatVersion(); got != "proxtail v0.3.1" {
		t.Fatalf("expected 'proxtail v0.3.1', got %q", got)
	}

	buildDate = "2026-10-31"
	if got := formatVersion(); got != "proxtail v0.3.1 (2026-10-31)" {
		t.Fatalf("expected 'proxtail v0.3.1 (2026-10-31)', got %q", got)
	}
}

func TestMetricsPerPortGaugesAndPortsConfigured(t *testing.T) {
	port := getFreePort(t)
	tm := newTailnetManager()
	tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) {
		return &tsnet.Server{Hostname: c.Hostname}, nil
	}
	tm.cleanupFunc = func(*tsnet.Server, string) {}
	defer tm.closeAll()

	servers, _, _, err := tm.syncTailnets([]config.TailnetConfig{{Name: "prod", Hostname: "prod-proxy"}})
	if err != nil {
		t.Fatalf("syncTailnets failed: %v", err)
	}

	ports := []proxy.PortMapping{
		{
			Source:        port,
			Destination:   "100.64.0.1:8080",
			Protocol:      "tcp",
			Tailnet:       "prod",
			ListenAddress: "127.0.0.1",
			VIPService:    "api",
		},
	}

	p := proxy.NewProxyWithMultiTailnet(ports, servers, 32*1024, 10)
	p.SetProbeConfig(proxy.ProbeConfig{Enabled: false})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()
	<-p.Started()

	handler := newHealthHandler(p, tm)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected /metrics 200 OK, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "proxtail_ports_configured 1\n") {
		t.Errorf("expected proxtail_ports_configured 1 in /metrics, got:\n%s", body)
	}
	wantLine := fmt.Sprintf(`proxtail_port_up{tailnet="prod",protocol="tcp",listen_address="127.0.0.1",source_port="%d",destination="100.64.0.1:8080",vip_service="api"} 1`, port)
	if !strings.Contains(body, wantLine) {
		t.Errorf("expected %q in /metrics, got:\n%s", wantLine, body)
	}
}

func TestHealthzConcurrentDiscoveryAndTTLCache(t *testing.T) {
	tm := newTailnetManager()
	tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) {
		return &tsnet.Server{Hostname: c.Hostname}, nil
	}
	tm.cleanupFunc = func(*tsnet.Server, string) {}

	var callCount atomic.Int32
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	barrier := make(chan struct{})
	var barrierOnce sync.Once

	tm.healthDiscoverFunc = func(_ context.Context, _ string, _ *tsnet.Server) ([]healthPeerStatus, []healthVIPService) {
		callCount.Add(1)
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		if cur >= 2 {
			barrierOnce.Do(func() { close(barrier) })
		} else {
			select {
			case <-barrier:
			case <-time.After(300 * time.Millisecond):
			}
		}
		inFlight.Add(-1)
		return []healthPeerStatus{{HostName: "peer-1", Online: true}}, []healthVIPService{{Name: "svc-a"}}
	}
	defer tm.closeAll()

	servers, _, _, err := tm.syncTailnets([]config.TailnetConfig{
		{Name: "alpha", Hostname: "alpha-host"},
		{Name: "beta", Hostname: "beta-host"},
	})
	if err != nil {
		t.Fatalf("syncTailnets failed: %v", err)
	}

	p := proxy.NewProxyWithMultiTailnet(nil, servers, 32*1024, 10)
	handler := newHealthHandler(p, tm)

	// First /healthz request populates the 2s TTL cache and queries alpha & beta concurrently
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected /healthz 200 OK, got %d", rec1.Code)
	}
	if maxInFlight.Load() < 2 {
		t.Errorf("expected concurrent tailnet health discovery (maxInFlight >= 2), got %d", maxInFlight.Load())
	}
	if callCount.Load() != 2 {
		t.Fatalf("expected 2 discovery calls on initial /healthz, got %d", callCount.Load())
	}

	// Second /healthz request within 2s TTL serves cached tailnet details without invoking healthDiscoverFunc again
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected /healthz 200 OK, got %d", rec2.Code)
	}
	if callCount.Load() != 2 {
		t.Fatalf("expected cached /healthz to make 0 additional discovery calls (total 2), got %d", callCount.Load())
	}

	// Explicit cache invalidation forces fresh discovery on the next /healthz call
	tm.invalidateHealthCache()
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if callCount.Load() != 4 {
		t.Fatalf("expected /healthz after invalidateHealthCache to make 2 new calls (total 4), got %d", callCount.Load())
	}
}

func TestWarnNonReloadableProxyChangesOnReload(t *testing.T) {
	var logBuf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origOut)

	oldProxy := config.ProxySettings{
		WorkerPoolSize:      100,
		BufferSize:          64,
		ListenAddress:       "127.0.0.1",
		HealthListenAddress: "127.0.0.1",
		HealthPort:          9090,
	}
	newProxy := config.ProxySettings{
		WorkerPoolSize:      200,
		BufferSize:          128,
		ListenAddress:       "127.0.0.1",
		HealthListenAddress: "0.0.0.0",
		HealthPort:          9091,
	}

	warnNonReloadableProxyChanges(oldProxy, newProxy)
	out := logBuf.String()
	for _, want := range []string{
		"WARN: proxy.worker_pool_size changed (100 -> 200)",
		"WARN: proxy.buffer_size changed (64 -> 128)",
		"WARN: proxy.health_port changed (9090 -> 9091)",
		"WARN: proxy.health_listen_address changed (\"127.0.0.1\" -> \"0.0.0.0\")",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected log output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestRunHealthCheckDescriptiveErrorsAndHealthListenAddress(t *testing.T) {
	t.Run("standby mode without health_port returns actionable error", func(t *testing.T) {
		cfg, err := config.Parse([]byte(`
tailnets: []
proxy:
  health_port: 0
`))
		if err != nil {
			t.Fatal(err)
		}
		err = runHealthCheck(cfg)
		if err == nil || !strings.Contains(err.Error(), "standby mode (0 tailnets configured)") {
			t.Fatalf("expected standby mode guidance error, got: %v", err)
		}
	})

	t.Run("auto_proxy_vips-only without health_port returns actionable error", func(t *testing.T) {
		cfg, err := config.Parse([]byte(`
tailnets:
  - name: 'prod'
    auto_proxy_vips: true
proxy:
  health_port: 0
`))
		if err != nil {
			t.Fatal(err)
		}
		err = runHealthCheck(cfg)
		if err == nil || !strings.Contains(err.Error(), "only auto_proxy_vips mappings") {
			t.Fatalf("expected auto_proxy_vips guidance error, got: %v", err)
		}
	})

	t.Run("UDP-only without health_port returns actionable error", func(t *testing.T) {
		cfg, err := config.Parse([]byte(`
tailnets:
  - name: 'prod'
    auto_proxy_vips: false
    ports:
      - port: 15353
        destination: '100.64.0.1:53'
        protocol: 'udp'
proxy:
  health_port: 0
`))
		if err != nil {
			t.Fatal(err)
		}
		err = runHealthCheck(cfg)
		if err == nil || !strings.Contains(err.Error(), "UDP-only port mappings") {
			t.Fatalf("expected UDP-only guidance error, got: %v", err)
		}
	})

	t.Run("health_listen_address is used by startHealthServer and runHealthCheck", func(t *testing.T) {
		healthPort := getFreePort(t)
		cfg, err := config.Parse([]byte(fmt.Sprintf(`
tailnets: []
proxy:
  listen_address: '0.0.0.0'
  health_listen_address: '127.0.0.1'
  health_port: %d
`, healthPort)))
		if err != nil {
			t.Fatal(err)
		}

		tm := newTailnetManager()
		defer tm.closeAll()
		p := proxy.NewProxyWithMultiTailnet(nil, nil, 32*1024, 10)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = p.Start(ctx) }()
		defer p.Stop()
		<-p.Started()

		if err := startHealthServer(ctx, cfg.Proxy.EffectiveHealthListenAddress(), cfg.Proxy.HealthPort, p, tm); err != nil {
			t.Fatalf("startHealthServer failed: %v", err)
		}
		if err := runHealthCheck(cfg); err != nil {
			t.Fatalf("expected runHealthCheck to succeed against health_listen_address, got: %v", err)
		}
	})
}

