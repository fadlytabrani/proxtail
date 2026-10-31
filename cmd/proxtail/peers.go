package main

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

const tailnetHealthCacheTTL = 2 * time.Second

type cachedTailnetHealth struct {
	expiresAt   time.Time
	peers       []healthPeerStatus
	vipServices []healthVIPService
}

// healthPeerStatus describes a discovered peer on a tailnet for /healthz.
type healthPeerStatus struct {
	HostName string `json:"hostname"`
	DNSName  string `json:"dns_name,omitempty"`
	IPv4     string `json:"ipv4,omitempty"`
	IPv6     string `json:"ipv6,omitempty"`
	Online   bool   `json:"online"`
}

// healthVIPService describes a Tailnet Virtual IP / VIP Service for /healthz.
type healthVIPService struct {
	Name        string   `json:"name"`
	DNSName     string   `json:"dns_name,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
	IPv4        string   `json:"ipv4,omitempty"`
	IPv6        string   `json:"ipv6,omitempty"`
	Ports       []string `json:"ports,omitempty"`
}

// extractPeersFromStatus converts a Tailscale ipnstate.Status peer map into a
// deterministic, hostname-sorted slice of healthPeerStatus entries.
func extractPeersFromStatus(tsStatus *ipnstate.Status) []healthPeerStatus {
	if tsStatus == nil || len(tsStatus.Peer) == 0 {
		return nil
	}

	peers := make([]healthPeerStatus, 0, len(tsStatus.Peer))
	for _, peer := range tsStatus.Peer {
		if peer == nil {
			continue
		}
		var p4, p6 string
		for _, pip := range peer.TailscaleIPs {
			if pip.Is4() && p4 == "" {
				p4 = pip.String()
			} else if pip.Is6() && p6 == "" {
				p6 = pip.String()
			}
		}
		peers = append(peers, healthPeerStatus{
			HostName: strings.TrimSuffix(peer.HostName, "."),
			DNSName:  strings.TrimSuffix(peer.DNSName, "."),
			IPv4:     p4,
			IPv6:     p6,
			Online:   peer.Online,
		})
	}

	sort.Slice(peers, func(i, j int) bool {
		if peers[i].HostName != peers[j].HostName {
			return peers[i].HostName < peers[j].HostName
		}
		if peers[i].DNSName != peers[j].DNSName {
			return peers[i].DNSName < peers[j].DNSName
		}
		return peers[i].IPv4 < peers[j].IPv4
	})
	return peers
}

// extractVIPServices converts a map of Tailscale ServiceDetails into a
// deterministic, name-sorted slice of healthVIPService entries with MagicDNS FQDNs.
func extractVIPServices(svcs map[tailcfg.ServiceName]tailcfg.ServiceDetails, magicDNSSuffix string) []healthVIPService {
	if len(svcs) == 0 {
		return nil
	}

	magicDNSSuffix = strings.Trim(magicDNSSuffix, ".")

	result := make([]healthVIPService, 0, len(svcs))
	for _, svc := range svcs {
		var p4, p6 string
		for _, addr := range svc.Addrs {
			if addr.Is4() && p4 == "" {
				p4 = addr.String()
			} else if addr.Is6() && p6 == "" {
				p6 = addr.String()
			}
		}

		var ports []string
		for _, p := range svc.Ports {
			ports = append(ports, p.String())
		}
		sort.Strings(ports)

		bare := svc.Name.WithoutPrefix()
		name := bare
		if name == "" {
			name = string(svc.Name)
		}

		var dnsName string
		if bare != "" && magicDNSSuffix != "" {
			dnsName = bare + "." + magicDNSSuffix
		}

		result = append(result, healthVIPService{
			Name:        name,
			DNSName:     dnsName,
			DisplayName: svc.DisplayName,
			IPv4:        p4,
			IPv6:        p6,
			Ports:       ports,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].IPv4 < result[j].IPv4
	})
	return result
}

// discoverTailnetDetails queries the live tsnet LocalClient for tailnet peers and VIP services concurrently.
func discoverTailnetDetails(ctx context.Context, srv *tsnet.Server) ([]healthPeerStatus, []healthVIPService) {
	if srv == nil || srv.Sys() == nil {
		return nil, nil
	}
	lc, err := srv.LocalClient()
	if err != nil || lc == nil {
		return nil, nil
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

	var peers []healthPeerStatus
	var magicDNSSuffix string
	if stErr == nil && tsStatus != nil {
		peers = extractPeersFromStatus(tsStatus)
		if tsStatus.CurrentTailnet != nil && tsStatus.CurrentTailnet.MagicDNSSuffix != "" {
			magicDNSSuffix = tsStatus.CurrentTailnet.MagicDNSSuffix
		} else if tsStatus.MagicDNSSuffix != "" {
			magicDNSSuffix = tsStatus.MagicDNSSuffix
		}
	}

	var vipServices []healthVIPService
	if svcErr == nil && len(svcs) > 0 {
		vipServices = extractVIPServices(svcs, magicDNSSuffix)
	}

	return peers, vipServices
}

func (tm *tailnetManager) invalidateHealthCache() {
	tm.healthCacheMu.Lock()
	clear(tm.healthCache)
	tm.healthCacheMu.Unlock()
}

func (tm *tailnetManager) getCachedTailnetDetails(ctx context.Context, name string, srv *tsnet.Server) ([]healthPeerStatus, []healthVIPService) {
	now := time.Now()
	tm.healthCacheMu.Lock()
	if entry, ok := tm.healthCache[name]; ok && now.Before(entry.expiresAt) {
		peers := slices.Clone(entry.peers)
		vips := slices.Clone(entry.vipServices)
		tm.healthCacheMu.Unlock()
		return peers, vips
	}
	discoverFn := tm.healthDiscoverFunc
	tm.healthCacheMu.Unlock()

	if discoverFn == nil {
		discoverFn = func(qCtx context.Context, _ string, s *tsnet.Server) ([]healthPeerStatus, []healthVIPService) {
			return discoverTailnetDetails(qCtx, s)
		}
	}

	peers, vips := discoverFn(ctx, name, srv)

	tm.healthCacheMu.Lock()
	if tm.healthCache == nil {
		tm.healthCache = make(map[string]cachedTailnetHealth)
	}
	tm.healthCache[name] = cachedTailnetHealth{
		expiresAt:   time.Now().Add(tailnetHealthCacheTTL),
		peers:       slices.Clone(peers),
		vipServices: slices.Clone(vips),
	}
	tm.healthCacheMu.Unlock()

	return peers, vips
}
