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

func TestExtractTailnetDetailsFromStatus(t *testing.T) {
	// Verify unstarted tsnet.Server safely returns fallback Self.HostName from discoverTailnetDetails
	unstartedDetails := discoverTailnetDetails(context.Background(), &tsnet.Server{Hostname: "unstarted"})
	if unstartedDetails.Status == nil || unstartedDetails.Status.Self == nil || unstartedDetails.Status.Self.HostName != "unstarted" {
		t.Fatalf("expected fallback Self.HostName 'unstarted', got %+v", unstartedDetails.Status)
	}
	if unstartedDetails.Services != nil {
		t.Fatalf("expected nil Services for unstarted tsnet.Server, got %+v", unstartedDetails.Services)
	}

	kPeer := key.NewNode().Public()
	expiry := time.Date(2027, 4, 29, 10, 0, 0, 0, time.UTC)
	hsTime := time.Date(2026, 10, 31, 10, 22, 15, 0, time.UTC)
	selfTags := views.SliceOf([]string{"tag:proxy", "tag:ingress"})
	peerTags := views.SliceOf([]string{"tag:prod-db", "tag:server"})
	peerRoutes := views.SliceOf([]netip.Prefix{
		netip.MustParsePrefix("192.168.10.0/24"),
		netip.MustParsePrefix("10.20.0.0/16"),
	})

	tsStatus := &ipnstate.Status{
		Version:        "1.96.4",
		BackendState:   "Running",
		CertDomains:    []string{"proxtail-corp.tail88356.ts.net"},
		Health:         []string{"warn-1"},
		MagicDNSSuffix: "tail88356.ts.net",
		CurrentTailnet: &ipnstate.TailnetStatus{
			Name:            "example.com",
			MagicDNSSuffix:  "tail88356.ts.net",
			MagicDNSEnabled: true,
		},
		Self: &ipnstate.PeerStatus{
			ID:       tailcfg.StableNodeID("nSelf01"),
			HostName: "proxtail-corp",
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
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			kPeer: {
				ID:       tailcfg.StableNodeID("nWeb01"),
				HostName: "web-backend",
				DNSName:  "web-backend.tail88356.ts.net.",
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
				Tags:          &peerTags,
				PrimaryRoutes: &peerRoutes,
			},
		},
	}

	portsPG, _ := tailcfg.ParseProtoPortRanges([]string{"tcp:5432"})
	svcs := map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		tailcfg.ServiceName("svc:postgres"): {
			Name:        tailcfg.ServiceName("svc:postgres"),
			DisplayName: "Primary Database",
			Addrs:       []netip.Addr{netip.MustParseAddr("100.64.0.50")},
			Ports:       portsPG,
		},
	}

	details := extractTailnetDetailsFromStatus(tsStatus, svcs, &tsnet.Server{Hostname: "fallback-host"})
	if details.Status == nil || details.Status.CurrentTailnet == nil {
		t.Fatalf("expected non-nil Status and CurrentTailnet, got %+v", details)
	}
	if details.Status.CurrentTailnet.Name != "example.com" ||
		details.Status.CurrentTailnet.MagicDNSSuffix != "tail88356.ts.net" ||
		!details.Status.CurrentTailnet.MagicDNSEnabled {
		t.Errorf("unexpected CurrentTailnet metadata: %+v", details.Status.CurrentTailnet)
	}
	if details.Status.Self.ID != "nSelf01" ||
		details.Status.Self.HostName != "proxtail-corp" ||
		details.Status.Self.DNSName != "proxtail-corp.tail88356.ts.net." ||
		len(details.Status.TailscaleIPs) != 2 ||
		details.Status.TailscaleIPs[0].String() != "100.64.0.1" ||
		details.Status.BackendState != "Running" ||
		!details.Status.Self.Online ||
		details.Status.Self.Relay != "syd" ||
		details.Status.Self.KeyExpiry == nil ||
		!details.Status.Self.KeyExpiry.Equal(expiry) ||
		details.Status.Self.Tags == nil ||
		details.Status.Self.Tags.Len() != 2 ||
		len(details.Status.CertDomains) != 1 ||
		len(details.Status.Health) != 1 {
		t.Errorf("unexpected Status/Self details: %+v", details.Status)
	}
	peer, ok := details.Status.Peer[kPeer]
	if !ok || peer == nil || peer.HostName != "web-backend" || !peer.Online || !peer.Active || peer.RxBytes != 1048576 {
		t.Errorf("unexpected Peer details: %+v", peer)
	}
	svc, ok := details.Services[tailcfg.ServiceName("svc:postgres")]
	if !ok || svc.DisplayName != "Primary Database" || len(svc.Addrs) != 1 || svc.Addrs[0].String() != "100.64.0.50" {
		t.Errorf("unexpected Services details: %+v", details.Services)
	}

	// Verify deep cloning isolation
	cloned := cloneTailnetHealthDetails(details)
	cloned.Status.Self.HostName = "mutated"
	cloned.Status.Health[0] = "mutated-warn"
	cloned.Status.Peer[kPeer].HostName = "mutated-peer"
	if details.Status.Self.HostName != "proxtail-corp" ||
		details.Status.Health[0] != "warn-1" ||
		details.Status.Peer[kPeer].HostName != "web-backend" {
		t.Errorf("expected cloneTailnetHealthDetails to isolate mutations from original")
	}
}

func TestHealthTailnetStatusJSONMarshaling(t *testing.T) {
	kPeer := key.NewNode().Public()
	selfTags := views.SliceOf([]string{"tag:proxy"})
	portsPG, _ := tailcfg.ParseProtoPortRanges([]string{"tcp:5432"})

	st := healthTailnetStatus{
		Profile: "corp-net",
		Status: &ipnstate.Status{
			Version:        "1.96.4",
			BackendState:   "Running",
			MagicDNSSuffix: "corp-net.ts.net",
			CurrentTailnet: &ipnstate.TailnetStatus{
				Name:            "example.com",
				MagicDNSSuffix:  "corp-net.ts.net",
				MagicDNSEnabled: true,
			},
			TailscaleIPs: []netip.Addr{
				netip.MustParseAddr("100.64.0.1"),
				netip.MustParseAddr("fd7a:115c:a1e0::1"),
			},
			Self: &ipnstate.PeerStatus{
				ID:       "nSelf01",
				HostName: "proxtail-lb",
				DNSName:  "proxtail-lb.corp-net.ts.net.",
				TailscaleIPs: []netip.Addr{
					netip.MustParseAddr("100.64.0.1"),
					netip.MustParseAddr("fd7a:115c:a1e0::1"),
				},
				Online: true,
				Relay:  "syd",
				Tags:   &selfTags,
			},
			Peer: map[key.NodePublic]*ipnstate.PeerStatus{
				kPeer: {
					ID:       "nPeer01",
					HostName: "backend",
					DNSName:  "backend.corp-net.ts.net.",
					TailscaleIPs: []netip.Addr{
						netip.MustParseAddr("100.64.0.10"),
						netip.MustParseAddr("fd7a:115c:a1e0::10"),
					},
					Online:  true,
					Active:  true,
					CurAddr: "203.0.113.10:41641",
					Relay:   "syd",
				},
			},
		},
		Services: map[tailcfg.ServiceName]tailcfg.ServiceDetails{
			tailcfg.ServiceName("svc:postgres"): {
				Name:        tailcfg.ServiceName("svc:postgres"),
				DisplayName: "Database Cluster",
				Addrs: []netip.Addr{
					netip.MustParseAddr("100.64.0.50"),
					netip.MustParseAddr("fd7a:115c:a1e0::50"),
				},
				Ports: portsPG,
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

	if decoded.Profile != "corp-net" ||
		decoded.Status == nil ||
		decoded.Self == nil ||
		decoded.Self.HostName != "proxtail-lb" ||
		len(decoded.Self.TailscaleIPs) != 2 ||
		decoded.Self.TailscaleIPs[0].String() != "100.64.0.1" ||
		decoded.BackendState != "Running" {
		t.Errorf("unexpected decoded self/profile: %+v", decoded)
	}
	svc, ok := decoded.Services[tailcfg.ServiceName("svc:postgres")]
	if !ok || svc.DisplayName != "Database Cluster" || len(svc.Addrs) != 2 || svc.Addrs[0].String() != "100.64.0.50" {
		t.Errorf("unexpected decoded Services: %+v", decoded.Services)
	}
	peer, ok := decoded.Peer[kPeer]
	if !ok || peer == nil || peer.HostName != "backend" || !peer.Online || !peer.Active {
		t.Errorf("unexpected decoded Peer: %+v", decoded.Peer)
	}
}
