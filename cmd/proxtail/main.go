package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/tsnet"

	"proxtail/internal/config"
	"proxtail/internal/proxy"
)

// tailnetManager manages the lifecycle of multiple tsnet.Server instances.
type tailnetManager struct {
	mu      sync.Mutex
	servers map[string]*tsnet.Server
	configs map[string]config.TailnetConfig
}

func newTailnetManager() *tailnetManager {
	return &tailnetManager{
		servers: make(map[string]*tsnet.Server),
		configs: make(map[string]config.TailnetConfig),
	}
}

// initializeServer creates and starts a tsnet.Server for a specific tailnet configuration.
func initializeServer(tailnetCfg config.TailnetConfig) (*tsnet.Server, error) {
	authKeyEnvVar := tailnetCfg.AuthKey
	if authKeyEnvVar == "" {
		authKeyEnvVar = fmt.Sprintf("TS_AUTHKEY_%s", strings.ToUpper(tailnetCfg.Name))
	}

	authKey := os.Getenv(authKeyEnvVar)
	if authKey == "" {
		return nil, fmt.Errorf("environment variable %s not set", authKeyEnvVar)
	}

	ephemeral := tailnetCfg.IsEphemeral()
	server := &tsnet.Server{
		Hostname:  tailnetCfg.Hostname,
		AuthKey:   authKey,
		Ephemeral: ephemeral,
	}

	// Use in-memory store for ephemeral nodes, or persistent state directory for non-ephemeral nodes.
	stateDir := os.Getenv("TS_STATE_DIR")
	if ephemeral {
		server.Store = &mem.Store{}
	} else if stateDir != "" {
		tailnetDir := filepath.Join(stateDir, tailnetCfg.Name)
		if err := os.MkdirAll(tailnetDir, 0o700); err != nil {
			return nil, fmt.Errorf("creating tailnet state dir %s: %w", tailnetDir, err)
		}
		server.Dir = tailnetDir
	}

	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("failed to start tsnet server: %w", err)
	}

	log.Printf("Tailnet '%s' server started successfully (hostname: %s, ephemeral: %v)",
		tailnetCfg.Name, tailnetCfg.Hostname, ephemeral)
	return server, nil
}

// cleanupTailnetServer performs thorough cleanup of a Tailscale server.
func cleanupTailnetServer(server *tsnet.Server, tailnetName string) {
	log.Printf("Cleaning up tailnet connection: %s", tailnetName)
	if err := server.Close(); err != nil {
		log.Printf("Warning: error closing tailnet server %s: %v", tailnetName, err)
	}
	time.Sleep(100 * time.Millisecond)
	log.Printf("Tailnet connection %s cleaned up", tailnetName)
}

// syncTailnets reconciles running tsnet.Server instances with the provided configuration.
func (tm *tailnetManager) syncTailnets(tailnets []config.TailnetConfig) (map[string]*tsnet.Server, func(), error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	newConfigs := make(map[string]config.TailnetConfig, len(tailnets))
	for _, t := range tailnets {
		newConfigs[t.Name] = t
	}

	// Start any newly added tailnets first before tearing down removed ones.
	addedServers := make(map[string]*tsnet.Server)
	for _, t := range tailnets {
		if _, exists := tm.servers[t.Name]; !exists {
			srv, err := initializeServer(t)
			if err != nil {
				for name, started := range addedServers {
					cleanupTailnetServer(started, name)
				}
				return nil, nil, fmt.Errorf("failed to initialize tailnet '%s': %w", t.Name, err)
			}
			addedServers[t.Name] = srv
		}
	}

	for name, srv := range addedServers {
		tm.servers[name] = srv
		tm.configs[name] = newConfigs[name]
	}

	// Identify removed tailnets so they can be closed after listeners stop using them.
	removedServers := make(map[string]*tsnet.Server)
	for name, srv := range tm.servers {
		if _, exists := newConfigs[name]; !exists {
			removedServers[name] = srv
			delete(tm.servers, name)
			delete(tm.configs, name)
		}
	}

	snapshot := make(map[string]*tsnet.Server, len(tm.servers))
	for name, srv := range tm.servers {
		snapshot[name] = srv
	}

	cleanupRemoved := func() {
		for name, srv := range removedServers {
			cleanupTailnetServer(srv, name)
		}
	}

	return snapshot, cleanupRemoved, nil
}

// closeAll shuts down all managed tsnet.Server instances.
func (tm *tailnetManager) closeAll() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	log.Println("Starting cleanup of tailnet servers...")
	for name, srv := range tm.servers {
		cleanupTailnetServer(srv, name)
		delete(tm.servers, name)
		delete(tm.configs, name)
	}
	log.Println("All tailnet servers cleaned up")
}

// reloadConfig loads and validates the updated configuration and applies it to the running proxy.
func reloadConfig(configFile string, proxyServer *proxy.Proxy, tm *tailnetManager) error {
	newCfg, err := config.Load(configFile)
	if err != nil {
		return fmt.Errorf("failed to load new config: %w", err)
	}

	newPortMappings := newCfg.ToProxyPortMappings()

	servers, cleanupRemoved, err := tm.syncTailnets(newCfg.Tailnets)
	if err != nil {
		return err
	}
	defer cleanupRemoved()

	for _, mapping := range newPortMappings {
		if _, exists := servers[mapping.Tailnet]; !exists {
			return fmt.Errorf("port mapping for port %d references unknown tailnet '%s'", mapping.Source, mapping.Tailnet)
		}
	}

	proxyServer.UpdateTailnetServers(servers)

	if err := proxyServer.ReloadConfig(newPortMappings); err != nil {
		return fmt.Errorf("failed to apply new config: %w", err)
	}

	return nil
}

// startConfigWatcher watches the configuration file's directory for updates and triggers reloads.
func startConfigWatcher(ctx context.Context, configFile string, proxyServer *proxy.Proxy, tm *tailnetManager) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Warning: Failed to create config watcher: %v", err)
		return
	}
	defer watcher.Close()

	cleanConfigFile := filepath.Clean(configFile)
	configDir := filepath.Dir(cleanConfigFile)
	configBase := filepath.Base(cleanConfigFile)

	if err := watcher.Add(configDir); err != nil {
		log.Printf("Warning: Failed to watch config directory %s: %v", configDir, err)
		return
	}

	log.Printf("Watching for config changes in: %s", configDir)

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) == configBase &&
				(event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0) {
				log.Printf("Config file changed (%s), reloading configuration...", event.Op)
				time.Sleep(100 * time.Millisecond)

				if err := reloadConfig(cleanConfigFile, proxyServer, tm); err != nil {
					log.Printf("Failed to reload config: %v", err)
				} else {
					log.Printf("Configuration reloaded successfully")
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Config watcher error: %v", err)
		}
	}
}

// resolveConfigPath determines the configuration file path from environment variables or defaults.
func resolveConfigPath() string {
	if p := os.Getenv("PROXTAIL_CONFIG"); p != "" {
		return p
	}
	if p := os.Getenv("TSNET_PROXY_CONFIG"); p != "" {
		return p
	}
	return "/etc/proxtail/config.yaml"
}

func main() {
	configFile := resolveConfigPath()

	cfg, err := config.Load(configFile)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v", configFile, err)
	}

	portMappings := cfg.ToProxyPortMappings()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)

	shutdown := make(chan struct{})
	reloadChan := make(chan struct{}, 1)

	go func() {
		for {
			sig := <-sigs
			switch sig {
			case syscall.SIGHUP:
				log.Printf("Received SIGHUP signal, triggering config reload...")
				select {
				case reloadChan <- struct{}{}:
					log.Printf("Config reload signal sent")
				default:
					log.Printf("Config reload already in progress, skipping")
				}
			case syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT:
				log.Printf("Received %s signal, initiating graceful shutdown...", sig)
				close(shutdown)
				cancel()
				return
			}
		}
	}()

	tm := newTailnetManager()
	defer tm.closeAll()

	tailnetServers, _, err := tm.syncTailnets(cfg.Tailnets)
	if err != nil {
		log.Fatalf("Failed to initialize tailnets: %v", err)
	}

	for _, mapping := range portMappings {
		if _, exists := tailnetServers[mapping.Tailnet]; !exists {
			log.Fatalf("Port mapping for port %d references unknown tailnet '%s'", mapping.Source, mapping.Tailnet)
		}
	}

	proxyServer := proxy.NewProxyWithMultiTailnet(
		portMappings,
		tailnetServers,
		cfg.Proxy.BufferSize*1024,
		cfg.Proxy.QueueSize,
		cfg.Proxy.WorkerPoolSize,
	)

	go startConfigWatcher(ctx, configFile, proxyServer, tm)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloadChan:
				log.Printf("Processing SIGHUP-triggered config reload...")
				if err := reloadConfig(configFile, proxyServer, tm); err != nil {
					log.Printf("Failed to reload config via SIGHUP: %v", err)
				} else {
					log.Printf("Configuration reloaded successfully via SIGHUP")
				}
			}
		}
	}()

	if cfg.Proxy.EnableMetrics {
		go func() {
			ticker := time.NewTicker(time.Duration(cfg.Proxy.MetricsInterval) * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					stats := proxyServer.GetStats()
					log.Printf("METRICS: TCP_Connections=%d, UDP_Packets=%d, Bytes_Transferred=%d, Errors=%d",
						stats.TCPConnections, stats.UDPPackets, stats.BytesTransferred, stats.ErrorCount)
				}
			}
		}()
	}

	fmt.Printf("Starting proxtail with %d tailnets, %d workers, %dKB buffers, queue size %d...\n",
		len(tailnetServers), cfg.Proxy.WorkerPoolSize, cfg.Proxy.BufferSize, cfg.Proxy.QueueSize)
	fmt.Printf("Hot reload enabled - watching for config changes in: %s\n", configFile)
	fmt.Printf("Manual reload available - send SIGHUP signal to trigger config reload\n")

	go func() {
		if err := proxyServer.Start(ctx); err != nil {
			log.Printf("Error starting proxy: %v", err)
		}
	}()

	select {
	case <-shutdown:
		log.Println("Shutdown signal received, waiting for proxy to stop...")
	case <-ctx.Done():
		log.Println("Context cancelled, waiting for proxy to stop...")
	}

	log.Println("Waiting for proxy to shut down gracefully...")
	time.Sleep(3 * time.Second)

	log.Println("Stopping proxy server...")
	proxyServer.Stop()

	log.Println("Waiting for Tailscale cleanup...")
	time.Sleep(2 * time.Second)

	log.Println("Proxy shutdown complete")
}
