package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
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

// resolveTailnetDir determines a writable per-tailnet working directory for tsnet.Server
// and reports whether a persisted tailscaled.state file already exists.
// Even in ephemeral mode (which uses mem.Store for state), tsnet writes log configuration
// and buffer files to Server.Dir (falling back to os.UserConfigDir() if Dir is empty, which
// fails in Kubernetes when running with readOnlyRootFilesystem: true or without a HOME dir).
func resolveTailnetDir(tailnetName string, ephemeral bool) (string, bool, error) {
	stateDir := strings.TrimSpace(os.Getenv("TS_STATE_DIR"))
	if stateDir != "" {
		candidate := filepath.Join(stateDir, tailnetName)
		if !ephemeral {
			hasPersistedState := false
			if _, err := os.Stat(filepath.Join(candidate, "tailscaled.state")); err == nil {
				hasPersistedState = true
			}
			if err := os.MkdirAll(candidate, 0o700); err != nil {
				return "", false, fmt.Errorf("creating tailnet state dir %s: %w", candidate, err)
			}
			return candidate, hasPersistedState, nil
		}
		if err := os.MkdirAll(candidate, 0o700); err == nil {
			return candidate, false, nil
		}
	}

	if ephemeral {
		tmpCandidate := filepath.Join(os.TempDir(), "proxtail-tsnet", tailnetName)
		if err := os.MkdirAll(tmpCandidate, 0o700); err != nil {
			return "", false, fmt.Errorf("creating ephemeral tailnet dir %s: %w", tmpCandidate, err)
		}
		return tmpCandidate, false, nil
	}

	return "", false, nil
}

// initializeServer creates and starts a tsnet.Server for a specific tailnet configuration.
func initializeServer(tailnetCfg config.TailnetConfig) (*tsnet.Server, error) {
	ephemeral := tailnetCfg.IsEphemeral()
	tailnetDir, hasPersistedState, err := resolveTailnetDir(tailnetCfg.Name, ephemeral)
	if err != nil {
		return nil, err
	}

	authKey, clientSecret, err := tailnetCfg.ResolveCredentials(tailnetCfg.Name == "default")
	if err != nil && !hasPersistedState {
		return nil, err
	}

	server := &tsnet.Server{
		Dir:           tailnetDir,
		Hostname:      tailnetCfg.Hostname,
		AuthKey:       authKey,
		ClientSecret:  clientSecret,
		ControlURL:    tailnetCfg.ControlURL,
		Ephemeral:     ephemeral,
		AdvertiseTags: tailnetCfg.Tags,
		Port:          tailnetCfg.WireGuardPort,
		RunWebClient:  tailnetCfg.WebClient,
	}

	// Use in-memory store for ephemeral nodes so node state is never persisted across restarts.
	if ephemeral {
		server.Store = &mem.Store{}
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
// starting added tailnets in parallel, restarting modified tailnets, and returning a cleanup callback for removed ones.
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

	type initTask struct {
		cfg     config.TailnetConfig
		restart bool
	}
	var tasks []initTask

	for _, t := range tailnets {
		existingSrv, exists := tm.servers[t.Name]
		if !exists {
			tasks = append(tasks, initTask{cfg: t, restart: false})
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
			tasks = append(tasks, initTask{cfg: t, restart: true})
		}
	}

	if len(tasks) > 0 {
		type initResult struct {
			name    string
			srv     *tsnet.Server
			restart bool
			err     error
		}
		results := make([]initResult, len(tasks))
		var wg sync.WaitGroup
		for i, task := range tasks {
			idx, tk := i, task
			wg.Go(func() {
				srv, err := tm.initFunc(tk.cfg)
				results[idx] = initResult{
					name:    tk.cfg.Name,
					srv:     srv,
					restart: tk.restart,
					err:     err,
				}
			})
		}
		wg.Wait()

		var firstErr error
		for _, res := range results {
			if res.err != nil {
				if firstErr == nil {
					if res.restart {
						firstErr = fmt.Errorf("failed to reinitialize tailnet '%s': %w", res.name, res.err)
					} else {
						firstErr = fmt.Errorf("failed to initialize tailnet '%s': %w", res.name, res.err)
					}
				}
				continue
			}
			addedServers[res.name] = res.srv
			if res.restart {
				restartedTailnets[res.name] = true
			}
		}

		if firstErr != nil {
			for name, started := range addedServers {
				tm.cleanupFunc(started, name)
			}
			return nil, nil, nil, firstErr
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

	snapshot := maps.Clone(tm.servers)

	cleanupRemoved := func() {
		for name, srv := range removedServers {
			tm.cleanupFunc(srv, name)
		}
	}

	return snapshot, restartedTailnets, cleanupRemoved, nil
}

// snapshotServers returns a thread-safe copy of the currently active tailnet servers.
func (tm *tailnetManager) snapshotServers() map[string]*tsnet.Server {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return maps.Clone(tm.servers)
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
			// Trigger reload on direct config file modifications or Kubernetes ConfigMap symlink swaps ("..data")
			base := filepath.Base(event.Name)
			if (base == configBase || base == "..data") &&
				event.Has(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) {
				log.Printf("Config file change detected (%s on %s), scheduling reload...", event.Op, base)
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

// healthTailnetStatus describes the runtime status of a single tailnet for /healthz.
type healthTailnetStatus struct {
	Name     string `json:"name"`
	Hostname string `json:"hostname,omitempty"`
	IPv4     string `json:"ipv4,omitempty"`
	IPv6     string `json:"ipv6,omitempty"`
}

// healthResponse is the JSON payload returned by GET /healthz.
type healthResponse struct {
	Status           string                `json:"status"`
	Tailnets         []healthTailnetStatus `json:"tailnets"`
	TCPConnections   int64                 `json:"tcp_connections"`
	UDPPackets       int64                 `json:"udp_packets"`
	BytesTransferred int64                 `json:"bytes_transferred"`
	Errors           int64                 `json:"errors"`
}

// readyResponse is the JSON payload returned by GET /readyz.
type readyResponse struct {
	Status          string `json:"status"`
	ActiveListeners int    `json:"active_listeners"`
	Tailnets        int    `json:"tailnets"`
}

// newHealthHandler constructs an http.Handler serving /healthz (liveness), /readyz (readiness), and /metrics (Prometheus).
func newHealthHandler(proxyServer *proxy.Proxy, tm *tailnetManager) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		stats := proxyServer.GetStats()
		servers := tm.snapshotServers()

		names := make([]string, 0, len(servers))
		for name := range servers {
			names = append(names, name)
		}
		sort.Strings(names)

		tailnets := make([]healthTailnetStatus, 0, len(names))
		for _, name := range names {
			st := healthTailnetStatus{Name: name}
			if srv := servers[name]; srv != nil {
				st.Hostname = srv.Hostname
				if srv.Sys() != nil {
					ip4, ip6 := srv.TailscaleIPs()
					if ip4.IsValid() {
						st.IPv4 = ip4.String()
					}
					if ip6.IsValid() {
						st.IPv6 = ip6.String()
					}
				}
			}
			tailnets = append(tailnets, st)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(healthResponse{
			Status:           "ok",
			Tailnets:         tailnets,
			TCPConnections:   stats.TCPConnections,
			UDPPackets:       stats.UDPPackets,
			BytesTransferred: stats.BytesTransferred,
			Errors:           stats.ErrorCount,
		})
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		servers := tm.snapshotServers()
		activeListeners := proxyServer.ActiveListenerCount()
		ready := proxyServer.IsReady() && (len(proxyServer.Ports) == 0 || len(servers) > 0)

		w.Header().Set("Content-Type", "application/json")
		status := "ready"
		if !ready {
			status = "not_ready"
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(readyResponse{
			Status:          status,
			ActiveListeners: activeListeners,
			Tailnets:        len(servers),
		})
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		stats := proxyServer.GetStats()
		servers := tm.snapshotServers()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var b strings.Builder
		fmt.Fprintf(&b, "# HELP proxtail_tcp_connections_active Current number of active TCP connections.\n")
		fmt.Fprintf(&b, "# TYPE proxtail_tcp_connections_active gauge\n")
		fmt.Fprintf(&b, "proxtail_tcp_connections_active %d\n", stats.TCPConnections)
		fmt.Fprintf(&b, "# HELP proxtail_udp_packets_total Total number of UDP packets forwarded.\n")
		fmt.Fprintf(&b, "# TYPE proxtail_udp_packets_total counter\n")
		fmt.Fprintf(&b, "proxtail_udp_packets_total %d\n", stats.UDPPackets)
		fmt.Fprintf(&b, "# HELP proxtail_bytes_transferred_total Total number of bytes transferred across all flows.\n")
		fmt.Fprintf(&b, "# TYPE proxtail_bytes_transferred_total counter\n")
		fmt.Fprintf(&b, "proxtail_bytes_transferred_total %d\n", stats.BytesTransferred)
		fmt.Fprintf(&b, "# HELP proxtail_errors_total Total number of connection or forwarding errors.\n")
		fmt.Fprintf(&b, "# TYPE proxtail_errors_total counter\n")
		fmt.Fprintf(&b, "proxtail_errors_total %d\n", stats.ErrorCount)
		fmt.Fprintf(&b, "# HELP proxtail_tailnets_configured Number of currently active tailnet instances.\n")
		fmt.Fprintf(&b, "# TYPE proxtail_tailnets_configured gauge\n")
		fmt.Fprintf(&b, "proxtail_tailnets_configured %d\n", len(servers))
		fmt.Fprintf(&b, "# HELP proxtail_listeners_active Number of currently bound local TCP and UDP listeners.\n")
		fmt.Fprintf(&b, "# TYPE proxtail_listeners_active gauge\n")
		fmt.Fprintf(&b, "proxtail_listeners_active %d\n", proxyServer.ActiveListenerCount())
		_, _ = w.Write([]byte(b.String()))
	})

	return mux
}

// startHealthServer starts the optional HTTP health check and Prometheus metrics server.
func startHealthServer(ctx context.Context, listenAddress string, port int, proxyServer *proxy.Proxy, tm *tailnetManager) error {
	addr := fmt.Sprintf(":%d", port)
	if strings.TrimSpace(listenAddress) != "" {
		addr = net.JoinHostPort(strings.TrimSpace(listenAddress), strconv.Itoa(port))
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("starting health server on %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler:           newHealthHandler(proxyServer, tm),
		ReadHeaderTimeout: 5 * time.Second,
	}

	context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	go func() {
		log.Printf("Health & metrics server listening on %s (/healthz, /readyz, /metrics)", addr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Health server error: %v", err)
		}
	}()

	return nil
}

// runHealthCheck probes the running proxtail instance via its configured health port or TCP listeners.
func runHealthCheck(cfg *config.Config) error {
	probeHost := strings.TrimSpace(cfg.Proxy.ListenAddress)
	if probeHost == "" || probeHost == "0.0.0.0" || probeHost == "::" {
		probeHost = "127.0.0.1"
	}

	if cfg.Proxy.HealthPort > 0 {
		url := fmt.Sprintf("http://%s/readyz", net.JoinHostPort(probeHost, strconv.Itoa(cfg.Proxy.HealthPort)))
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			return fmt.Errorf("health check request to %s failed: %w", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("health check endpoint %s returned status %d", url, resp.StatusCode)
		}
		return nil
	}

	// If health_port is not enabled, verify that at least one configured TCP listener is accepting connections.
	for _, mapping := range cfg.ToProxyPortMappings() {
		if mapping.Protocol == "tcp" {
			host := strings.TrimSpace(mapping.ListenAddress)
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			target := net.JoinHostPort(host, strconv.Itoa(mapping.Source))
			conn, err := net.DialTimeout("tcp", target, 2*time.Second)
			if err != nil {
				return fmt.Errorf("TCP listener probe on %s failed: %w", target, err)
			}
			_ = conn.Close()
			return nil
		}
	}

	return nil
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
	var flagValidate bool
	var flagHealthCheck bool
	flag.StringVar(&flagConfig, "config", "", "Path to configuration file")
	flag.StringVar(&flagConfig, "c", "", "Path to configuration file (shorthand)")
	flag.BoolVar(&flagValidate, "validate", false, "Validate configuration file and exit")
	flag.BoolVar(&flagValidate, "check", false, "Validate configuration file and exit (shorthand)")
	flag.BoolVar(&flagHealthCheck, "healthcheck", false, "Probe running proxtail instance health and exit")
	flag.Parse()

	configFile := resolveConfigPath(flagConfig)

	cfg, err := config.Load(configFile)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v", configFile, err)
	}

	if flagHealthCheck {
		if err := runHealthCheck(cfg); err != nil {
			log.Fatalf("Health check failed: %v", err)
		}
		return
	}

	portMappings := cfg.ToProxyPortMappings()
	if flagValidate {
		fmt.Printf("Configuration %s is valid (%d tailnets, %d port mappings)\n",
			configFile, len(cfg.Tailnets), len(portMappings))
		return
	}

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

	if cfg.Proxy.HealthPort > 0 {
		if err := startHealthServer(ctx, cfg.Proxy.ListenAddress, cfg.Proxy.HealthPort, proxyServer, tm); err != nil {
			log.Fatalf("Failed to start health server: %v", err)
		}
	}

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
