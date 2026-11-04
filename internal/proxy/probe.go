package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// discoPingProvider is an optional interface that Dialer implementations can
// satisfy (alongside *tsnet.Server) to provide Tailscale-layer Disco path
// probing in tests or custom backends.
type discoPingProvider interface {
	PingDisco(ctx context.Context, ip netip.Addr) (*ipnstate.PingResult, error)
}

// tsmpPingProvider is an optional interface that Dialer implementations can
// satisfy (alongside *tsnet.Server) to provide Tailscale Message Protocol (TSMP)
// probing for ACL packet filter rejection diagnostics.
type tsmpPingProvider interface {
	PingTSMP(ctx context.Context, ip netip.Addr) (*ipnstate.PingResult, error)
}

// ProbeResult describes the result of an on-demand pre-flight reachability and
// Tailscale path probe to a candidate destination on a tailnet profile.
type ProbeResult struct {
	Stage              string  `json:"Stage,omitempty"`
	Profile            string  `json:"Profile"`
	Protocol           string  `json:"Protocol"`
	Destination        string  `json:"Destination"`
	PeerIP             string  `json:"PeerIP,omitempty"`
	PeerOnline         bool    `json:"PeerOnline"`
	Reachable          bool    `json:"Reachable"`
	ACLBlocked         bool    `json:"ACLBlocked,omitempty"`
	Error              string  `json:"Error,omitempty"`
	DialLatencySeconds float64 `json:"DialLatencySeconds,omitempty"`
	Path               string  `json:"Path,omitempty"`
	Endpoint           string  `json:"Endpoint,omitempty"`
	DERPRegionID       int     `json:"DERPRegionID,omitempty"`
	DERPRegionCode     string  `json:"DERPRegionCode,omitempty"`
	LatencySeconds     float64 `json:"LatencySeconds,omitempty"`
}

// peerAddrProvider is an optional interface that Dialer implementations can
// satisfy to resolve peer hostnames, DNS names, VIPs, or subnet prefixes to
// their backing Tailscale peer IP address for Disco probing.
type peerAddrProvider interface {
	PeerAddrMap(ctx context.Context) (map[string]netip.Addr, error)
}

// peerPathInfo holds the most recent Tailscale-layer Disco path telemetry for a listener.
type peerPathInfo struct {
	path           string
	endpoint       string
	derpRegionID   int
	derpRegionCode string
	latencySeconds float64
}

type discoCacheKey struct {
	dialer Dialer
	addr   netip.Addr
}

type discoCacheEntry struct {
	expiresAt time.Time
	info      peerPathInfo
	ok        bool
}

type discoInFlightCall struct {
	done chan struct{}
	info peerPathInfo
	ok   bool
}

func applyPathInfo(st *PortStatus, info peerPathInfo) {
	if st == nil || info.path == "" {
		return
	}
	st.Path = info.path
	st.Endpoint = info.endpoint
	st.DERPRegionID = info.derpRegionID
	st.DERPRegionCode = info.derpRegionCode
	st.LatencySeconds = info.latencySeconds
}

func classifyDiscoPingResult(pr *ipnstate.PingResult) (peerPathInfo, bool) {
	if pr == nil || strings.TrimSpace(pr.Err) != "" {
		return peerPathInfo{}, false
	}

	endpoint := strings.TrimSpace(pr.Endpoint)
	derpCode := strings.TrimSpace(pr.DERPRegionCode)

	var pathType string
	switch {
	case pr.IsLocalIP:
		pathType = "local"
	case endpoint != "":
		pathType = "direct"
	case derpCode != "" || pr.DERPRegionID > 0:
		pathType = "derp"
	default:
		return peerPathInfo{}, false
	}

	return peerPathInfo{
		path:           pathType,
		endpoint:       endpoint,
		derpRegionID:   int(pr.DERPRegionID),
		derpRegionCode: derpCode,
		latencySeconds: pr.LatencySeconds,
	}, true
}

func primaryPeerTailscaleIP(peer *ipnstate.PeerStatus) netip.Addr {
	if peer == nil {
		return netip.Addr{}
	}
	var fallback netip.Addr
	for _, ip := range peer.TailscaleIPs {
		if !ip.IsValid() {
			continue
		}
		if ip.Is4() {
			return ip
		}
		if !fallback.IsValid() {
			fallback = ip
		}
	}
	return fallback
}

func buildPeerAddrMap(st *ipnstate.Status) map[string]netip.Addr {
	if st == nil || len(st.Peer) == 0 {
		return nil
	}
	addrs := make(map[string]netip.Addr, len(st.Peer)*3)
	addrOnline := make(map[string]bool, len(st.Peer)*3)

	recordAddr := func(k string, ip netip.Addr, online bool) {
		if k == "" || !ip.IsValid() {
			return
		}
		if existingOnline, exists := addrOnline[k]; !exists || (online && !existingOnline) {
			addrs[k] = ip
			addrOnline[k] = existingOnline || online
		}
	}

	for _, peer := range st.Peer {
		if peer == nil {
			continue
		}
		primaryIP := primaryPeerTailscaleIP(peer)
		if !primaryIP.IsValid() {
			continue
		}
		peerOnline := peer.Online && !peer.Expired

		dnsClean := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(peer.DNSName), "."))
		if dnsClean != "" {
			recordAddr(dnsClean, primaryIP, peerOnline)
			if shortLabel, _, ok := strings.Cut(dnsClean, "."); ok && shortLabel != "" {
				recordAddr(shortLabel, primaryIP, peerOnline)
			}
		}
		hostClean := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(peer.HostName), "."))
		if hostClean != "" {
			recordAddr(hostClean, primaryIP, peerOnline)
		}
		for _, ip := range peer.TailscaleIPs {
			if ip.IsValid() {
				recordAddr(strings.ToLower(ip.String()), ip, peerOnline)
			}
		}
		if peer.PrimaryRoutes != nil {
			for i := 0; i < peer.PrimaryRoutes.Len(); i++ {
				pfx := peer.PrimaryRoutes.At(i)
				if !pfx.IsValid() {
					continue
				}
				if pfx.IsSingleIP() {
					recordAddr(strings.ToLower(pfx.Addr().String()), primaryIP, peerOnline)
				} else {
					recordAddr(strings.ToLower(pfx.Masked().String()), primaryIP, peerOnline)
				}
			}
		}
	}
	return addrs
}

func canProbeDisco(dialer Dialer) bool {
	if dialer == nil {
		return false
	}
	if _, ok := dialer.(discoPingProvider); ok {
		return true
	}
	tsSrv, isTS := dialer.(*tsnet.Server)
	return isTS && tsSrv != nil && tsSrv.Sys() != nil
}

func (p *Proxy) discoCacheTTL() time.Duration {
	p.mu.RLock()
	interval := p.probeConfig.Interval
	p.mu.RUnlock()

	ttl := peerStatusCacheTTL
	if interval > 0 && interval/2 < ttl {
		ttl = interval / 2
	}
	if ttl <= 0 {
		ttl = time.Millisecond
	}
	return ttl
}

func (p *Proxy) resolvePeerIP(ctx context.Context, dialer Dialer, destination string) (netip.Addr, bool) {
	destHost, _, splitErr := net.SplitHostPort(destination)
	if splitErr != nil || destHost == "" {
		destHost = destination
	}
	destHostClean := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(destHost), "."))
	destHostClean = strings.TrimPrefix(strings.TrimSuffix(destHostClean, "]"), "[")
	if destHostClean == "" {
		return netip.Addr{}, false
	}

	if snap, ok := p.getPeerSnapshot(ctx, dialer); ok {
		if addr, found := snap.peerAddrs[destHostClean]; found && addr.IsValid() {
			return addr, true
		}
		if len(snap.routes) > 0 {
			if destAddr, err := netip.ParseAddr(destHostClean); err == nil && destAddr.IsValid() {
				bestBits := -1
				bestOnline := false
				var bestPeerAddr netip.Addr
				for _, r := range snap.routes {
					if !r.prefix.Contains(destAddr) || !r.peerAddr.IsValid() {
						continue
					}
					bits := r.prefix.Bits()
					if bits > bestBits || (bits == bestBits && r.online && !bestOnline) {
						bestBits = bits
						bestOnline = r.online
						bestPeerAddr = r.peerAddr
					}
				}
				if bestBits >= 0 && bestPeerAddr.IsValid() {
					return bestPeerAddr, true
				}
			}
		}
	}

	if addr, err := netip.ParseAddr(destHostClean); err == nil && addr.IsValid() {
		return addr, true
	}
	return netip.Addr{}, false
}

func pingDisco(ctx context.Context, dialer Dialer, ip netip.Addr) (*ipnstate.PingResult, error) {
	if provider, ok := dialer.(discoPingProvider); ok {
		return provider.PingDisco(ctx, ip)
	}
	tsSrv, isTS := dialer.(*tsnet.Server)
	if !isTS || tsSrv == nil || tsSrv.Sys() == nil {
		return nil, errors.New("dialer does not support tailscale disco ping")
	}
	lc, err := tsSrv.LocalClient()
	if err != nil || lc == nil {
		if err == nil {
			err = errors.New("tailscale local client unavailable")
		}
		return nil, err
	}
	return lc.Ping(ctx, ip, tailcfg.PingDisco)
}

// probeDiscoPath performs a Tailscale-layer Disco ping to the peer backing destination
// and returns the classified path information (direct UDP endpoint vs DERP relay).
// Results are coalesced and cached per (Dialer, peer IP) so multiple ports targeting
// the same backend peer share a single Disco ping per probe cycle.
func (p *Proxy) probeDiscoPath(ctx context.Context, dialer Dialer, destination string) (peerPathInfo, bool) {
	if !canProbeDisco(dialer) {
		return peerPathInfo{}, false
	}

	peerIP, ok := p.resolvePeerIP(ctx, dialer, destination)
	if !ok || !peerIP.IsValid() {
		return peerPathInfo{}, false
	}

	cacheKey := discoCacheKey{
		dialer: dialer,
		addr:   peerIP,
	}

	p.peerCacheMu.Lock()
	if entry, found := p.discoCache[cacheKey]; found && time.Now().Before(entry.expiresAt) {
		p.peerCacheMu.Unlock()
		return entry.info, entry.ok
	}
	if call, inFlight := p.discoInFlight[cacheKey]; inFlight {
		p.peerCacheMu.Unlock()
		select {
		case <-call.done:
			return call.info, call.ok
		case <-ctx.Done():
			return peerPathInfo{}, false
		}
	}
	call := &discoInFlightCall{done: make(chan struct{})}
	if p.discoInFlight == nil {
		p.discoInFlight = make(map[discoCacheKey]*discoInFlightCall)
	}
	p.discoInFlight[cacheKey] = call
	p.peerCacheMu.Unlock()

	pr, err := pingDisco(ctx, dialer, peerIP)
	var info peerPathInfo
	var hasPath bool
	if err == nil {
		info, hasPath = classifyDiscoPingResult(pr)
	}
	call.info = info
	call.ok = hasPath

	p.peerCacheMu.Lock()
	if err == nil {
		if p.discoCache == nil {
			p.discoCache = make(map[discoCacheKey]discoCacheEntry)
		}
		p.discoCache[cacheKey] = discoCacheEntry{
			expiresAt: time.Now().Add(p.discoCacheTTL()),
			info:      info,
			ok:        hasPath,
		}
	}
	delete(p.discoInFlight, cacheKey)
	close(call.done)
	p.peerCacheMu.Unlock()

	return info, hasPath
}

func applyProbeResultPath(res *ProbeResult, info peerPathInfo) {
	if res == nil || info.path == "" {
		return
	}
	res.Path = info.path
	res.Endpoint = info.endpoint
	res.DERPRegionID = info.derpRegionID
	res.DERPRegionCode = info.derpRegionCode
	res.LatencySeconds = info.latencySeconds
}

func canProbeTSMP(dialer Dialer) bool {
	if dialer == nil {
		return false
	}
	if _, ok := dialer.(tsmpPingProvider); ok {
		return true
	}
	tsSrv, isTS := dialer.(*tsnet.Server)
	return isTS && tsSrv != nil && tsSrv.Sys() != nil
}

func pingTSMP(ctx context.Context, dialer Dialer, ip netip.Addr) (*ipnstate.PingResult, error) {
	if provider, ok := dialer.(tsmpPingProvider); ok {
		return provider.PingTSMP(ctx, ip)
	}
	tsSrv, isTS := dialer.(*tsnet.Server)
	if !isTS || tsSrv == nil || tsSrv.Sys() == nil {
		return nil, errors.New("dialer does not support tailscale TSMP ping")
	}
	lc, err := tsSrv.LocalClient()
	if err != nil || lc == nil {
		if err == nil {
			err = errors.New("tailscale local client unavailable")
		}
		return nil, err
	}
	return lc.Ping(ctx, ip, tailcfg.PingTSMP)
}

func isACLBlockedError(msg string) bool {
	lower := strings.ToLower(strings.TrimSpace(msg))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "filter blocked") ||
		strings.Contains(lower, "packet filter") ||
		strings.Contains(lower, "administratively prohibited") ||
		strings.Contains(lower, "acl") ||
		strings.Contains(lower, "rejected")
}

// HasTailnet reports whether the proxy knows about the given tailnet profile
// (either as an active dialer or as a recorded initialization error).
func (p *Proxy) HasTailnet(profile string) bool {
	profile = strings.TrimSpace(profile)
	p.mu.RLock()
	_, hasDialer := p.tailnetDialers[profile]
	_, hasInitErr := p.tailnetErrors[profile]
	p.mu.RUnlock()
	return hasDialer || hasInitErr
}

// ProbeDestination performs an on-demand pre-flight check to verify whether a candidate
// destination (host:port) on the specified tailnet profile is online, reachable over the
// requested protocol ("tcp" or "udp"), permitted by Tailscale ACLs, and whether traffic
// uses a direct P2P UDP path or a DERP relay.
// The boolean return value is false only when the requested tailnet profile is unknown.
func (p *Proxy) ProbeDestination(ctx context.Context, profile, protocol, destination string, timeout time.Duration) (ProbeResult, bool) {
	res, found := p.ProbeDestinationStream(ctx, profile, protocol, destination, timeout, nil)
	res.Stage = ""
	return res, found
}

// ProbeDestinationStream executes the 3-stage pre-flight probe ("netmap", "path", and "port"),
// running the L4 port probe and Tailscale Disco path probe concurrently after the netmap stage
// and invoking onStage (when non-nil) as each stage completes with a cumulative ProbeResult snapshot.
func (p *Proxy) ProbeDestinationStream(ctx context.Context, profile, protocol, destination string, timeout time.Duration, onStage func(ProbeResult)) (ProbeResult, bool) {
	profile = strings.TrimSpace(profile)
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "" {
		protocol = "tcp"
	}
	destination = strings.TrimSpace(destination)

	emit := func(stage string, snapshot ProbeResult) {
		if onStage == nil {
			return
		}
		snapshot.Stage = stage
		onStage(snapshot)
	}

	res := ProbeResult{
		Profile:     profile,
		Protocol:    protocol,
		Destination: destination,
	}

	p.mu.RLock()
	dialer, hasDialer := p.tailnetDialers[profile]
	initErr, hasInitErr := p.tailnetErrors[profile]
	p.mu.RUnlock()

	if !hasDialer {
		if hasInitErr {
			res.PeerOnline = false
			res.Reachable = false
			res.Error = fmt.Sprintf("tailnet '%s' unavailable: %s", profile, initErr)
			emit("netmap", res)
			return res, true
		}
		res.Error = fmt.Sprintf("tailnet profile '%s' not found", profile)
		return res, false
	}

	if timeout <= 0 {
		_, timeout, _, _, _ = p.getProbeSettings()
	}
	if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}

	peerIP, hasPeerIP := p.resolvePeerIP(ctx, dialer, destination)
	if hasPeerIP && peerIP.IsValid() {
		res.PeerIP = peerIP.String()
	}

	if offline, reason := p.isPeerOffline(ctx, dialer, destination); offline {
		res.PeerOnline = false
		res.Reachable = false
		res.Error = reason
		emit("netmap", res)
		return res, true
	}
	res.PeerOnline = true
	emit("netmap", res)

	type discoOutcome struct {
		info peerPathInfo
		ok   bool
	}
	type portOutcome struct {
		reachable   bool
		dialElapsed float64
		aclBlocked  bool
		errStr      string
	}

	var discoCh chan discoOutcome
	if ctx.Err() == nil && canProbeDisco(dialer) {
		discoCh = make(chan discoOutcome, 1)
		go func() {
			discoCtx, discoCancel := context.WithTimeout(ctx, timeout)
			defer discoCancel()
			info, ok := p.probeDiscoPath(discoCtx, dialer, destination)
			discoCh <- discoOutcome{info: info, ok: ok}
		}()
	}

	portCh := make(chan portOutcome, 1)
	go func() {
		probeCtx, probeCancel := context.WithTimeout(ctx, timeout)
		dialStart := time.Now()
		var dialErr error
		if protocol == "udp" {
			dialErr = p.probeUDPUpstream(probeCtx, dialer, destination)
		} else {
			var conn net.Conn
			conn, dialErr = dialer.Dial(probeCtx, "tcp", destination)
			if dialErr == nil {
				_ = conn.Close()
			}
		}
		dialElapsed := time.Since(dialStart).Seconds()
		probeCancel()

		if dialErr == nil {
			portCh <- portOutcome{
				reachable:   true,
				dialElapsed: dialElapsed,
			}
			return
		}

		out := portOutcome{
			reachable:  false,
			errStr:     dialErr.Error(),
			aclBlocked: isACLBlockedError(dialErr.Error()),
		}

		diagTimeout := timeout
		if diagTimeout > 500*time.Millisecond {
			diagTimeout = 500 * time.Millisecond
		}
		if !out.aclBlocked && ctx.Err() == nil && hasPeerIP && peerIP.IsValid() && canProbeTSMP(dialer) {
			tsmpCtx, tsmpCancel := context.WithTimeout(ctx, diagTimeout)
			if pr, tsmpErr := pingTSMP(tsmpCtx, dialer, peerIP); tsmpErr == nil && pr != nil && strings.TrimSpace(pr.Err) != "" {
				tsmpMsg := strings.TrimSpace(pr.Err)
				if isACLBlockedError(tsmpMsg) {
					out.aclBlocked = true
					out.errStr = fmt.Sprintf("%s (%s)", out.errStr, tsmpMsg)
				}
			}
			tsmpCancel()
		}
		portCh <- out
	}()

	for discoCh != nil || portCh != nil {
		select {
		case dOut := <-discoCh:
			discoCh = nil
			if dOut.ok {
				applyProbeResultPath(&res, dOut.info)
				emit("path", res)
			}
		case pOut := <-portCh:
			portCh = nil
			res.Reachable = pOut.reachable
			res.DialLatencySeconds = pOut.dialElapsed
			res.ACLBlocked = pOut.aclBlocked
			res.Error = pOut.errStr
			emit("port", res)
		}
	}

	return res, true
}
