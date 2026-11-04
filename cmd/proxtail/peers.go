package main

import (
	"context"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/types/key"
)

const tailnetHealthCacheTTL = 2 * time.Second

// tailnetHealthDetails holds the cached LocalAPI status and VIP services for a single tailnet.
type tailnetHealthDetails struct {
	Status   *ipnstate.Status                               `json:"Status,omitempty"`
	Services map[tailcfg.ServiceName]tailcfg.ServiceDetails `json:"Services,omitempty"`
}

type cachedTailnetHealth struct {
	expiresAt time.Time
	details   tailnetHealthDetails
}

func clonePeerStatus(p *ipnstate.PeerStatus) *ipnstate.PeerStatus {
	if p == nil {
		return nil
	}
	cp := *p
	cp.TailscaleIPs = slices.Clone(p.TailscaleIPs)
	cp.Addrs = slices.Clone(p.Addrs)
	cp.PeerAPIURL = slices.Clone(p.PeerAPIURL)
	cp.Capabilities = slices.Clone(p.Capabilities)
	cp.CapMap = maps.Clone(p.CapMap)
	cp.SSH_HostKeys = slices.Clone(p.SSH_HostKeys)
	if p.KeyExpiry != nil {
		t := *p.KeyExpiry
		cp.KeyExpiry = &t
	}
	return &cp
}

func cloneIPNStatus(s *ipnstate.Status) *ipnstate.Status {
	if s == nil {
		return nil
	}
	cp := *s
	cp.TailscaleIPs = slices.Clone(s.TailscaleIPs)
	cp.Self = clonePeerStatus(s.Self)
	if s.ExitNodeStatus != nil {
		en := *s.ExitNodeStatus
		en.TailscaleIPs = slices.Clone(s.ExitNodeStatus.TailscaleIPs)
		cp.ExitNodeStatus = &en
	}
	cp.Health = slices.Clone(s.Health)
	if s.CurrentTailnet != nil {
		ct := *s.CurrentTailnet
		cp.CurrentTailnet = &ct
	}
	cp.CertDomains = slices.Clone(s.CertDomains)
	if s.Peer != nil {
		cp.Peer = make(map[key.NodePublic]*ipnstate.PeerStatus, len(s.Peer))
		for k, v := range s.Peer {
			cp.Peer[k] = clonePeerStatus(v)
		}
	}
	cp.User = maps.Clone(s.User)
	if s.ClientVersion != nil {
		cv := *s.ClientVersion
		cp.ClientVersion = &cv
	}
	return &cp
}

func cloneServicesMap(svcs map[tailcfg.ServiceName]tailcfg.ServiceDetails) map[tailcfg.ServiceName]tailcfg.ServiceDetails {
	if len(svcs) == 0 {
		return nil
	}
	out := make(map[tailcfg.ServiceName]tailcfg.ServiceDetails, len(svcs))
	for k, v := range svcs {
		v.Addrs = slices.Clone(v.Addrs)
		v.Ports = slices.Clone(v.Ports)
		out[k] = v
	}
	return out
}

// extractTailnetDetailsFromStatus prepares a tailnetHealthDetails snapshot from
// Tailscale's native ipnstate.Status and VIP ServiceDetails, falling back to srv
// metadata when LocalClient status fields are unavailable.
func extractTailnetDetailsFromStatus(tsStatus *ipnstate.Status, svcs map[tailcfg.ServiceName]tailcfg.ServiceDetails, srv *tsnet.Server) tailnetHealthDetails {
	st := cloneIPNStatus(tsStatus)
	if st == nil {
		st = &ipnstate.Status{}
	}
	if st.Self == nil {
		st.Self = &ipnstate.PeerStatus{}
	}

	if srv != nil {
		if st.Self.HostName == "" {
			st.Self.HostName = srv.Hostname
		}
		if srv.Sys() != nil {
			ip4, ip6 := srv.TailscaleIPs()
			var fallbackIPs []netip.Addr
			if ip4.IsValid() {
				fallbackIPs = append(fallbackIPs, ip4)
			}
			if ip6.IsValid() {
				fallbackIPs = append(fallbackIPs, ip6)
			}
			if len(st.TailscaleIPs) == 0 && len(fallbackIPs) > 0 {
				st.TailscaleIPs = slices.Clone(fallbackIPs)
			}
			if len(st.Self.TailscaleIPs) == 0 && len(fallbackIPs) > 0 {
				st.Self.TailscaleIPs = slices.Clone(fallbackIPs)
			}
		}
	}

	if len(st.TailscaleIPs) == 0 && len(st.Self.TailscaleIPs) > 0 {
		st.TailscaleIPs = slices.Clone(st.Self.TailscaleIPs)
	} else if len(st.Self.TailscaleIPs) == 0 && len(st.TailscaleIPs) > 0 {
		st.Self.TailscaleIPs = slices.Clone(st.TailscaleIPs)
	}

	return tailnetHealthDetails{
		Status:   st,
		Services: cloneServicesMap(svcs),
	}
}

func cloneTailnetHealthDetails(d tailnetHealthDetails) tailnetHealthDetails {
	return tailnetHealthDetails{
		Status:   cloneIPNStatus(d.Status),
		Services: cloneServicesMap(d.Services),
	}
}

// discoverTailnetDetails queries the live tsnet LocalClient for tailnet status and VIP services concurrently.
func discoverTailnetDetails(ctx context.Context, srv *tsnet.Server) tailnetHealthDetails {
	if srv == nil || srv.Sys() == nil {
		return extractTailnetDetailsFromStatus(nil, nil, srv)
	}
	lc, err := srv.LocalClient()
	if err != nil || lc == nil {
		return extractTailnetDetailsFromStatus(nil, nil, srv)
	}
	stCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var (
		tsStatus *ipnstate.Status
		stErr    error
		svcs     map[tailcfg.ServiceName]tailcfg.ServiceDetails
		svcErr   error
		wg       sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		tsStatus, stErr = lc.Status(stCtx)
	}()
	go func() {
		defer wg.Done()
		svcs, svcErr = lc.GetServices(stCtx)
	}()
	wg.Wait()

	if stErr != nil {
		tsStatus = nil
	}
	if svcErr != nil {
		svcs = nil
	}

	return extractTailnetDetailsFromStatus(tsStatus, svcs, srv)
}

func (tm *tailnetManager) invalidateHealthCache() {
	tm.healthCacheMu.Lock()
	clear(tm.healthCache)
	tm.healthCacheMu.Unlock()
}

func (tm *tailnetManager) invalidateTailnetHealthCache(tailnetName string) {
	tm.healthCacheMu.Lock()
	if tm.healthCache != nil {
		delete(tm.healthCache, tailnetName)
	}
	tm.healthCacheMu.Unlock()
}

func (tm *tailnetManager) getCachedTailnetDetails(ctx context.Context, name string, srv *tsnet.Server) tailnetHealthDetails {
	now := time.Now()
	tm.healthCacheMu.Lock()
	if entry, ok := tm.healthCache[name]; ok && now.Before(entry.expiresAt) {
		cloned := cloneTailnetHealthDetails(entry.details)
		tm.healthCacheMu.Unlock()
		return cloned
	}
	discoverFn := tm.healthDiscoverFunc
	tm.healthCacheMu.Unlock()

	if discoverFn == nil {
		discoverFn = func(qCtx context.Context, _ string, s *tsnet.Server) tailnetHealthDetails {
			return discoverTailnetDetails(qCtx, s)
		}
	}

	details := discoverFn(ctx, name, srv)
	if details.Status == nil {
		details.Status = &ipnstate.Status{}
	}
	if details.Status.Self == nil {
		details.Status.Self = &ipnstate.PeerStatus{}
	}
	if details.Status.Self.HostName == "" && srv != nil {
		details.Status.Self.HostName = srv.Hostname
	}

	tm.healthCacheMu.Lock()
	if tm.healthCache == nil {
		tm.healthCache = make(map[string]cachedTailnetHealth)
	}
	tm.healthCache[name] = cachedTailnetHealth{
		expiresAt: time.Now().Add(tailnetHealthCacheTTL),
		details:   cloneTailnetHealthDetails(details),
	}
	tm.healthCacheMu.Unlock()

	return details
}
