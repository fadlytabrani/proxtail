package main

import (
	"context"
	"net/netip"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
	"tailscale.com/types/key"
)

func TestExtractPeersFromStatus(t *testing.T) {
	if got := extractPeersFromStatus(nil); got != nil {
		t.Fatalf("expected nil for nil status, got %+v", got)
	}

	var k1, k2 key.NodePublic
	tsStatus := &ipnstate.Status{
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			k1: {
				HostName: "web-backend.",
				DNSName:  "web-backend.tailnet.ts.net.",
				TailscaleIPs: []netip.Addr{
					netip.MustParseAddr("100.64.0.20"),
					netip.MustParseAddr("fd7a:115c:a1e0::20"),
				},
				Online: true,
			},
			k2: {
				HostName: "api-backend",
				DNSName:  "api-backend.tailnet.ts.net.",
				TailscaleIPs: []netip.Addr{
					netip.MustParseAddr("100.64.0.10"),
				},
				Online: false,
			},
		},
	}

	// Ensure deterministic key distinction if zero NodePublic collides
	k1 = key.NewNode().Public()
	k2 = key.NewNode().Public()
	tsStatus.Peer = map[key.NodePublic]*ipnstate.PeerStatus{
		k1: {
			HostName: "web-backend.",
			DNSName:  "web-backend.tailnet.ts.net.",
			TailscaleIPs: []netip.Addr{
				netip.MustParseAddr("100.64.0.20"),
				netip.MustParseAddr("fd7a:115c:a1e0::20"),
			},
			Online: true,
		},
		k2: {
			HostName: "api-backend",
			DNSName:  "api-backend.tailnet.ts.net.",
			TailscaleIPs: []netip.Addr{
				netip.MustParseAddr("100.64.0.10"),
			},
			Online: false,
		},
	}

	peers := extractPeersFromStatus(tsStatus)
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(peers))
	}

	// Verify sorted order by hostname ("api-backend" before "web-backend")
	if peers[0].HostName != "api-backend" || peers[0].DNSName != "api-backend.tailnet.ts.net" || peers[0].IPv4 != "100.64.0.10" || peers[0].Online {
		t.Errorf("unexpected first peer: %+v", peers[0])
	}
	if peers[1].HostName != "web-backend" || peers[1].DNSName != "web-backend.tailnet.ts.net" || peers[1].IPv4 != "100.64.0.20" || peers[1].IPv6 != "fd7a:115c:a1e0::20" || !peers[1].Online {
		t.Errorf("unexpected second peer: %+v", peers[1])
	}

	// Verify unstarted tsnet.Server safely returns nil from discoverTailnetPeers
	if got := discoverTailnetPeers(context.Background(), &tsnet.Server{Hostname: "unstarted"}); got != nil {
		t.Errorf("expected nil peers for unstarted tsnet.Server, got %+v", got)
	}
}
