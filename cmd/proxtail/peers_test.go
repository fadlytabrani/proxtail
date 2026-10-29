package main

import (
	"context"
	"encoding/json"
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

	k1 := key.NewNode().Public()
	k2 := key.NewNode().Public()
	k3 := key.NewNode().Public()

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
				DNSName:  "api-backend-01.tailnet.ts.net.",
				TailscaleIPs: []netip.Addr{
					netip.MustParseAddr("100.64.0.10"),
				},
				Online: false,
			},
			k3: {
				HostName: "api-backend",
				DNSName:  "api-backend-02.tailnet.ts.net.",
				TailscaleIPs: []netip.Addr{
					netip.MustParseAddr("100.64.0.11"),
				},
				Online: true,
			},
		},
	}

	peers := extractPeersFromStatus(tsStatus)
	if len(peers) != 3 {
		t.Fatalf("expected 3 peers, got %d", len(peers))
	}

	// Verify deterministic sorting:
	// "api-backend" (k2) -> "api-backend" (k3) -> "web-backend" (k1)
	if peers[0].HostName != "api-backend" || peers[0].DNSName != "api-backend-01.tailnet.ts.net" || peers[0].IPv4 != "100.64.0.10" || peers[0].Online {
		t.Errorf("unexpected first peer: %+v", peers[0])
	}
	if peers[1].HostName != "api-backend" || peers[1].DNSName != "api-backend-02.tailnet.ts.net" || peers[1].IPv4 != "100.64.0.11" || !peers[1].Online {
		t.Errorf("unexpected second peer: %+v", peers[1])
	}
	if peers[2].HostName != "web-backend" || peers[2].DNSName != "web-backend.tailnet.ts.net" || peers[2].IPv4 != "100.64.0.20" || peers[2].IPv6 != "fd7a:115c:a1e0::20" || !peers[2].Online {
		t.Errorf("unexpected third peer: %+v", peers[2])
	}

	// Verify unstarted tsnet.Server safely returns nil from discoverTailnetPeers
	if got := discoverTailnetPeers(context.Background(), &tsnet.Server{Hostname: "unstarted"}); got != nil {
		t.Errorf("expected nil peers for unstarted tsnet.Server, got %+v", got)
	}
}

func TestHealthTailnetStatusJSONMarshaling(t *testing.T) {
	st := healthTailnetStatus{
		Name:     "corp-net",
		Hostname: "proxtail-lb",
		IPv4:     "100.64.0.1",
		IPv6:     "fd7a:115c:a1e0::1",
		Peers: []healthPeerStatus{
			{
				HostName: "backend",
				DNSName:  "backend.corp-net.ts.net",
				IPv4:     "100.64.0.10",
				IPv6:     "fd7a:115c:a1e0::10",
				Online:   true,
			},
		},
	}

	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("failed to marshal healthTailnetStatus: %v", err)
	}

	var decoded healthTailnetStatus
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal healthTailnetStatus: %v", err)
	}

	if len(decoded.Peers) != 1 || decoded.Peers[0].HostName != "backend" || !decoded.Peers[0].Online {
		t.Errorf("unexpected decoded peers: %+v", decoded.Peers)
	}
}
