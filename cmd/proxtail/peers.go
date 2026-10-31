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

// tailnetHealthDetails holds the cached Tier-0 status details for a single tailnet.
type tailnetHealthDetails struct {
	TailnetName     string             `json:"tailnet_name,omitempty"`
	MagicDNSSuffix  string             `json:"magic_dns_suffix,omitempty"`
	MagicDNSEnabled bool               `json:"magic_dns_enabled"`
	Self            healthSelfStatus   `json:"self"`
	VIPServices     []healthVIPService `json:"vip_services,omitempty"`
	Peers           []healthPeerStatus `json:"peers,omitempty"`
}

type cachedTailnetHealth struct {
	expiresAt time.Time
	details   tailnetHealthDetails
}

// healthSelfStatus describes ProxTail's own node state on a tailnet for /healthz.
type healthSelfStatus struct {
	ID             string   `json:"id,omitempty"`
	HostName       string   `json:"hostname,omitempty"`
	DNSName        string   `json:"dns_name,omitempty"`
	IPv4           string   `json:"ipv4,omitempty"`
	IPv6           string   `json:"ipv6,omitempty"`
	BackendState   string   `json:"backend_state,omitempty"`
	Online         bool     `json:"online"`
	Relay          string   `json:"relay,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	KeyExpiry      string   `json:"key_expiry,omitempty"`
	CertDomains    []string `json:"cert_domains,omitempty"`
	HealthWarnings []string `json:"health_warnings,omitempty"`
}

// healthPeerStatus describes a discovered peer on a tailnet for /healthz.
type healthPeerStatus struct {
	ID            string   `json:"id,omitempty"`
	HostName      string   `json:"hostname"`
	DNSName       string   `json:"dns_name,omitempty"`
	IPv4          string   `json:"ipv4,omitempty"`
	IPv6          string   `json:"ipv6,omitempty"`
	Online        bool     `json:"online"`
	Active        bool     `json:"active"`
	CurAddr       string   `json:"cur_addr,omitempty"`
	Relay         string   `json:"relay,omitempty"`
	RxBytes       int64    `json:"rx_bytes,omitempty"`
	TxBytes       int64    `json:"tx_bytes,omitempty"`
	LastHandshake string   `json:"last_handshake,omitempty"`
	LastSeen      string   `json:"last_seen,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	PrimaryRoutes []string `json:"primary_routes,omitempty"`
	Expired       bool     `json:"expired,omitempty"`
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

// extractSelfFromStatus extracts ProxTail's own node metadata on a tailnet from
// ipnstate.Status, falling back to srv fields when LocalClient status is unavailable.
func extractSelfFromStatus(tsStatus *ipnstate.Status, srv *tsnet.Server) healthSelfStatus {
	var self healthSelfStatus
	if srv != nil {
		self.HostName = srv.Hostname
		if srv.Sys() != nil {
			ip4, ip6 := srv.TailscaleIPs()
			if ip4.IsValid() {
				self.IPv4 = ip4.String()
			}
			if ip6.IsValid() {
				self.IPv6 = ip6.String()
			}
		}
	}
	if tsStatus == nil {
		return self
	}

	self.BackendState = tsStatus.BackendState
	if len(tsStatus.CertDomains) > 0 {
		self.CertDomains = slices.Clone(tsStatus.CertDomains)
	}
	if len(tsStatus.Health) > 0 {
		self.HealthWarnings = slices.Clone(tsStatus.Health)
	}

	for _, ip := range tsStatus.TailscaleIPs {
		if ip.Is4() && self.IPv4 == "" {
			self.IPv4 = ip.String()
		} else if ip.Is6() && self.IPv6 == "" {
			self.IPv6 = ip.String()
		}
	}

	if s := tsStatus.Self; s != nil {
		self.ID = string(s.ID)
		if h := strings.TrimSuffix(s.HostName, "."); h != "" {
			self.HostName = h
		}
		self.DNSName = strings.TrimSuffix(s.DNSName, ".")
		for _, ip := range s.TailscaleIPs {
			if ip.Is4() && self.IPv4 == "" {
				self.IPv4 = ip.String()
			} else if ip.Is6() && self.IPv6 == "" {
				self.IPv6 = ip.String()
			}
		}
		self.Online = s.Online
		self.Relay = s.Relay
		if s.Tags != nil && s.Tags.Len() > 0 {
			self.Tags = slices.Clone(s.Tags.AsSlice())
		}
		if s.KeyExpiry != nil && !s.KeyExpiry.IsZero() {
			self.KeyExpiry = s.KeyExpiry.UTC().Format(time.RFC3339)
		}
	}

	return self
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
		var lastHandshake, lastSeen string
		if !peer.LastHandshake.IsZero() {
			lastHandshake = peer.LastHandshake.UTC().Format(time.RFC3339)
		}
		if !peer.LastSeen.IsZero() {
			lastSeen = peer.LastSeen.UTC().Format(time.RFC3339)
		}
		var tags []string
		if peer.Tags != nil && peer.Tags.Len() > 0 {
			tags = slices.Clone(peer.Tags.AsSlice())
		}
		var primaryRoutes []string
		if peer.PrimaryRoutes != nil && peer.PrimaryRoutes.Len() > 0 {
			primaryRoutes = make([]string, 0, peer.PrimaryRoutes.Len())
			for i := range peer.PrimaryRoutes.Len() {
				primaryRoutes = append(primaryRoutes, peer.PrimaryRoutes.At(i).String())
			}
			sort.Strings(primaryRoutes)
		}
		peers = append(peers, healthPeerStatus{
			ID:            string(peer.ID),
			HostName:      strings.TrimSuffix(peer.HostName, "."),
			DNSName:       strings.TrimSuffix(peer.DNSName, "."),
			IPv4:          p4,
			IPv6:          p6,
			Online:        peer.Online,
			Active:        peer.Active,
			CurAddr:       peer.CurAddr,
			Relay:         peer.Relay,
			RxBytes:       peer.RxBytes,
			TxBytes:       peer.TxBytes,
			LastHandshake: lastHandshake,
			LastSeen:      lastSeen,
			Tags:          tags,
			PrimaryRoutes: primaryRoutes,
			Expired:       peer.Expired,
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

// extractTailnetDetailsFromStatus combines ipnstate.Status and VIP ServiceDetails
// into a complete tailnetHealthDetails snapshot.
func extractTailnetDetailsFromStatus(tsStatus *ipnstate.Status, svcs map[tailcfg.ServiceName]tailcfg.ServiceDetails, srv *tsnet.Server) tailnetHealthDetails {
	var details tailnetHealthDetails
	details.Self = extractSelfFromStatus(tsStatus, srv)

	var rawMagicSuffix string
	if tsStatus != nil {
		details.Peers = extractPeersFromStatus(tsStatus)
		if tsStatus.CurrentTailnet != nil {
			details.TailnetName = tsStatus.CurrentTailnet.Name
			details.MagicDNSEnabled = tsStatus.CurrentTailnet.MagicDNSEnabled
			rawMagicSuffix = tsStatus.CurrentTailnet.MagicDNSSuffix
		}
		if rawMagicSuffix == "" {
			rawMagicSuffix = tsStatus.MagicDNSSuffix
		}
		details.MagicDNSSuffix = strings.Trim(rawMagicSuffix, ".")
	}

	if len(svcs) > 0 {
		details.VIPServices = extractVIPServices(svcs, details.MagicDNSSuffix)
	}

	return details
}

func cloneTailnetHealthDetails(d tailnetHealthDetails) tailnetHealthDetails {
	out := d
	out.Self.Tags = slices.Clone(d.Self.Tags)
	out.Self.CertDomains = slices.Clone(d.Self.CertDomains)
	out.Self.HealthWarnings = slices.Clone(d.Self.HealthWarnings)
	if d.VIPServices != nil {
		out.VIPServices = make([]healthVIPService, len(d.VIPServices))
		for i, v := range d.VIPServices {
			v.Ports = slices.Clone(v.Ports)
			out.VIPServices[i] = v
		}
	}
	if d.Peers != nil {
		out.Peers = make([]healthPeerStatus, len(d.Peers))
		for i, p := range d.Peers {
			p.Tags = slices.Clone(p.Tags)
			p.PrimaryRoutes = slices.Clone(p.PrimaryRoutes)
			out.Peers[i] = p
		}
	}
	return out
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
	if details.Self.HostName == "" && srv != nil {
		details.Self.HostName = srv.Hostname
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
