package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"

	"proxtail/internal/config"
	"proxtail/internal/proxy"
)

// errNoNetmap indicates that a tsnet LocalBackend has not yet received its initial netmap
// from the coordination server (/localapi/v0/services returned "no netmap").
var errNoNetmap = errors.New("tailnet has no netmap yet")

// vipDiscoverFunc queries the VIP services visible to a tailnet node.
type vipDiscoverFunc func(ctx context.Context, tailnetName string, srv *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error)

// vipAllocKey uniquely identifies a sticky local port allocation for an auto-proxied VIP target port.
// Dual-protocol services (e.g. TCP+UDP on port 53) share the same vipAllocKey and therefore the same local port number.
type vipAllocKey struct {
	Tailnet    string
	Service    string
	TargetPort int
}

// vipTargetSpec describes a normalized VIP destination port and its enabled transport protocols.
type vipTargetSpec struct {
	Key        vipAllocKey
	TargetAddr string
	Protocols  []string
}

type vipWatcherEntry struct {
	srv    *tsnet.Server
	cancel context.CancelFunc
}

// defaultDiscoverVIPServices queries the live tsnet LocalClient for Tailscale VIP services.
func defaultDiscoverVIPServices(ctx context.Context, _ string, srv *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
	if srv == nil || srv.Sys() == nil {
		return nil, nil
	}
	lc, err := srv.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("getting local client: %w", err)
	}
	if lc == nil {
		return nil, nil
	}

	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	svcs, err := lc.GetServices(qCtx)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no netmap") {
			return nil, errNoNetmap
		}
		return nil, err
	}
	return svcs, nil
}

// expandVIPServicesForTailnet converts Tailscale ServiceDetails entries for a single tailnet
// into deterministic, validated vipTargetSpec entries.
func expandVIPServicesForTailnet(
	tailnetName string,
	svcs map[tailcfg.ServiceName]tailcfg.ServiceDetails,
	rangeCapacity int,
) ([]vipTargetSpec, error) {
	if len(svcs) == 0 {
		return nil, nil
	}

	type targetAccum struct {
		addr   string
		hasTCP bool
		hasUDP bool
	}
	accum := make(map[vipAllocKey]*targetAccum)

	// Sort service keys first so any validation error is reported deterministically.
	svcNames := make([]tailcfg.ServiceName, 0, len(svcs))
	for name := range svcs {
		svcNames = append(svcNames, name)
	}
	sort.Slice(svcNames, func(i, j int) bool {
		return svcNames[i] < svcNames[j]
	})

	for _, rawName := range svcNames {
		svc := svcs[rawName]
		serviceName := strings.TrimSpace(svc.Name.WithoutPrefix())
		if serviceName == "" {
			serviceName = strings.TrimSpace(rawName.WithoutPrefix())
		}
		if serviceName == "" {
			serviceName = strings.TrimSpace(string(svc.Name))
		}
		if serviceName == "" {
			serviceName = strings.TrimSpace(string(rawName))
		}

		var ip4, ip6 string
		for _, addr := range svc.Addrs {
			if !addr.IsValid() {
				continue
			}
			if addr.Is4() && ip4 == "" {
				ip4 = addr.String()
			} else if addr.Is6() && ip6 == "" {
				ip6 = addr.String()
			}
		}
		targetIP := ip4
		if targetIP == "" {
			targetIP = ip6
		}
		if targetIP == "" {
			return nil, fmt.Errorf("VIP service '%s' in tailnet '%s' has no valid IP address", serviceName, tailnetName)
		}

		for _, ppr := range svc.Ports {
			var addTCP, addUDP bool
			switch ppr.Proto {
			case 6: // IPPROTO_TCP
				addTCP = true
			case 17: // IPPROTO_UDP
				addUDP = true
			case 0: // Unspecified / all (TCP + UDP)
				addTCP = true
				addUDP = true
			default:
				// Ignore non-TCP/UDP protocols (e.g. ICMP).
				continue
			}

			first := int(ppr.Ports.First)
			last := int(ppr.Ports.Last)
			if ppr.Ports == tailcfg.PortRangeAny || (first == 0 && last == 65535) {
				return nil, fmt.Errorf(
					"VIP service '%s' in tailnet '%s' advertises wildcard port range (%s), which cannot be mapped into vip_port_range (capacity %d)",
					serviceName, tailnetName, ppr.String(), rangeCapacity,
				)
			}
			if first < 1 || last < first || last > 65535 {
				return nil, fmt.Errorf(
					"VIP service '%s' in tailnet '%s' has invalid port range %d-%d",
					serviceName, tailnetName, first, last,
				)
			}
			span := last - first + 1
			if span > rangeCapacity {
				return nil, fmt.Errorf(
					"VIP service '%s' in tailnet '%s' port range %d-%d (%d ports) exceeds vip_port_range capacity (%d)",
					serviceName, tailnetName, first, last, span, rangeCapacity,
				)
			}

			for p := first; p <= last; p++ {
				key := vipAllocKey{
					Tailnet:    tailnetName,
					Service:    serviceName,
					TargetPort: p,
				}
				entry, exists := accum[key]
				if !exists {
					entry = &targetAccum{addr: targetIP}
					accum[key] = entry
				} else if entry.addr == "" {
					entry.addr = targetIP
				}
				if addTCP {
					entry.hasTCP = true
				}
				if addUDP {
					entry.hasUDP = true
				}
			}
		}
	}

	if len(accum) == 0 {
		return nil, nil
	}

	targets := make([]vipTargetSpec, 0, len(accum))
	for key, entry := range accum {
		var protos []string
		if entry.hasTCP {
			protos = append(protos, "tcp")
		}
		if entry.hasUDP {
			protos = append(protos, "udp")
		}
		targets = append(targets, vipTargetSpec{
			Key:        key,
			TargetAddr: entry.addr,
			Protocols:  protos,
		})
	}

	sortVIPTargets(targets)
	return targets, nil
}

func sortVIPTargets(targets []vipTargetSpec) {
	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i].Key, targets[j].Key
		if a.Tailnet != b.Tailnet {
			return a.Tailnet < b.Tailnet
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.TargetPort < b.TargetPort
	})
}

// allocateVIPPorts performs sticky port allocation across [vipStart, vipEnd] for sorted VIP targets.
func allocateVIPPorts(
	targets []vipTargetSpec,
	prevAllocations map[vipAllocKey]int,
	vipStart, vipEnd int,
	listenAddress string,
) ([]proxy.PortMapping, map[vipAllocKey]int, []string, error) {
	nextAllocations := make(map[vipAllocKey]int, len(targets))
	usedPorts := make(map[int]bool, len(targets))

	// Pass 1: Retain existing allocations that remain valid within [vipStart, vipEnd].
	for _, target := range targets {
		if prevPort, ok := prevAllocations[target.Key]; ok {
			if prevPort >= vipStart && prevPort <= vipEnd && !usedPorts[prevPort] {
				nextAllocations[target.Key] = prevPort
				usedPorts[prevPort] = true
			}
		}
	}

	// Pass 2: Assign the lowest available port in [vipStart, vipEnd] to new targets.
	nextCandidate := vipStart
	for _, target := range targets {
		if _, exists := nextAllocations[target.Key]; exists {
			continue
		}
		for nextCandidate <= vipEnd && usedPorts[nextCandidate] {
			nextCandidate++
		}
		if nextCandidate > vipEnd {
			return nil, nil, nil, fmt.Errorf(
				"vip_port_range %d-%d exhausted while allocating local port for VIP service '%s' (target port %d) in tailnet '%s'",
				vipStart, vipEnd, target.Key.Service, target.Key.TargetPort, target.Key.Tailnet,
			)
		}
		nextAllocations[target.Key] = nextCandidate
		usedPorts[nextCandidate] = true
		nextCandidate++
	}

	var diffLogs []string

	// Record removed VIP allocations in deterministic order for operator visibility.
	if len(prevAllocations) > 0 {
		var removedKeys []vipAllocKey
		for oldKey := range prevAllocations {
			if _, stillPresent := nextAllocations[oldKey]; !stillPresent {
				removedKeys = append(removedKeys, oldKey)
			}
		}
		sort.Slice(removedKeys, func(i, j int) bool {
			if removedKeys[i].Tailnet != removedKeys[j].Tailnet {
				return removedKeys[i].Tailnet < removedKeys[j].Tailnet
			}
			if removedKeys[i].Service != removedKeys[j].Service {
				return removedKeys[i].Service < removedKeys[j].Service
			}
			return removedKeys[i].TargetPort < removedKeys[j].TargetPort
		})
		for _, oldKey := range removedKeys {
			diffLogs = append(diffLogs, fmt.Sprintf(
				"VIP auto-proxy: %s/%s (port %d) removed; releasing local port %d",
				oldKey.Tailnet, oldKey.Service, oldKey.TargetPort, prevAllocations[oldKey],
			))
		}
	}

	listenAddrClean := strings.TrimSpace(listenAddress)
	mappings := make([]proxy.PortMapping, 0, len(targets))

	for _, target := range targets {
		localPort := nextAllocations[target.Key]
		dest := net.JoinHostPort(target.TargetAddr, strconv.Itoa(target.Key.TargetPort))
		if prevPort, existed := prevAllocations[target.Key]; !existed || prevPort != localPort {
			diffLogs = append(diffLogs, fmt.Sprintf(
				"VIP auto-proxy: %s/%s (%s) -> local port %d (%s)",
				target.Key.Tailnet, target.Key.Service, dest, localPort, strings.Join(target.Protocols, "+"),
			))
		}
		for _, proto := range target.Protocols {
			mappings = append(mappings, proxy.PortMapping{
				Source:            localPort,
				ListenAddress:     listenAddrClean,
				Destination:       dest,
				Protocol:          proto,
				Tailnet:           target.Key.Tailnet,
				VIPService:        target.Key.Service,
				ConnectionTimeout: 30,
			})
		}
	}

	return mappings, nextAllocations, diffLogs, nil
}

func resolveVIPPortRange(cfg *config.Config) (int, int, error) {
	spec := ""
	if cfg != nil {
		spec = strings.TrimSpace(cfg.Proxy.VIPPortRange)
	}
	if spec == "" {
		spec = config.DefaultVIPPortRange
	}
	return config.ParsePortRange(spec)
}

func resolveVIPRefreshInterval(tm *tailnetManager, cfg *config.Config) time.Duration {
	if tm.vipRefreshOverride > 0 {
		return tm.vipRefreshOverride
	}
	if cfg != nil && cfg.Proxy.VIPRefreshInterval > 0 {
		return time.Duration(cfg.Proxy.VIPRefreshInterval) * time.Second
	}
	return time.Duration(config.DefaultVIPRefreshInterval) * time.Second
}

// computeVIPMappingsLocked queries VIP services across enabled tailnets and computes sticky local port mappings.
// Caller must hold tm.reloadMu.
func (tm *tailnetManager) computeVIPMappingsLocked(
	ctx context.Context,
	cfg *config.Config,
	servers map[string]*tsnet.Server,
	isInitial bool,
) ([]proxy.PortMapping, map[vipAllocKey]int, map[string]map[tailcfg.ServiceName]tailcfg.ServiceDetails, []string, error) {
	vipStart, vipEnd, err := resolveVIPPortRange(cfg)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	rangeCapacity := vipEnd - vipStart + 1

	discover := tm.vipDiscoverFunc
	if discover == nil {
		discover = defaultDiscoverVIPServices
	}

	discoveredSnap := make(map[string]map[tailcfg.ServiceName]tailcfg.ServiceDetails)
	var allTargets []vipTargetSpec

	for _, tailnetCfg := range cfg.Tailnets {
		if !tailnetCfg.IsAutoProxyVIPs() {
			continue
		}
		srv := servers[tailnetCfg.Name]
		svcs, dErr := discover(ctx, tailnetCfg.Name, srv)
		if dErr != nil {
			if errors.Is(dErr, errNoNetmap) {
				if isInitial {
					log.Printf("Tailnet '%s' awaiting initial netmap for VIP service discovery", tailnetCfg.Name)
				} else if prev, ok := tm.lastTailnetVIPs[tailnetCfg.Name]; ok {
					svcs = prev
				}
			} else {
				return nil, nil, nil, nil, fmt.Errorf("discovering VIP services for tailnet '%s': %w", tailnetCfg.Name, dErr)
			}
		}
		if len(svcs) > 0 {
			discoveredSnap[tailnetCfg.Name] = maps.Clone(svcs)
		}

		targets, expErr := expandVIPServicesForTailnet(tailnetCfg.Name, svcs, rangeCapacity)
		if expErr != nil {
			return nil, nil, nil, nil, expErr
		}
		allTargets = append(allTargets, targets...)
	}

	sortVIPTargets(allTargets)

	mappings, nextAllocs, diffLogs, allocErr := allocateVIPPorts(
		allTargets,
		tm.vipAllocations,
		vipStart,
		vipEnd,
		cfg.Proxy.ListenAddress,
	)
	if allocErr != nil {
		return nil, nil, nil, nil, allocErr
	}

	return mappings, nextAllocs, discoveredSnap, diffLogs, nil
}

// prepareInitialPorts builds the initial combined manual + auto-discovered VIP port mappings at startup.
func (tm *tailnetManager) prepareInitialPorts(ctx context.Context, cfg *config.Config, servers map[string]*tsnet.Server) ([]proxy.PortMapping, error) {
	tm.reloadMu.Lock()
	defer tm.reloadMu.Unlock()

	manualPorts := cfg.ToProxyPortMappings()
	vipPorts, nextAllocs, discoveredSnap, diffLogs, err := tm.computeVIPMappingsLocked(ctx, cfg, servers, true)
	if err != nil {
		return nil, err
	}

	combined := make([]proxy.PortMapping, 0, len(manualPorts)+len(vipPorts))
	combined = append(combined, manualPorts...)
	combined = append(combined, vipPorts...)

	tm.activeCfg = cfg
	tm.vipAllocations = nextAllocs
	tm.lastTailnetVIPs = discoveredSnap
	tm.appliedPorts = slices.Clone(combined)

	for _, line := range diffLogs {
		log.Println(line)
	}

	return combined, nil
}

func portMappingEqualExact(a, b proxy.PortMapping) bool {
	probeEqual := (a.Probe == nil && b.Probe == nil) ||
		(a.Probe != nil && b.Probe != nil && *a.Probe == *b.Probe)
	return a.Source == b.Source &&
		strings.TrimSpace(a.ListenAddress) == strings.TrimSpace(b.ListenAddress) &&
		a.Destination == b.Destination &&
		a.Tailnet == b.Tailnet &&
		a.VIPService == b.VIPService &&
		strings.EqualFold(a.Protocol, b.Protocol) &&
		a.ConnectionTimeout == b.ConnectionTimeout &&
		probeEqual
}

func portSlicesEqual(a, b []proxy.PortMapping) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !portMappingEqualExact(a[i], b[i]) {
			return false
		}
	}
	return true
}

// reconcileVIPs queries all auto_proxy_vips tailnets, allocates sticky local ports, and atomically
// reconciles the running proxy listeners (rolling back any partial bind on failure).
func (tm *tailnetManager) reconcileVIPs(ctx context.Context, proxyServer *proxy.Proxy) error {
	tm.reloadMu.Lock()
	defer tm.reloadMu.Unlock()

	if tm.activeCfg == nil {
		return nil
	}

	servers := tm.snapshotServers()
	vipPorts, nextAllocs, discoveredSnap, diffLogs, err := tm.computeVIPMappingsLocked(ctx, tm.activeCfg, servers, false)
	if err != nil {
		return err
	}

	manualPorts := tm.activeCfg.ToProxyPortMappings()
	desiredPorts := make([]proxy.PortMapping, 0, len(manualPorts)+len(vipPorts))
	desiredPorts = append(desiredPorts, manualPorts...)
	desiredPorts = append(desiredPorts, vipPorts...)

	prevApplied := tm.appliedPorts
	if prevApplied == nil {
		prevApplied = proxyServer.CurrentPorts()
	}

	if portSlicesEqual(prevApplied, desiredPorts) {
		tm.lastTailnetVIPs = discoveredSnap
		tm.appliedPorts = slices.Clone(desiredPorts)
		return nil
	}

	if err := proxyServer.ReloadConfig(desiredPorts); err != nil {
		if rbErr := proxyServer.ReloadConfig(slices.Clone(prevApplied)); rbErr != nil {
			log.Printf("Warning: error rolling back failed VIP reconciliation: %v", rbErr)
		}
		return fmt.Errorf("applying VIP port mappings failed (rolled back): %w", err)
	}

	tm.vipAllocations = nextAllocs
	tm.lastTailnetVIPs = discoveredSnap
	tm.appliedPorts = slices.Clone(desiredPorts)

	for _, line := range diffLogs {
		log.Println(line)
	}

	return nil
}

// triggerVIPReconcile signals the background VIP reconciler loop to run a debounced reconciliation.
func (tm *tailnetManager) triggerVIPReconcile() {
	tm.vipWatchMu.Lock()
	ch := tm.vipTriggerCh
	tm.vipWatchMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// startVIPReconciler launches per-tailnet IPN bus watchers and the background fallback ticker
// to keep auto-discovered VIP service listeners continuously synchronized at runtime.
func (tm *tailnetManager) startVIPReconciler(ctx context.Context, proxyServer *proxy.Proxy) {
	tm.reloadMu.Lock()
	cfg := tm.activeCfg
	tm.reloadMu.Unlock()

	servers := tm.snapshotServers()

	tm.vipWatchMu.Lock()
	if tm.reconcilerCancel != nil {
		tm.reconcilerCancel()
	}
	rCtx, rCancel := context.WithCancel(ctx)
	tm.reconcilerCtx = rCtx
	tm.reconcilerCancel = rCancel
	if tm.vipTriggerCh == nil {
		tm.vipTriggerCh = make(chan struct{}, 1)
	}
	if tm.vipTickerCh == nil {
		tm.vipTickerCh = make(chan time.Duration, 1)
	}
	if tm.vipWatchers == nil {
		tm.vipWatchers = make(map[string]vipWatcherEntry)
	}
	interval := resolveVIPRefreshInterval(tm, cfg)
	tm.vipInterval = interval
	triggerCh := tm.vipTriggerCh
	tickerCh := tm.vipTickerCh
	debounceDelay := tm.vipDebounceDelay
	if debounceDelay <= 0 {
		debounceDelay = 100 * time.Millisecond
	}
	tm.vipWatchMu.Unlock()

	if cfg != nil {
		tm.syncVIPWatchers(cfg, servers)
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		var debounceTimer *time.Timer
		var debounceC <-chan time.Time

		stopDebounce := func() {
			if debounceTimer != nil {
				if !debounceTimer.Stop() {
					select {
					case <-debounceTimer.C:
					default:
					}
				}
			}
			debounceC = nil
		}
		defer stopDebounce()

		runReconcile := func() {
			if rCtx.Err() != nil {
				return
			}
			if err := tm.reconcileVIPs(rCtx, proxyServer); err != nil && rCtx.Err() == nil {
				log.Printf("VIP service reconciliation failed: %v", err)
			}
		}

		for {
			select {
			case <-rCtx.Done():
				return
			case newInterval := <-tickerCh:
				if newInterval > 0 {
					ticker.Reset(newInterval)
				}
			case <-triggerCh:
				if debounceTimer == nil {
					debounceTimer = time.NewTimer(debounceDelay)
				} else {
					if !debounceTimer.Stop() {
						select {
						case <-debounceTimer.C:
						default:
						}
					}
					debounceTimer.Reset(debounceDelay)
				}
				debounceC = debounceTimer.C
			case <-debounceC:
				debounceC = nil
				runReconcile()
			case <-ticker.C:
				stopDebounce()
				runReconcile()
			}
		}
	}()
}

// syncVIPWatchers starts/stops per-tailnet WatchIPNBus goroutines and updates the fallback ticker interval
// when the active configuration or tailnet server map changes.
func (tm *tailnetManager) syncVIPWatchers(cfg *config.Config, servers map[string]*tsnet.Server) {
	tm.vipWatchMu.Lock()
	defer tm.vipWatchMu.Unlock()

	if tm.reconcilerCtx == nil || tm.reconcilerCtx.Err() != nil || cfg == nil {
		return
	}

	desired := make(map[string]*tsnet.Server)
	for _, tailnetCfg := range cfg.Tailnets {
		if !tailnetCfg.IsAutoProxyVIPs() {
			continue
		}
		if srv := servers[tailnetCfg.Name]; srv != nil && srv.Sys() != nil {
			desired[tailnetCfg.Name] = srv
		}
	}

	for name, existing := range tm.vipWatchers {
		wantSrv, ok := desired[name]
		if !ok || existing.srv != wantSrv {
			existing.cancel()
			delete(tm.vipWatchers, name)
		}
	}

	for name, srv := range desired {
		if _, exists := tm.vipWatchers[name]; exists {
			continue
		}
		wCtx, wCancel := context.WithCancel(tm.reconcilerCtx)
		tm.vipWatchers[name] = vipWatcherEntry{
			srv:    srv,
			cancel: wCancel,
		}
		go tm.watchTailnetIPNBus(wCtx, name, srv)
	}

	newInterval := resolveVIPRefreshInterval(tm, cfg)
	if newInterval != tm.vipInterval && newInterval > 0 {
		tm.vipInterval = newInterval
		select {
		case tm.vipTickerCh <- newInterval:
		default:
		}
	}
}

// stopVIPWatchers cancels the background VIP reconciler and all per-tailnet IPN bus watchers.
func (tm *tailnetManager) stopVIPWatchers() {
	tm.vipWatchMu.Lock()
	defer tm.vipWatchMu.Unlock()

	for name, entry := range tm.vipWatchers {
		entry.cancel()
		delete(tm.vipWatchers, name)
	}
	if tm.reconcilerCancel != nil {
		tm.reconcilerCancel()
		tm.reconcilerCancel = nil
		tm.reconcilerCtx = nil
	}
}

// watchTailnetIPNBus subscribes to Tailscale's local IPN notification bus for SelfChange / State events
// (which fire whenever the node comes online or its visible VIP services change) with zero peer-churn overhead.
func (tm *tailnetManager) watchTailnetIPNBus(ctx context.Context, _ string, srv *tsnet.Server) {
	if srv == nil || srv.Sys() == nil {
		return
	}

	backoff := 500 * time.Millisecond
	const maxBackoff = 15 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		lc, err := srv.LocalClient()
		if err == nil && lc != nil {
			watcher, wErr := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
			if wErr == nil {
				backoff = 500 * time.Millisecond
				for {
					n, nextErr := watcher.Next()
					if nextErr != nil {
						_ = watcher.Close()
						break
					}
					if n.SelfChange != nil || n.State != nil || n.LoginFinished != nil {
						tm.triggerVIPReconcile()
					}
				}
			}
		}

		if ctx.Err() != nil {
			return
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
