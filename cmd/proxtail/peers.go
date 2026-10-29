package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// healthPeerStatus describes a discovered peer on a tailnet for /healthz.
type healthPeerStatus struct {
	HostName string `json:"hostname"`
	DNSName  string `json:"dns_name,omitempty"`
	IPv4     string `json:"ipv4,omitempty"`
	IPv6     string `json:"ipv6,omitempty"`
	Online   bool   `json:"online"`
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

// discoverTailnetPeers queries the live tsnet LocalClient for tailnet peer status.
func discoverTailnetPeers(ctx context.Context, srv *tsnet.Server) []healthPeerStatus {
	if srv == nil || srv.Sys() == nil {
		return nil
	}
	lc, err := srv.LocalClient()
	if err != nil || lc == nil {
		return nil
	}
	stCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	tsStatus, err := lc.Status(stCtx)
	if err != nil || tsStatus == nil {
		return nil
	}
	return extractPeersFromStatus(tsStatus)
}
