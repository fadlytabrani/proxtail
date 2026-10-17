package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tailscale.com/tsnet"

	"proxtail/internal/config"
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
	if got := resolveConfigPath(); got != "/etc/proxtail/config.yaml" {
		t.Errorf("expected default path, got %q", got)
	}

	t.Setenv("TSNET_PROXY_CONFIG", "/legacy/config.yaml")
	if got := resolveConfigPath(); got != "/legacy/config.yaml" {
		t.Errorf("expected legacy path, got %q", got)
	}

	t.Setenv("PROXTAIL_CONFIG", "/custom/config.yaml")
	if got := resolveConfigPath(); got != "/custom/config.yaml" {
		t.Errorf("expected custom path, got %q", got)
	}
}
