package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	_ "tailscale.com/feature/oauthkey"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"

	"proxtail/internal/config"
	"proxtail/internal/proxy"
)

func init() {
	// proxtail operates purely in userspace (tsnet + gVisor netstack) without
	// a host TUN interface or kernel routing table modifications. Disable netns
	// by default to prevent failed SO_BINDTODEVICE / SO_MARK syscalls and noisy
	// warnings when running as an unprivileged process (e.g., UID 10001).
	if os.Getenv("PROXTAIL_NETNS_ENABLED") != "true" {
		netns.SetEnabled(false)
	}
}

// tailnetManager manages the lifecycle of multiple tsnet.Server instances.
type tailnetManager struct {
	mu              sync.Mutex
	reloadMu        sync.Mutex
	servers         map[string]*tsnet.Server
	configs         map[string]config.TailnetConfig
	initErrors      map[string]string
	initFunc        func(config.TailnetConfig) (*tsnet.Server, error)
	cleanupFunc     func(*tsnet.Server, string)
	dialerForServer func(string, *tsnet.Server) proxy.Dialer

	vipDiscoverFunc vipDiscoverFunc
	activeCfg       *config.Config
	appliedPorts    []proxy.PortMapping
	vipAllocations  map[vipAllocKey]int
	lastTailnetVIPs map[string]map[tailcfg.ServiceName]tailcfg.ServiceDetails

	vipWatchMu         sync.Mutex
	reconcilerCtx      context.Context
	reconcilerCancel   context.CancelFunc
	reconcilerProxy    *proxy.Proxy
	vipTriggerCh       chan struct{}
	vipTickerCh        chan time.Duration
	vipWatchers        map[string]vipWatcherEntry
	vipInterval        time.Duration
	vipRefreshOverride time.Duration
	vipDebounceDelay   time.Duration
	ipnBusWatchFunc    ipnBusWatchFunc

	healthCacheMu      sync.Mutex
	healthCache        map[string]cachedTailnetHealth
	healthDiscoverFunc func(context.Context, string, *tsnet.Server) tailnetHealthDetails
}

func newTailnetManager() *tailnetManager {
	return &tailnetManager{
		servers:         make(map[string]*tsnet.Server),
		configs:         make(map[string]config.TailnetConfig),
		initErrors:      make(map[string]string),
		initFunc:        initializeServer,
		cleanupFunc:     cleanupTailnetServer,
		vipDiscoverFunc: defaultDiscoverVIPServices,
		vipAllocations:  make(map[vipAllocKey]int),
		lastTailnetVIPs: make(map[string]map[tailcfg.ServiceName]tailcfg.ServiceDetails),
		vipWatchers:     make(map[string]vipWatcherEntry),
		healthCache:     make(map[string]cachedTailnetHealth),
	}
}

// resolveTailnetDir determines a writable per-tailnet working directory for tsnet.Server
// and reports whether a persisted tailscaled.state file already exists.
// Even in ephemeral mode (which uses mem.Store for state), tsnet writes log configuration
// and buffer files to Server.Dir (falling back to os.UserConfigDir() if Dir is empty, which
// fails in Kubernetes when running with readOnlyRootFilesystem: true or without a HOME dir).
func resolveTailnetDir(tailnetName string, ephemeral bool, configuredStateDir ...string) (string, bool, error) {
	var stateDir string
	if len(configuredStateDir) > 0 {
		stateDir = strings.TrimSpace(configuredStateDir[0])
	}
	if stateDir == "" {
		stateDir = strings.TrimSpace(os.Getenv("PROXTAIL_STATE_DIR"))
	}
	if stateDir == "" {
		stateDir = strings.TrimSpace(os.Getenv("TS_STATE_DIR"))
	}
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

// resolveOAuthTags attempts to auto-discover required ACL tags from the Tailscale OAuth endpoint
// if no explicit tags were provided in configuration.
func resolveOAuthTags(clientSecret, controlURL string) []string {
	if !strings.HasPrefix(clientSecret, "tskey-client-") {
		return nil
	}
	baseURL := "https://api.tailscale.com"
	if controlURL != "" {
		baseURL = strings.TrimRight(controlURL, "/")
	}
	tokenURL := baseURL + "/api/v2/oauth/token"

	rawSecret, _, _ := strings.Cut(clientSecret, "?")
	data := url.Values{}
	data.Set("client_id", "proxtail")
	data.Set("client_secret", rawSecret)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result struct {
		Tags string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.Tags == "" {
		return nil
	}

	rawTags := strings.Split(result.Tags, ",")
	var tags []string
	for _, t := range rawTags {
		t = strings.TrimSpace(t)
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// initializeServer creates and starts a tsnet.Server for a specific tailnet configuration.
func initializeServer(tailnetCfg config.TailnetConfig) (*tsnet.Server, error) {
	ephemeral := tailnetCfg.IsEphemeral()
	tailnetDir, hasPersistedState, err := resolveTailnetDir(tailnetCfg.Profile, ephemeral, tailnetCfg.StateDir)
	if err != nil {
		return nil, err
	}

	authKey, clientSecret, err := tailnetCfg.ResolveCredentials(tailnetCfg.Profile == "default")
	if err != nil && !hasPersistedState {
		return nil, err
	}

	tags := tailnetCfg.Tags
	if len(tags) == 0 && clientSecret != "" {
		if discovered := resolveOAuthTags(clientSecret, tailnetCfg.ControlURL); len(discovered) > 0 {
			log.Printf("Auto-discovered OAuth ACL tags for tailnet '%s': %v", tailnetCfg.Profile, discovered)
			tags = discovered
		}
	}

	server := &tsnet.Server{
		Dir:           tailnetDir,
		Hostname:      tailnetCfg.Hostname,
		AuthKey:       authKey,
		ClientSecret:  clientSecret,
		ControlURL:    tailnetCfg.ControlURL,
		Ephemeral:     ephemeral,
		AdvertiseTags: tags,
		Port:          tailnetCfg.WireGuardPort,
	}

	// Use in-memory store for ephemeral nodes so node state is never persisted across restarts.
	if ephemeral {
		server.Store = &mem.Store{}
	}

	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("failed to start tsnet server: %w", err)
	}

	log.Printf("Tailnet '%s' server started successfully (hostname: %s, ephemeral: %v)",
		tailnetCfg.Profile, tailnetCfg.Hostname, ephemeral)
	return server, nil
}

// cleanupTailnetServer performs thorough cleanup of a Tailscale server.
func cleanupTailnetServer(server *tsnet.Server, tailnetName string) {
	log.Printf("Cleaning up tailnet connection: %s", tailnetName)
	if server != nil {
		if server.Ephemeral {
			if lc, err := server.LocalClient(); err == nil && lc != nil {
				logoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := lc.Logout(logoutCtx); err != nil {
					log.Printf("Warning: error logging out ephemeral tailnet server %s: %v", tailnetName, err)
				} else {
					log.Printf("Logged out ephemeral tailnet node for %s from control plane", tailnetName)
				}
				cancel()
			}
		}
		if err := server.Close(); err != nil {
			log.Printf("Warning: error closing tailnet server %s: %v", tailnetName, err)
		}
	}
	log.Printf("Tailnet connection %s cleaned up", tailnetName)
}

// syncTailnets reconciles running tsnet.Server instances with the provided configuration,
// starting added tailnets in parallel, restarting modified tailnets, and returning a cleanup callback for removed ones.
func (tm *tailnetManager) syncTailnets(tailnets []config.TailnetConfig) (map[string]*tsnet.Server, map[string]bool, func(), error) {
	servers, restarted, commit, _, err := tm.syncTailnetsWithRollback(tailnets)
	return servers, restarted, commit, err
}

// syncTailnetsWithRollback reconciles running tsnet.Server instances and returns both a commit callback
// (which cleans up removed tailnet servers after listeners migrate) and a rollback callback
// (which restores removed tailnet servers and cleans up newly added ones if config reload fails).
func (tm *tailnetManager) syncTailnetsWithRollback(tailnets []config.TailnetConfig) (map[string]*tsnet.Server, map[string]bool, func(), func(), error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.initErrors == nil {
		tm.initErrors = make(map[string]string)
	}

	prevConfigs := maps.Clone(tm.configs)
	prevInitErrors := maps.Clone(tm.initErrors)

	newConfigs := make(map[string]config.TailnetConfig, len(tailnets))
	for _, t := range tailnets {
		newConfigs[t.Profile] = t
	}

	addedServers := make(map[string]*tsnet.Server)
	restartedTailnets := make(map[string]bool)
	reinitializingTailnets := make(map[string]bool)
	removedServers := make(map[string]*tsnet.Server)
	removedConfigs := make(map[string]config.TailnetConfig)

	type initTask struct {
		cfg     config.TailnetConfig
		restart bool
	}
	var tasks []initTask

	for _, t := range tailnets {
		existingSrv, exists := tm.servers[t.Profile]
		if !exists {
			tasks = append(tasks, initTask{cfg: t, restart: false})
			continue
		}

		oldCfg := tm.configs[t.Profile]
		if !oldCfg.ConnectionSettingsEqual(t) {
			log.Printf("Tailnet '%s' connection settings changed, reinitializing...", t.Profile)
			// Close and log out existing server first to release state dir lock and avoid duplicate hostname suffix.
			tm.cleanupFunc(existingSrv, t.Profile)
			delete(tm.servers, t.Profile)
			delete(tm.configs, t.Profile)
			delete(tm.initErrors, t.Profile)
			reinitializingTailnets[t.Profile] = true
			tasks = append(tasks, initTask{cfg: t, restart: true})
		} else {
			tm.configs[t.Profile] = t
			delete(tm.initErrors, t.Profile)
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
					name:    tk.cfg.Profile,
					srv:     srv,
					restart: tk.restart,
					err:     err,
				}
			})
		}
		wg.Wait()

		for _, res := range results {
			if res.err != nil {
				if res.restart {
					log.Printf("ERROR: failed to reinitialize tailnet '%s': %v (isolating failure; healthy tailnets unaffected)", res.name, res.err)
				} else {
					log.Printf("ERROR: failed to initialize tailnet '%s': %v (isolating failure; healthy tailnets unaffected)", res.name, res.err)
				}
				tm.initErrors[res.name] = res.err.Error()
				tm.configs[res.name] = newConfigs[res.name]
				delete(tm.servers, res.name)
				continue
			}
			delete(tm.initErrors, res.name)
			addedServers[res.name] = res.srv
			if res.restart {
				restartedTailnets[res.name] = true
			}
		}
	}

	for name, srv := range addedServers {
		tm.servers[name] = srv
		tm.configs[name] = newConfigs[name]
	}

	// Identify removed tailnets so they can be closed after listeners stop using them,
	// or restored if the reload fails and rolls back.
	for name, srv := range tm.servers {
		if _, exists := newConfigs[name]; !exists {
			removedServers[name] = srv
			removedConfigs[name] = tm.configs[name]
			delete(tm.servers, name)
			delete(tm.configs, name)
			delete(tm.initErrors, name)
		}
	}
	for name := range tm.configs {
		if _, exists := newConfigs[name]; !exists {
			delete(tm.configs, name)
			delete(tm.initErrors, name)
		}
	}
	for name := range tm.initErrors {
		if _, exists := newConfigs[name]; !exists {
			delete(tm.initErrors, name)
		}
	}

	snapshot := maps.Clone(tm.servers)

	var finishOnce sync.Once
	commit := func() {
		finishOnce.Do(func() {
			for name, srv := range removedServers {
				tm.cleanupFunc(srv, name)
			}
		})
	}

	rollback := func() {
		finishOnce.Do(func() {
			tm.mu.Lock()
			for name, srv := range addedServers {
				if !restartedTailnets[name] {
					delete(tm.servers, name)
					delete(tm.configs, name)
					tm.cleanupFunc(srv, name)
				}
			}
			for name, srv := range removedServers {
				tm.servers[name] = srv
				tm.configs[name] = removedConfigs[name]
			}
			for name := range newConfigs {
				if reinitializingTailnets[name] {
					continue
				}
				if oldCfg, existed := prevConfigs[name]; existed {
					tm.configs[name] = oldCfg
					if oldErr, hadErr := prevInitErrors[name]; hadErr {
						tm.initErrors[name] = oldErr
					} else {
						delete(tm.initErrors, name)
					}
				} else {
					delete(tm.configs, name)
					delete(tm.initErrors, name)
				}
			}
			for name, oldCfg := range prevConfigs {
				if _, exists := newConfigs[name]; !exists {
					tm.configs[name] = oldCfg
					if oldErr, hadErr := prevInitErrors[name]; hadErr {
						tm.initErrors[name] = oldErr
					}
				}
			}
			tm.mu.Unlock()
		})
	}

	return snapshot, restartedTailnets, commit, rollback, nil
}

// snapshotServers returns a thread-safe copy of the currently active tailnet servers.
func (tm *tailnetManager) snapshotServers() map[string]*tsnet.Server {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return maps.Clone(tm.servers)
}

// snapshotConfigs returns a thread-safe copy of the configured tailnet configs.
func (tm *tailnetManager) snapshotConfigs() map[string]config.TailnetConfig {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return maps.Clone(tm.configs)
}

// snapshotInitErrors returns a thread-safe copy of the per-tailnet initialization errors.
func (tm *tailnetManager) snapshotInitErrors() map[string]string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return maps.Clone(tm.initErrors)
}

// isConfiguredTailnet reports whether the given tailnet profile is currently configured
// (either running in tm.servers or tracked in tm.configs / tm.initErrors).
func (tm *tailnetManager) isConfiguredTailnet(name string) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if _, ok := tm.servers[name]; ok {
		return true
	}
	if _, ok := tm.configs[name]; ok {
		return true
	}
	if _, ok := tm.initErrors[name]; ok {
		return true
	}
	return false
}

// closeAll shuts down all managed tsnet.Server instances.
func (tm *tailnetManager) closeAll() {
	tm.stopVIPWatchers()

	tm.mu.Lock()
	defer tm.mu.Unlock()

	log.Println("Starting cleanup of tailnet servers...")
	for name, srv := range tm.servers {
		tm.cleanupFunc(srv, name)
		delete(tm.servers, name)
		delete(tm.configs, name)
	}
	clear(tm.configs)
	clear(tm.initErrors)
	log.Println("All tailnet servers cleaned up")
}

func applyTailnetServersToProxy(proxyServer *proxy.Proxy, tm *tailnetManager, servers map[string]*tsnet.Server) {
	proxyServer.SetTailnetErrors(tm.snapshotInitErrors())
	if tm.dialerForServer != nil {
		dialers := make(map[string]proxy.Dialer, len(servers))
		for name, srv := range servers {
			dialers[name] = tm.dialerForServer(name, srv)
		}
		proxyServer.UpdateTailnetDialers(dialers)
	} else {
		proxyServer.UpdateTailnetServers(servers)
	}
}

var (
	configWatchDebounce       = 200 * time.Millisecond
	configReloadRetryAttempts = 6
	configReloadRetryInterval = 50 * time.Millisecond
	configReloadSettleDelay   = 50 * time.Millisecond
)

func configRemovesExistingTailnet(existing map[string]config.TailnetConfig, next []config.TailnetConfig) bool {
	if len(existing) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(next))
	for _, t := range next {
		seen[t.Profile] = struct{}{}
	}
	for name := range existing {
		if _, ok := seen[name]; !ok {
			return true
		}
	}
	return false
}

// loadConfigForReload reads and parses configFile during a live hot-reload, retrying through
// transient save states (such as O_TRUNC 0-byte truncation, atomic unlink-then-create, or
// multi-chunk writes) so active tailnets are never torn down by an intermediate file state.
func loadConfigForReload(configFile string, tm *tailnetManager) (*config.Config, error) {
	existingConfigs := tm.snapshotConfigs()
	hasActiveTailnets := len(existingConfigs) > 0
	attempts := max(1, configReloadRetryAttempts)

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 && configReloadRetryInterval > 0 {
			time.Sleep(configReloadRetryInterval)
		}

		rawBytes, err := os.ReadFile(configFile)
		if err != nil {
			lastErr = fmt.Errorf("reading config file %s: %w", configFile, err)
			continue
		}

		cfg, err := config.Parse(rawBytes)
		if err != nil {
			lastErr = err
			continue
		}

		if cfg.IsEmptyDocument && hasActiveTailnets {
			lastErr = fmt.Errorf("refusing to reload empty/truncated config file %s while %d tailnet(s) are configured (use 'tailnets: []' to explicitly remove all tailnets)", configFile, len(existingConfigs))
			continue
		}

		if hasActiveTailnets && configReloadSettleDelay > 0 && configRemovesExistingTailnet(existingConfigs, cfg.Tailnets) {
			time.Sleep(configReloadSettleDelay)
			settledBytes, readErr := os.ReadFile(configFile)
			if readErr != nil {
				lastErr = fmt.Errorf("reading config file %s: %w", configFile, readErr)
				continue
			}
			if !bytes.Equal(rawBytes, settledBytes) {
				settledCfg, parseErr := config.Parse(settledBytes)
				if parseErr != nil {
					lastErr = parseErr
					continue
				}
				if settledCfg.IsEmptyDocument {
					lastErr = fmt.Errorf("refusing to reload empty/truncated config file %s while %d tailnet(s) are configured (use 'tailnets: []' to explicitly remove all tailnets)", configFile, len(existingConfigs))
					continue
				}
				return settledCfg, nil
			}
		}

		return cfg, nil
	}

	return nil, fmt.Errorf("failed to load new config: %w", lastErr)
}

// reloadConfig loads and validates the updated configuration and applies it to the running proxy.
func reloadConfig(configFile string, proxyServer *proxy.Proxy, tm *tailnetManager) error {
	tm.reloadMu.Lock()
	defer tm.reloadMu.Unlock()

	newCfg, err := loadConfigForReload(configFile, tm)
	if err != nil {
		return err
	}

	manualMappings := newCfg.ToProxyPortMappings()

	servers, restartedTailnets, commitTailnets, rollbackTailnets, err := tm.syncTailnetsWithRollback(newCfg.Tailnets)
	if err != nil {
		return err
	}

	for _, mapping := range manualMappings {
		if !tm.isConfiguredTailnet(mapping.Tailnet) {
			rollbackTailnets()
			return fmt.Errorf("port mapping for port %d references unknown tailnet '%s'", mapping.Source, mapping.Tailnet)
		}
	}

	vipPorts, nextAllocs, discoveredSnap, diffLogs, err := tm.computeVIPMappingsLocked(context.Background(), newCfg, servers, false)
	if err != nil {
		rollbackTailnets()
		return fmt.Errorf("failed to resolve VIP port mappings: %w", err)
	}

	desiredPorts := make([]proxy.PortMapping, 0, len(manualMappings)+len(vipPorts))
	desiredPorts = append(desiredPorts, manualMappings...)
	desiredPorts = append(desiredPorts, vipPorts...)

	prevApplied := tm.appliedPorts
	if prevApplied == nil {
		prevApplied = proxyServer.CurrentPorts()
	}

	applyTailnetServersToProxy(proxyServer, tm, servers)
	proxyServer.SetProbeConfig(newCfg.ToProxyProbeConfig())

	if err := proxyServer.ReloadConfig(desiredPorts); err != nil {
		rollbackTailnets()
		restoredServers := tm.snapshotServers()
		applyTailnetServersToProxy(proxyServer, tm, restoredServers)
		if tm.activeCfg != nil {
			proxyServer.SetProbeConfig(tm.activeCfg.ToProxyProbeConfig())
		}
		if rbErr := proxyServer.ReloadConfig(slices.Clone(prevApplied)); rbErr != nil {
			log.Printf("Warning: error rolling back failed config reload: %v", rbErr)
		}
		return fmt.Errorf("failed to apply new config: %w", err)
	}

	if len(restartedTailnets) > 0 {
		if err := proxyServer.RestartTailnetPorts(restartedTailnets); err != nil {
			commitTailnets()
			return fmt.Errorf("failed to restart ports for updated tailnets: %w", err)
		}
	}

	commitTailnets()

	oldCfg := tm.activeCfg
	tm.activeCfg = newCfg
	tm.vipAllocations = nextAllocs
	tm.lastTailnetVIPs = discoveredSnap
	tm.appliedPorts = slices.Clone(desiredPorts)
	tm.invalidateHealthCache()

	if oldCfg != nil {
		warnNonReloadableProxyChanges(oldCfg.Proxy, newCfg.Proxy)
	}

	for _, line := range diffLogs {
		log.Println(line)
	}

	tm.syncVIPWatchers(newCfg, servers)

	return nil
}

// warnNonReloadableProxyChanges logs warnings when process-scoped ProxySettings are changed during a hot-reload.
func warnNonReloadableProxyChanges(oldProxy, newProxy config.ProxySettings) {
	if oldProxy.WorkerPoolSize != newProxy.WorkerPoolSize {
		log.Printf("WARN: proxy.worker_pool_size changed (%d -> %d); process restart required to take effect", oldProxy.WorkerPoolSize, newProxy.WorkerPoolSize)
	}
	if oldProxy.BufferSize != newProxy.BufferSize {
		log.Printf("WARN: proxy.buffer_size changed (%d -> %d); process restart required to take effect", oldProxy.BufferSize, newProxy.BufferSize)
	}
	if oldProxy.APIPort != newProxy.APIPort {
		log.Printf("WARN: proxy.api_port changed (%d -> %d); process restart required to take effect", oldProxy.APIPort, newProxy.APIPort)
	}
	if oldProxy.EffectiveAPIListenAddress() != newProxy.EffectiveAPIListenAddress() {
		log.Printf("WARN: proxy.api_listen_address changed (%q -> %q); process restart required to take effect", oldProxy.EffectiveAPIListenAddress(), newProxy.EffectiveAPIListenAddress())
	}
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
			// Trigger reload on direct config file modifications or Kubernetes ConfigMap symlink swaps ("..data").
			// Ignore bare Remove events because editors that save via unlink+create emit Remove before Create;
			// triggering on Create/Write/Rename avoids starting the debounce timer while the file is unlinked.
			base := filepath.Base(event.Name)
			if (base == configBase || base == "..data") &&
				event.Has(fsnotify.Write|fsnotify.Create|fsnotify.Rename) {
				log.Printf("Config file change detected (%s on %s), scheduling reload...", event.Op, base)
				if debounceTimer == nil {
					debounceTimer = time.NewTimer(configWatchDebounce)
					debounceC = debounceTimer.C
				} else {
					if !debounceTimer.Stop() {
						select {
						case <-debounceTimer.C:
						default:
						}
					}
					debounceTimer.Reset(configWatchDebounce)
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

// healthTailnetStatus describes the runtime status of a single tailnet for /healthz,
// embedding Tailscale's native ipnstate.Status (/localapi/v0/status) alongside
// VIP Services (/localapi/v0/services) and ProxTail profile metadata.
type healthTailnetStatus struct {
	Profile string `json:"Profile"`
	Error   string `json:"Error,omitempty"`
	*ipnstate.Status
	Services map[tailcfg.ServiceName]tailcfg.ServiceDetails `json:"Services,omitempty"`
}

// healthResponse is the JSON payload returned by GET /healthz.
type healthResponse struct {
	Status           string                `json:"Status"`
	ActiveListeners  int                   `json:"ActiveListeners"`
	Tailnets         []healthTailnetStatus `json:"Tailnets"`
	Ports            []proxy.PortStatus    `json:"Ports,omitempty"`
	TCPConnections   int64                 `json:"TCPConnections"`
	UDPPackets       int64                 `json:"UDPPackets"`
	BytesTransferred int64                 `json:"BytesTransferred"`
	Errors           int64                 `json:"Errors"`
}

// computeHealthStatus determines the overall health status ("ok", "degraded", or "down")
// and the corresponding HTTP status code (200 OK for "ok"/"degraded", 503 Service Unavailable for "down").
func computeHealthStatus(isReady bool, configuredPorts int, activeListeners int, ports []proxy.PortStatus, tailnets []healthTailnetStatus) (string, int) {
	if !isReady {
		return "down", http.StatusServiceUnavailable
	}
	if configuredPorts > 0 && (len(tailnets) == 0 || activeListeners == 0) {
		return "down", http.StatusServiceUnavailable
	}
	if len(tailnets) > 0 {
		errorTailnets := 0
		for _, tn := range tailnets {
			if tn.Error != "" || (tn.Status != nil && tn.BackendState == "Error") {
				errorTailnets++
			}
		}
		if errorTailnets == len(tailnets) {
			return "down", http.StatusServiceUnavailable
		}
	}

	for _, ps := range ports {
		if !ps.Up {
			return "degraded", http.StatusOK
		}
	}
	for _, tn := range tailnets {
		if tn.Error != "" {
			return "degraded", http.StatusOK
		}
		if tn.Status != nil {
			if (tn.BackendState != "" && tn.BackendState != "Running") || len(tn.Health) > 0 {
				return "degraded", http.StatusOK
			}
		}
	}

	return "ok", http.StatusOK
}

// newHealthHandler constructs an http.Handler serving /healthz (unified health & readiness), /probe (on-demand pre-flight reachability & path probing), and /metrics (Prometheus).
func newHealthHandler(proxyServer *proxy.Proxy, tm *tailnetManager) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(proxy.ProbeResult{
				Error: "method not allowed: use GET",
			})
			return
		}

		q := r.URL.Query()
		profile := strings.TrimSpace(q.Get("profile"))
		if profile == "" {
			profile = strings.TrimSpace(q.Get("tailnet"))
		}
		protocol := strings.ToLower(strings.TrimSpace(q.Get("protocol")))
		if protocol == "" {
			protocol = "tcp"
		}
		destination := strings.TrimSpace(q.Get("destination"))

		if profile == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(proxy.ProbeResult{
				Protocol:    protocol,
				Destination: destination,
				Error:       "missing required query parameter 'profile'",
			})
			return
		}
		if protocol != "tcp" && protocol != "udp" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(proxy.ProbeResult{
				Profile:     profile,
				Protocol:    protocol,
				Destination: destination,
				Error:       "invalid 'protocol': must be 'tcp' or 'udp'",
			})
			return
		}
		if destination == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(proxy.ProbeResult{
				Profile:  profile,
				Protocol: protocol,
				Error:    "missing required query parameter 'destination' (expected host:port)",
			})
			return
		}
		host, portStr, splitErr := net.SplitHostPort(destination)
		portNum, portErr := strconv.Atoi(portStr)
		if splitErr != nil || strings.TrimSpace(host) == "" || portErr != nil || portNum < 1 || portNum > 65535 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(proxy.ProbeResult{
				Profile:     profile,
				Protocol:    protocol,
				Destination: destination,
				Error:       "invalid 'destination': expected host:port with port 1-65535",
			})
			return
		}

		var timeout time.Duration
		if rawTimeout := strings.TrimSpace(q.Get("timeout")); rawTimeout != "" {
			if d, err := time.ParseDuration(rawTimeout); err == nil {
				timeout = d
			} else if secs, err := strconv.Atoi(rawTimeout); err == nil {
				timeout = time.Duration(secs) * time.Second
			} else {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(proxy.ProbeResult{
					Profile:     profile,
					Protocol:    protocol,
					Destination: destination,
					Error:       "invalid 'timeout': expected duration (e.g. 2s, 1500ms)",
				})
				return
			}
			if timeout <= 0 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(proxy.ProbeResult{
					Profile:     profile,
					Protocol:    protocol,
					Destination: destination,
					Error:       "invalid 'timeout': must be greater than 0",
				})
				return
			}
		}

		rawStream := strings.ToLower(strings.TrimSpace(q.Get("stream")))
		accept := strings.ToLower(r.Header.Get("Accept"))
		streamMode := ""
		switch rawStream {
		case "", "false", "0":
			if strings.Contains(accept, "text/event-stream") {
				streamMode = "sse"
			} else if strings.Contains(accept, "application/x-ndjson") {
				streamMode = "ndjson"
			}
		case "true", "1", "ndjson":
			streamMode = "ndjson"
		case "sse":
			streamMode = "sse"
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(proxy.ProbeResult{
				Profile:     profile,
				Protocol:    protocol,
				Destination: destination,
				Error:       "invalid 'stream': expected 'true', 'ndjson', 'sse', or 'false'",
			})
			return
		}

		hasProfile := proxyServer.HasTailnet(profile)
		var fallbackErr string
		if !hasProfile && tm != nil {
			initErrors := tm.snapshotInitErrors()
			configs := tm.snapshotConfigs()
			if initErr := initErrors[profile]; initErr != "" {
				fallbackErr = fmt.Sprintf("tailnet '%s' unavailable: %s", profile, initErr)
				hasProfile = true
			} else if _, ok := configs[profile]; ok {
				fallbackErr = fmt.Sprintf("tailnet '%s' is not ready", profile)
				hasProfile = true
			}
		}
		if !hasProfile {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(proxy.ProbeResult{
				Profile:     profile,
				Protocol:    protocol,
				Destination: destination,
				Error:       fmt.Sprintf("tailnet profile '%s' not found", profile),
			})
			return
		}

		if streamMode == "" {
			if fallbackErr != "" {
				_ = json.NewEncoder(w).Encode(proxy.ProbeResult{
					Profile:     profile,
					Protocol:    protocol,
					Destination: destination,
					Error:       fallbackErr,
				})
				return
			}
			res, _ := proxyServer.ProbeDestination(r.Context(), profile, protocol, destination, timeout)
			_ = json.NewEncoder(w).Encode(res)
			return
		}

		flusher, _ := w.(http.Flusher)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if streamMode == "sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Connection", "keep-alive")
		} else {
			w.Header().Set("Content-Type", "application/x-ndjson")
		}

		writeStage := func(ev proxy.ProbeResult) {
			data, err := json.Marshal(ev)
			if err != nil {
				return
			}
			if streamMode == "sse" {
				_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Stage, data)
			} else {
				_, _ = w.Write(append(data, '\n'))
			}
			if flusher != nil {
				flusher.Flush()
			}
		}

		if fallbackErr != "" {
			writeStage(proxy.ProbeResult{
				Stage:       "netmap",
				Profile:     profile,
				Protocol:    protocol,
				Destination: destination,
				Error:       fallbackErr,
			})
			return
		}

		_, _ = proxyServer.ProbeDestinationStream(r.Context(), profile, protocol, destination, timeout, writeStage)
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		stats := proxyServer.GetStats()
		portStatuses := proxyServer.GetPortStatuses()
		configuredPorts := proxyServer.ConfiguredPortCount()
		activeListeners := proxyServer.ActiveListenerCount()
		isReady := proxyServer.IsReady()
		servers := tm.snapshotServers()
		configs := tm.snapshotConfigs()
		initErrors := tm.snapshotInitErrors()

		nameSet := make(map[string]struct{}, len(servers)+len(configs)+len(initErrors))
		for name := range servers {
			nameSet[name] = struct{}{}
		}
		for name := range configs {
			nameSet[name] = struct{}{}
		}
		for name := range initErrors {
			nameSet[name] = struct{}{}
		}

		names := make([]string, 0, len(nameSet))
		for name := range nameSet {
			names = append(names, name)
		}
		sort.Strings(names)

		tailnets := make([]healthTailnetStatus, len(names))
		var wg sync.WaitGroup
		for i, name := range names {
			i, name := i, name
			srv := servers[name]
			st := healthTailnetStatus{
				Profile: name,
				Status: &ipnstate.Status{
					Self: &ipnstate.PeerStatus{},
				},
			}
			if srv == nil {
				if cfg, ok := configs[name]; ok {
					st.Self.HostName = cfg.Hostname
				}
				if initErr := initErrors[name]; initErr != "" {
					st.Error = initErr
					st.BackendState = "Error"
					st.Health = []string{initErr}
				}
				tailnets[i] = st
				continue
			}
			st.Self.HostName = srv.Hostname
			if srv.Sys() != nil || tm.healthDiscoverFunc != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					details := tm.getCachedTailnetDetails(r.Context(), name, srv)
					if details.Status != nil {
						st.Status = details.Status
					}
					if st.Self == nil {
						st.Self = &ipnstate.PeerStatus{}
					}
					if st.Self.HostName == "" {
						st.Self.HostName = srv.Hostname
					}
					st.Services = details.Services
					tailnets[i] = st
				}()
				continue
			}
			tailnets[i] = st
		}
		wg.Wait()

		status, httpCode := computeHealthStatus(isReady, configuredPorts, activeListeners, portStatuses, tailnets)

		w.Header().Set("Content-Type", "application/json")
		if httpCode != http.StatusOK {
			w.WriteHeader(httpCode)
		}
		_ = json.NewEncoder(w).Encode(healthResponse{
			Status:           status,
			ActiveListeners:  activeListeners,
			Tailnets:         tailnets,
			Ports:            portStatuses,
			TCPConnections:   stats.TCPConnections,
			UDPPackets:       stats.UDPPackets,
			BytesTransferred: stats.BytesTransferred,
			Errors:           stats.ErrorCount,
		})
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		stats := proxyServer.GetStats()
		servers := tm.snapshotServers()
		portStatuses := proxyServer.GetPortStatuses()
		configuredPorts := proxyServer.ConfiguredPortCount()

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
		fmt.Fprintf(&b, "# HELP proxtail_ports_configured Number of currently configured port mappings.\n")
		fmt.Fprintf(&b, "# TYPE proxtail_ports_configured gauge\n")
		fmt.Fprintf(&b, "proxtail_ports_configured %d\n", configuredPorts)
		fmt.Fprintf(&b, "# HELP proxtail_listeners_active Number of currently bound local TCP and UDP listeners.\n")
		fmt.Fprintf(&b, "# TYPE proxtail_listeners_active gauge\n")
		fmt.Fprintf(&b, "proxtail_listeners_active %d\n", proxyServer.ActiveListenerCount())
		if len(portStatuses) > 0 {
			fmt.Fprintf(&b, "# HELP proxtail_port_up Whether the local listener for a configured port mapping is currently bound and active (1 = up, 0 = down).\n")
			fmt.Fprintf(&b, "# TYPE proxtail_port_up gauge\n")
			for _, ps := range portStatuses {
				upVal := 0
				if ps.Up {
					upVal = 1
				}
				fmt.Fprintf(&b, "proxtail_port_up{profile=%q,protocol=%q,listen_address=%q,port=\"%d\",destination=%q,vip_service=%q} %d\n",
					ps.Tailnet,
					ps.Protocol,
					ps.ListenAddress,
					ps.Source,
					ps.Destination,
					ps.VIPService,
					upVal,
				)
			}
		}
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
		log.Printf("Health & metrics server listening on %s (/healthz, /metrics)", addr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Health server error: %v", err)
		}
	}()

	return nil
}

// runHealthCheck probes the running proxtail instance via its configured API port or TCP listeners.
func runHealthCheck(cfg *config.Config) error {
	if cfg.Proxy.APIPort > 0 {
		probeHost := strings.TrimSpace(cfg.Proxy.EffectiveAPIListenAddress())
		if probeHost == "" || probeHost == "0.0.0.0" || probeHost == "::" {
			probeHost = "127.0.0.1"
		}
		url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(probeHost, strconv.Itoa(cfg.Proxy.APIPort)))
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

	// If api_port is not enabled, verify that at least one configured TCP listener is accepting connections.
	mappings := cfg.ToProxyPortMappings()
	for _, mapping := range mappings {
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

	if len(cfg.Tailnets) == 0 {
		return fmt.Errorf("cannot probe running instance in standby mode (0 tailnets configured): configure proxy.api_port to enable HTTP health checks")
	}

	if len(mappings) == 0 {
		for _, t := range cfg.Tailnets {
			if t.IsAutoProxyVIPs() {
				return fmt.Errorf("cannot probe running instance with only auto_proxy_vips mappings when api_port is disabled: configure proxy.api_port to probe dynamic VIP listeners")
			}
		}
		return fmt.Errorf("cannot probe running instance: neither api_port nor TCP listeners are configured (configure proxy.api_port to enable HTTP health checks)")
	}

	return fmt.Errorf("cannot probe running instance with UDP-only port mappings when api_port is disabled: configure proxy.api_port to enable HTTP health checks")
}

// resolveConfigPath determines the configuration file path from CLI flag, PROXTAIL_CONFIG,
// a local ./config.yaml in the current working directory, or /etc/proxtail/config.yaml.
func resolveConfigPath(flagConfig string) string {
	if flagConfig != "" {
		return flagConfig
	}
	if p := os.Getenv("PROXTAIL_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}
	return "/etc/proxtail/config.yaml"
}

// version and buildDate are populated at build time via -ldflags="-X main.version=... -X main.buildDate=..."
var (
	version   = "dev"
	buildDate = ""
)

func formatVersion() string {
	if buildDate != "" {
		return fmt.Sprintf("proxtail %s (%s)", version, buildDate)
	}
	return fmt.Sprintf("proxtail %s", version)
}

func main() {
	var flagConfig string
	var flagValidate bool
	var flagHealthCheck bool
	var flagVersion bool
	flag.StringVar(&flagConfig, "config", "", "Path to configuration file")
	flag.StringVar(&flagConfig, "c", "", "Path to configuration file (shorthand)")
	flag.BoolVar(&flagValidate, "validate", false, "Validate configuration file and exit")
	flag.BoolVar(&flagValidate, "check", false, "Validate configuration file and exit (shorthand)")
	flag.BoolVar(&flagHealthCheck, "healthcheck", false, "Probe running proxtail instance health and exit")
	flag.BoolVar(&flagVersion, "version", false, "Print version and exit")
	flag.BoolVar(&flagVersion, "v", false, "Print version and exit (shorthand)")
	flag.Parse()

	if flagVersion {
		fmt.Println(formatVersion())
		return
	}

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
	signal.Ignore(syscall.SIGPIPE)
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
		if !tm.isConfiguredTailnet(mapping.Tailnet) {
			tm.closeAll()
			log.Fatalf("Port mapping for port %d references unknown tailnet '%s'", mapping.Source, mapping.Tailnet)
		}
	}

	initialPorts, err := tm.prepareInitialPorts(ctx, cfg, tailnetServers)
	if err != nil {
		tm.closeAll()
		log.Fatalf("Failed to initialize VIP service port mappings: %v", err)
	}

	proxyServer := proxy.NewProxyWithMultiTailnet(
		initialPorts,
		tailnetServers,
		cfg.Proxy.BufferSize*1024,
		cfg.Proxy.WorkerPoolSize,
	)
	proxyServer.SetTailnetErrors(tm.snapshotInitErrors())
	proxyServer.SetProbeConfig(cfg.ToProxyProbeConfig())

	if cfg.Proxy.APIPort > 0 {
		if err := startHealthServer(ctx, cfg.Proxy.EffectiveAPIListenAddress(), cfg.Proxy.APIPort, proxyServer, tm); err != nil {
			tm.closeAll()
			log.Fatalf("Failed to start health server: %v", err)
		}
	}

	fmt.Printf("Starting proxtail with %d tailnets, %d workers, %dKB buffers...\n",
		len(tailnetServers), cfg.Proxy.WorkerPoolSize, cfg.Proxy.BufferSize)
	if len(cfg.Tailnets) == 0 {
		if cfg.Proxy.APIPort > 0 {
			log.Printf("Running in standby mode (0 tailnets configured; health server active on :%d)", cfg.Proxy.APIPort)
		} else {
			log.Printf("Running in standby mode (0 tailnets configured)")
		}
	} else if len(tailnetServers) == 0 {
		log.Printf("WARNING: 0 of %d configured tailnets initialized successfully; waiting for configuration reload", len(cfg.Tailnets))
	} else if len(initialPorts) == 0 {
		if cfg.Proxy.APIPort > 0 {
			log.Printf("Running in tailnet discovery mode (0 port mappings configured; health & discovery active on :%d)", cfg.Proxy.APIPort)
		} else {
			log.Printf("Running with 0 port mappings configured")
		}
	}
	fmt.Printf("Hot reload enabled - watching for config changes in: %s\n", configFile)
	fmt.Printf("Manual reload available - send SIGHUP signal to trigger config reload\n")

	proxyErrChan := make(chan error, 1)
	go func() {
		proxyErrChan <- proxyServer.Start(ctx)
	}()

	var startupErr error
	proxyStarted := false
	select {
	case startupErr = <-proxyErrChan:
		if startupErr != nil {
			log.Printf("Proxy server exited with error: %v", startupErr)
			cancel()
		}
	case <-proxyServer.Started():
		proxyStarted = true
	case <-shutdown:
		log.Println("Shutdown signal received, stopping proxy...")
	case <-ctx.Done():
		log.Println("Context cancelled, stopping proxy...")
	}

	if proxyStarted {
		tm.startVIPReconciler(ctx, proxyServer)
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
