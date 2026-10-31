package main

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
)

func TestExtractPeersFromStatus(t *testing.T) {
	if got := extractPeersFromStatus(nil); got != nil {
		t.Fatalf("expected nil for nil status, got %+v", got)
	}

	k1 := key.NewNode().Public()
	k2 := key.NewNode().Public()
	k3 := key.NewNode().Public()

	hsTime := time.Date(2026, 10, 31, 10, 22, 15, 0, time.UTC)
	seenTime := time.Date(2026, 10, 31, 10, 24, 0, 0, time.UTC)
	peerTags := views.SliceOf([]string{"tag:prod-db", "tag:server"})
	peerRoutes := views.SliceOf([]netip.Prefix{
		netip.MustParsePrefix("192.168.10.0/24"),
		netip.MustParsePrefix("10.20.0.0/16"),
	})

	tsStatus := &ipnstate.Status{
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			k1: {
				ID:       tailcfg.StableNodeID("nWeb01"),
				HostName: "web-backend.",
				DNSName:  "web-backend.tailnet.ts.net.",
				TailscaleIPs: []netip.Addr{
					netip.MustParseAddr("100.64.0.20"),
					netip.MustParseAddr("fd7a:115c:a1e0::20"),
				},
				Online:        true,
				Active:        true,
				CurAddr:       "203.0.113.45:41641",
				Relay:         "syd",
				RxBytes:       1048576,
				TxBytes:       524288,
				LastHandshake: hsTime,
				LastSeen:      seenTime,
				Tags:          &peerTags,
				PrimaryRoutes: &peerRoutes,
			},
			k2: {
				ID:       tailcfg.StableNodeID("nApi01"),
				HostName: "api-backend",
				DNSName:  "api-backend-01.tailnet.ts.net.",
				TailscaleIPs: []netip.Addr{
					netip.MustParseAddr("100.64.0.10"),
				},
				Online:  false,
				Expired: true,
			},
			k3: {
				ID:       tailcfg.StableNodeID("nApi02"),
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
	if peers[0].ID != "nApi01" || peers[0].HostName != "api-backend" || peers[0].DNSName != "api-backend-01.tailnet.ts.net" || peers[0].IPv4 != "100.64.0.10" || peers[0].Online || !peers[0].Expired {
		t.Errorf("unexpected first peer: %+v", peers[0])
	}
	if peers[1].ID != "nApi02" || peers[1].HostName != "api-backend" || peers[1].DNSName != "api-backend-02.tailnet.ts.net" || peers[1].IPv4 != "100.64.0.11" || !peers[1].Online {
		t.Errorf("unexpected second peer: %+v", peers[1])
	}
	if peers[2].ID != "nWeb01" ||
		peers[2].HostName != "web-backend" ||
		peers[2].DNSName != "web-backend.tailnet.ts.net" ||
		peers[2].IPv4 != "100.64.0.20" ||
		peers[2].IPv6 != "fd7a:115c:a1e0::20" ||
		!peers[2].Online ||
		!peers[2].Active ||
		peers[2].CurAddr != "203.0.113.45:41641" ||
		peers[2].Relay != "syd" ||
		peers[2].RxBytes != 1048576 ||
		peers[2].TxBytes != 524288 ||
		peers[2].LastHandshake != "2026-10-31T10:22:15Z" ||
		peers[2].LastSeen != "2026-10-31T10:24:00Z" ||
		len(peers[2].Tags) != 2 ||
		len(peers[2].PrimaryRoutes) != 2 ||
		peers[2].PrimaryRoutes[0] != "10.20.0.0/16" ||
		peers[2].PrimaryRoutes[1] != "192.168.10.0/24" {
		t.Errorf("unexpected third peer: %+v", peers[2])
	}

	// Verify unstarted tsnet.Server safely returns fallback self hostname from discoverTailnetDetails
	details := discoverTailnetDetails(context.Background(), &tsnet.Server{Hostname: "unstarted"})
	if details.Peers != nil || details.VIPServices != nil || details.Self.HostName != "unstarted" {
		t.Errorf("unexpected details for unstarted tsnet.Server, got %+v", details)
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
}

func TestExtractTailnetDetailsFromStatus(t *testing.T) {
	expiry := time.Date(2027, 4, 29, 10, 0, 0, 0, time.UTC)
	selfTags := views.SliceOf([]string{"tag:proxy", "tag:ingress"})
	tsStatus := &ipnstate.Status{
		BackendState: "Running",
		CertDomains:  []string{"proxtail-corp.tail88356.ts.net"},
		Health:       []string{"warn-1"},
		CurrentTailnet: &ipnstate.TailnetStatus{
			Name:            "example.com",
			MagicDNSSuffix:  "tail88356.ts.net.",
			MagicDNSEnabled: true,
		},
		Self: &ipnstate.PeerStatus{
			ID:       tailcfg.StableNodeID("nSelf01"),
			HostName: "proxtail-corp.",
			DNSName:  "proxtail-corp.tail88356.ts.net.",
			TailscaleIPs: []netip.Addr{
				netip.MustParseAddr("100.64.0.1"),
				netip.MustParseAddr("fd7a:115c:a1e0::1"),
			},
			Online:    true,
			Relay:     "syd",
			Tags:      &selfTags,
			KeyExpiry: &expiry,
		},
	}

	portsPG, _ := tailcfg.ParseProtoPortRanges([]string{"tcp:5432"})
	svcs := map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		tailcfg.ServiceName("svc:postgres"): {
			Name:  tailcfg.ServiceName("svc:postgres"),
			Addrs: []netip.Addr{netip.MustParseAddr("100.64.0.50")},
			Ports: portsPG,
		},
	}

	details := extractTailnetDetailsFromStatus(tsStatus, svcs, &tsnet.Server{Hostname: "fallback-host"})
	if details.TailnetName != "example.com" || details.MagicDNSSuffix != "tail88356.ts.net" || !details.MagicDNSEnabled {
		t.Errorf("unexpected tailnet metadata: %+v", details)
	}
	if details.Self.ID != "nSelf01" ||
		details.Self.HostName != "proxtail-corp" ||
		details.Self.DNSName != "proxtail-corp.tail88356.ts.net" ||
		details.Self.IPv4 != "100.64.0.1" ||
		details.Self.IPv6 != "fd7a:115c:a1e0::1" ||
		details.Self.BackendState != "Running" ||
		!details.Self.Online ||
		details.Self.Relay != "syd" ||
		details.Self.KeyExpiry != "2027-04-29T10:00:00Z" ||
		len(details.Self.Tags) != 2 ||
		len(details.Self.CertDomains) != 1 ||
		len(details.Self.HealthWarnings) != 1 {
		t.Errorf("unexpected self details: %+v", details.Self)
	}
	if len(details.VIPServices) != 1 || details.VIPServices[0].DNSName != "postgres.tail88356.ts.net" {
		t.Errorf("unexpected VIP services: %+v", details.VIPServices)
	}
}

func TestHealthTailnetStatusJSONMarshaling(t *testing.T) {
	st := healthTailnetStatus{
		Name:            "corp-net",
		TailnetName:     "example.com",
		MagicDNSSuffix:  "corp-net.ts.net",
		MagicDNSEnabled: true,
		Self: healthSelfStatus{
			ID:           "nSelf01",
			HostName:     "proxtail-lb",
			DNSName:      "proxtail-lb.corp-net.ts.net",
			IPv4:         "100.64.0.1",
			IPv6:         "fd7a:115c:a1e0::1",
			BackendState: "Running",
			Online:       true,
			Relay:        "syd",
			Tags:         []string{"tag:proxy"},
		},
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
				ID:       "nPeer01",
				HostName: "backend",
				DNSName:  "backend.corp-net.ts.net",
				IPv4:     "100.64.0.10",
				IPv6:     "fd7a:115c:a1e0::10",
				Online:   true,
				Active:   true,
				CurAddr:  "203.0.113.10:41641",
				Relay:    "syd",
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

	if decoded.Self.HostName != "proxtail-lb" || decoded.Self.IPv4 != "100.64.0.1" || decoded.Self.BackendState != "Running" {
		t.Errorf("unexpected decoded self: %+v", decoded.Self)
	}
	if len(decoded.VIPServices) != 1 || decoded.VIPServices[0].Name != "postgres" || decoded.VIPServices[0].DNSName != "postgres.corp-net.ts.net" || decoded.VIPServices[0].IPv4 != "100.64.0.50" {
		t.Errorf("unexpected decoded VIP services: %+v", decoded.VIPServices)
	}
	if len(decoded.Peers) != 1 || decoded.Peers[0].HostName != "backend" || !decoded.Peers[0].Online || !decoded.Peers[0].Active {
		t.Errorf("unexpected decoded peers: %+v", decoded.Peers)
	}
}
