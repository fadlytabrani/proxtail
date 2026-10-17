package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
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
	mu          sync.Mutex
	reloadMu    sync.Mutex
	servers     map[string]*tsnet.Server
	configs     map[string]config.TailnetConfig
	initFunc    func(config.TailnetConfig) (*tsnet.Server, error)
	cleanupFunc func(*tsnet.Server, string)
}

func newTailnetManager() *tailnetManager {
	return &tailnetManager{
		servers:     make(map[string]*tsnet.Server),
		configs:     make(map[string]config.TailnetConfig),
		initFunc:    initializeServer,
		cleanupFunc: cleanupTailnetServer,
	}
}

// initializeServer creates and starts a tsnet.Server for a specific tailnet configuration.
func initializeServer(tailnetCfg config.TailnetConfig) (*tsnet.Server, error) {
	authKey, err := tailnetCfg.ResolveAuthKey(tailnetCfg.Name == "default")
	if err != nil {
		return nil, err
	}

	ephemeral := tailnetCfg.IsEphemeral()
	server := &tsnet.Server{
		Hostname:   tailnetCfg.Hostname,
		AuthKey:    authKey,
		ControlURL: tailnetCfg.ControlURL,
		Ephemeral:  ephemeral,
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
	if server != nil {
		if err := server.Close(); err != nil {
			log.Printf("Warning: error closing tailnet server %s: %v", tailnetName, err)
		}
	}
	log.Printf("Tailnet connection %s cleaned up", tailnetName)
}

// syncTailnets reconciles running tsnet.Server instances with the provided configuration,
// starting added tailnets, restarting modified tailnets, and returning a cleanup callback for removed ones.
func (tm *tailnetManager) syncTailnets(tailnets []config.TailnetConfig) (map[string]*tsnet.Server, map[string]bool, func(), error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	newConfigs := make(map[string]config.TailnetConfig, len(tailnets))
	for _, t := range tailnets {
		newConfigs[t.Name] = t
	}

	addedServers := make(map[string]*tsnet.Server)
	restartedTailnets := make(map[string]bool)
	removedServers := make(map[string]*tsnet.Server)

	for _, t := range tailnets {
		existingSrv, exists := tm.servers[t.Name]
		if !exists {
			srv, err := tm.initFunc(t)
			if err != nil {
				for name, started := range addedServers {
					tm.cleanupFunc(started, name)
				}
				return nil, nil, nil, fmt.Errorf("failed to initialize tailnet '%s': %w", t.Name, err)
			}
			addedServers[t.Name] = srv
			continue
		}

		oldCfg := tm.configs[t.Name]
		if !oldCfg.ConnectionSettingsEqual(t) {
			log.Printf("Tailnet '%s' connection settings changed, reinitializing...", t.Name)
			// If non-ephemeral, close the existing server first to release any state dir lock.
			if !oldCfg.IsEphemeral() || !t.IsEphemeral() {
				tm.cleanupFunc(existingSrv, t.Name)
				delete(tm.servers, t.Name)
				delete(tm.configs, t.Name)
			} else {
				removedServers[t.Name] = existingSrv
			}

			srv, err := tm.initFunc(t)
			if err != nil {
				for name, started := range addedServers {
					tm.cleanupFunc(started, name)
				}
				return nil, nil, nil, fmt.Errorf("failed to reinitialize tailnet '%s': %w", t.Name, err)
			}
			addedServers[t.Name] = srv
			restartedTailnets[t.Name] = true
		}
	}

	for name, srv := range addedServers {
		tm.servers[name] = srv
		tm.configs[name] = newConfigs[name]
	}

	// Identify removed tailnets so they can be closed after listeners stop using them.
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
			tm.cleanupFunc(srv, name)
		}
	}

	return snapshot, restartedTailnets, cleanupRemoved, nil
}

// closeAll shuts down all managed tsnet.Server instances.
func (tm *tailnetManager) closeAll() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	log.Println("Starting cleanup of tailnet servers...")
	for name, srv := range tm.servers {
		tm.cleanupFunc(srv, name)
		delete(tm.servers, name)
		delete(tm.configs, name)
	}
	log.Println("All tailnet servers cleaned up")
}

// reloadConfig loads and validates the updated configuration and applies it to the running proxy.
func reloadConfig(configFile string, proxyServer *proxy.Proxy, tm *tailnetManager) error {
	tm.reloadMu.Lock()
	defer tm.reloadMu.Unlock()

	newCfg, err := config.Load(configFile)
	if err != nil {
		return fmt.Errorf("failed to load new config: %w", err)
	}

	newPortMappings := newCfg.ToProxyPortMappings()

	servers, restartedTailnets, cleanupRemoved, err := tm.syncTailnets(newCfg.Tailnets)
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

	if len(restartedTailnets) > 0 {
		if err := proxyServer.RestartTailnetPorts(restartedTailnets); err != nil {
			return fmt.Errorf("failed to restart ports for updated tailnets: %w", err)
		}
	}

	return nil
}

// startConfigWatcher watches the configuration file's directory for updates and signals reloadChan.
func startConfigWatcher(ctx context.Context, configFile string, reloadChan chan<- struct{}) {
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

	var debounceTimer *time.Timer
	var debounceC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) == configBase &&
				(event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0) {
				log.Printf("Config file change detected (%s), scheduling reload...", event.Op)
				if debounceTimer == nil {
					debounceTimer = time.NewTimer(100 * time.Millisecond)
					debounceC = debounceTimer.C
				} else {
					if !debounceTimer.Stop() {
						select {
						case <-debounceTimer.C:
						default:
						}
					}
					debounceTimer.Reset(100 * time.Millisecond)
				}
			}
		case <-debounceC:
			debounceC = nil
			select {
			case reloadChan <- struct{}{}:
			default:
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Config watcher error: %v", err)
		}
	}
}

// resolveConfigPath determines the configuration file path from CLI flag, environment variables,
// a local ./config.yaml in the current working directory, or /etc/proxtail/config.yaml.
func resolveConfigPath(flagConfig string) string {
	if flagConfig != "" {
		return flagConfig
	}
	if p := os.Getenv("PROXTAIL_CONFIG"); p != "" {
		return p
	}
	if p := os.Getenv("TSNET_PROXY_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}
	return "/etc/proxtail/config.yaml"
}

func main() {
	var flagConfig string
	flag.StringVar(&flagConfig, "config", "", "Path to configuration file")
	flag.StringVar(&flagConfig, "c", "", "Path to configuration file (shorthand)")
	flag.Parse()

	configFile := resolveConfigPath(flagConfig)

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
					log.Printf("Config reload signal queued")
				default:
					log.Printf("Config reload already queued, skipping duplicate signal")
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

	tailnetServers, _, _, err := tm.syncTailnets(cfg.Tailnets)
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
		cfg.Proxy.WorkerPoolSize,
	)

	go startConfigWatcher(ctx, configFile, reloadChan)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloadChan:
				log.Printf("Processing configuration reload...")
				if err := reloadConfig(configFile, proxyServer, tm); err != nil {
					log.Printf("Failed to reload config: %v", err)
				} else {
					log.Printf("Configuration reloaded successfully")
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

	fmt.Printf("Starting proxtail with %d tailnets, %d workers, %dKB buffers...\n",
		len(tailnetServers), cfg.Proxy.WorkerPoolSize, cfg.Proxy.BufferSize)
	fmt.Printf("Hot reload enabled - watching for config changes in: %s\n", configFile)
	fmt.Printf("Manual reload available - send SIGHUP signal to trigger config reload\n")

	proxyErrChan := make(chan error, 1)
	go func() {
		proxyErrChan <- proxyServer.Start(ctx)
	}()

	var startupErr error
	select {
	case startupErr = <-proxyErrChan:
		if startupErr != nil {
			log.Printf("Proxy server exited with error: %v", startupErr)
			cancel()
		}
	case <-shutdown:
		log.Println("Shutdown signal received, stopping proxy...")
	case <-ctx.Done():
		log.Println("Context cancelled, stopping proxy...")
	}

	log.Println("Stopping proxy server...")
	proxyServer.Stop()

	if startupErr == nil {
		if err := <-proxyErrChan; err != nil {
			startupErr = err
		}
	}

	if startupErr != nil {
		tm.closeAll()
		os.Exit(1)
	}

	log.Println("Proxy shutdown complete")
}
