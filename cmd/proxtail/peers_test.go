package main

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
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

	// Verify unstarted tsnet.Server safely returns nil from discoverTailnetDetails
	if p, _ := discoverTailnetDetails(context.Background(), &tsnet.Server{Hostname: "unstarted"}); p != nil {
		t.Errorf("expected nil peers for unstarted tsnet.Server, got %+v", p)
	}
}

func TestExtractVIPServices(t *testing.T) {
	if got := extractVIPServices(nil, "corp.ts.net"); got != nil {
		t.Fatalf("expected nil for nil services map, got %+v", got)
	}

	portsPG, _ := tailcfg.ParseProtoPortRanges([]string{"tcp:5432"})
	portsHTTP, _ := tailcfg.ParseProtoPortRanges([]string{"tcp:80", "tcp:443"})

	svcs := map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		tailcfg.ServiceName("svc:web"): {
			Name:        tailcfg.ServiceName("svc:web"),
			DisplayName: "Web Frontend",
			Addrs: []netip.Addr{
				netip.MustParseAddr("100.64.0.50"),
				netip.MustParseAddr("fd7a:115c:a1e0::50"),
			},
			Ports: portsHTTP,
		},
		tailcfg.ServiceName("svc:db"): {
			Name:        tailcfg.ServiceName("svc:db"),
			DisplayName: "Primary Database",
			Addrs: []netip.Addr{
				netip.MustParseAddr("100.64.0.51"),
			},
			Ports: portsPG,
		},
	}

	got := extractVIPServices(svcs, "corp.ts.net.")
	if len(got) != 2 {
		t.Fatalf("expected 2 VIP services, got %d", len(got))
	}

	// Verify sorted by Name: "db" before "web", with MagicDNS FQDNs
	if got[0].Name != "db" || got[0].DNSName != "db.corp.ts.net" || got[0].DisplayName != "Primary Database" || got[0].IPv4 != "100.64.0.51" || len(got[0].Ports) != 1 || got[0].Ports[0] != "tcp:5432" {
		t.Errorf("unexpected first VIP service: %+v", got[0])
	}
	if got[1].Name != "web" || got[1].DNSName != "web.corp.ts.net" || got[1].DisplayName != "Web Frontend" || got[1].IPv4 != "100.64.0.50" || got[1].IPv6 != "fd7a:115c:a1e0::50" || len(got[1].Ports) != 2 {
		t.Errorf("unexpected second VIP service: %+v", got[1])
	}

	// Verify without MagicDNS suffix, DNSName is omitted
	gotNoSuffix := extractVIPServices(svcs, "")
	if len(gotNoSuffix) != 2 || gotNoSuffix[0].DNSName != "" || gotNoSuffix[1].DNSName != "" {
		t.Errorf("expected empty DNSName when magicDNSSuffix is empty, got %+v", gotNoSuffix)
	}

	// Verify unstarted tsnet.Server safely returns nil from discoverTailnetDetails
	p, v := discoverTailnetDetails(context.Background(), &tsnet.Server{Hostname: "unstarted"})
	if p != nil || v != nil {
		t.Errorf("expected nil from discoverTailnetDetails for unstarted server, got p=%+v v=%+v", p, v)
	}
}

func TestHealthTailnetStatusJSONMarshaling(t *testing.T) {
	st := healthTailnetStatus{
		Name:     "corp-net",
		Hostname: "proxtail-lb",
		IPv4:     "100.64.0.1",
		IPv6:     "fd7a:115c:a1e0::1",
		VIPServices: []healthVIPService{
			{
				Name:        "postgres",
				DNSName:     "postgres.corp-net.ts.net",
				DisplayName: "Database Cluster",
				IPv4:        "100.64.0.50",
				IPv6:        "fd7a:115c:a1e0::50",
				Ports:       []string{"tcp:5432"},
			},
		},
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

	if len(decoded.VIPServices) != 1 || decoded.VIPServices[0].Name != "postgres" || decoded.VIPServices[0].DNSName != "postgres.corp-net.ts.net" || decoded.VIPServices[0].IPv4 != "100.64.0.50" {
		t.Errorf("unexpected decoded VIP services: %+v", decoded.VIPServices)
	}
	if len(decoded.Peers) != 1 || decoded.Peers[0].HostName != "backend" || !decoded.Peers[0].Online {
		t.Errorf("unexpected decoded peers: %+v", decoded.Peers)
	}
}
