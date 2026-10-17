package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tailscale.com/tsnet"

	"proxtail/internal/config"
	"proxtail/internal/proxy"
)

func TestTailnetManagerSyncTailnets(t *testing.T) {
	var initialized []string
	var cleaned []string

	tm := newTailnetManager()
	tm.initFunc = func(cfg config.TailnetConfig) (*tsnet.Server, error) {
		initialized = append(initialized, cfg.Name+":"+cfg.Hostname)
		return &tsnet.Server{Hostname: cfg.Hostname}, nil
	}
	tm.cleanupFunc = func(srv *tsnet.Server, name string) {
		cleaned = append(cleaned, name+":"+srv.Hostname)
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
