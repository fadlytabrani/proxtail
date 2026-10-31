package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"

	"proxtail/internal/config"
	"proxtail/internal/proxy"
)

var nextFreePortBase atomic.Int32

func init() {
	nextFreePortBase.Store(36000)
}

// getFreePortBlock finds a contiguous block of `count` free local ports (for both TCP and UDP)
// on 127.0.0.1 outside the default 30000-30999 range so tests are isolated from host processes.
func getFreePortBlock(t *testing.T, count int) (int, int) {
	t.Helper()
	for range 500 {
		base := int(nextFreePortBase.Add(int32(count+4))) - (count + 4)
		if base+count > 48000 {
			nextFreePortBase.Store(36000)
			continue
		}
		var tcpHold []net.Listener
		var udpHold []net.PacketConn
		ok := true
		for offset := range count {
			p := base + offset
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				ok = false
				break
			}
			tcpHold = append(tcpHold, ln)
			pc, err := net.ListenPacket("udp", addr)
			if err != nil {
				ok = false
				break
			}
			udpHold = append(udpHold, pc)
		}
		for _, pc := range udpHold {
			_ = pc.Close()
		}
		for _, ln := range tcpHold {
			_ = ln.Close()
		}
		if ok {
			return base, base + count - 1
		}
	}
	t.Fatalf("unable to find %d contiguous free ports on 127.0.0.1", count)
	return 0, 0
}

// vipRoutingDialer maps synthetic Tailscale VIP "<network>:<ip:port>" targets to local loopback test servers.
type vipRoutingDialer struct {
	mu     sync.RWMutex
	routes map[string]string // "<network>:<dest>" or "<dest>" -> "127.0.0.1:<port>"
}

func newVIPRoutingDialer() *vipRoutingDialer {
	return &vipRoutingDialer{
		routes: make(map[string]string),
	}
}

func (d *vipRoutingDialer) setRoute(network, vipDest, localTarget string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if network != "" {
		d.routes[strings.ToLower(network)+":"+vipDest] = localTarget
	} else {
		d.routes[vipDest] = localTarget
	}
}

func (d *vipRoutingDialer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.RLock()
	target, ok := d.routes[strings.ToLower(network)+":"+address]
	if !ok {
		target, ok = d.routes[address]
	}
	d.mu.RUnlock()
	if !ok {
		target = address
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, target)
}

func startTCPEchoBackend(t *testing.T, prefix string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start TCP echo backend: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						reply := []byte(prefix + string(buf[:n]))
						if _, wErr := c.Write(reply); wErr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func startUDPEchoBackend(t *testing.T, prefix string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start UDP echo backend: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo([]byte(prefix+string(buf[:n])), addr)
		}
	}()
	return pc.LocalAddr().String()
}

func assertTCPRoundTrip(t *testing.T, port int, msg, want string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("TCP dial to port %d failed: %v", port, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("TCP write to port %d failed: %v", port, err)
	}
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("TCP read from port %d failed: %v", port, err)
	}
	if string(buf) != want {
		t.Fatalf("TCP port %d response = %q, want %q", port, string(buf), want)
	}
}

func assertUDPRoundTrip(t *testing.T, port int, msg, want string) {
	t.Helper()
	conn, err := net.DialTimeout("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("UDP dial to port %d failed: %v", port, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("UDP write to port %d failed: %v", port, err)
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("UDP read from port %d failed: %v", port, err)
	}
	if string(buf[:n]) != want {
		t.Fatalf("UDP port %d response = %q, want %q", port, string(buf[:n]), want)
	}
}

func TestVIPAutoProxyEndToEndTCPAndUDPWithHealthz(t *testing.T) {
	dnsTCPBackend := startTCPEchoBackend(t, "dns-tcp:")
	dnsUDPBackend := startUDPEchoBackend(t, "dns-udp:")
	webTCPBackend := startTCPEchoBackend(t, "web-tcp:")
	manualBackend := startTCPEchoBackend(t, "manual:")

	dialer := newVIPRoutingDialer()
	dialer.setRoute("tcp", "100.101.102.10:53", dnsTCPBackend)
	dialer.setRoute("udp", "100.101.102.10:53", dnsUDPBackend)
	dialer.setRoute("tcp", "100.101.102.20:443", webTCPBackend)
	dialer.setRoute("tcp", "100.64.0.5:8080", manualBackend)

	vipStart, vipEnd := getFreePortBlock(t, 4)
	manualPort := getFreePort(t)

	yamlCfg := fmt.Sprintf(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
    ports:
      - port: %d
        destination: '100.64.0.5:8080'
proxy:
  vip_port_range: '%d-%d'
`, manualPort, vipStart, vipEnd)

	cfg, err := config.Parse([]byte(yamlCfg))
	if err != nil {
		t.Fatalf("config.Parse failed: %v", err)
	}

	tm := newTailnetManager()
	tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) {
		return &tsnet.Server{Hostname: c.Hostname}, nil
	}
	tm.cleanupFunc = func(*tsnet.Server, string) {}
	tm.vipDiscoverFunc = func(_ context.Context, tailnetName string, _ *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
		if tailnetName != "prod" {
			return nil, nil
		}
		return map[tailcfg.ServiceName]tailcfg.ServiceDetails{
			"svc:dns": {
				Name:  "svc:dns",
				Addrs: []netip.Addr{netip.MustParseAddr("100.101.102.10")},
				// Proto 0 = dual TCP + UDP sharing the same allocated local port (vipStart)
				Ports: []tailcfg.ProtoPortRange{{Proto: 0, Ports: tailcfg.PortRange{First: 53, Last: 53}}},
			},
			"svc:web": {
				Name:  "svc:web",
				Addrs: []netip.Addr{netip.MustParseAddr("100.101.102.20"), netip.MustParseAddr("fd7a:115c:a1e0::20")},
				Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 443, Last: 443}}},
			},
		}, nil
	}
	defer tm.closeAll()

	servers, _, _, err := tm.syncTailnets(cfg.Tailnets)
	if err != nil {
		t.Fatalf("syncTailnets failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initialPorts, err := tm.prepareInitialPorts(ctx, cfg, servers)
	if err != nil {
		t.Fatalf("prepareInitialPorts failed: %v", err)
	}
	// Expect 1 manual TCP + 2 dns (TCP + UDP on vipStart) + 1 web (TCP on vipStart+1) = 4 PortMappings
	if len(initialPorts) != 4 {
		t.Fatalf("expected 4 initial port mappings, got %d: %+v", len(initialPorts), initialPorts)
	}

	p := proxy.NewProxyWithDialers(initialPorts, map[string]proxy.Dialer{"prod": dialer}, 32*1024, 50)
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	select {
	case <-p.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxy to start")
	}

	// Verify traffic across manual port, dual-protocol VIP port (vipStart), and second VIP port (vipStart+1)
	assertTCPRoundTrip(t, manualPort, "ping", "manual:ping")
	assertTCPRoundTrip(t, vipStart, "query", "dns-tcp:query")
	assertUDPRoundTrip(t, vipStart, "query", "dns-udp:query")
	assertTCPRoundTrip(t, vipStart+1, "hello", "web-tcp:hello")

	// Verify /healthz reports vip_service metadata on auto-proxied ports
	handler := newHealthHandler(p, tm)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected /healthz 200 OK, got %d", rec.Code)
	}
	var hr healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &hr); err != nil {
		t.Fatalf("failed to decode /healthz: %v", err)
	}
	if len(hr.Ports) != 4 {
		t.Fatalf("expected 4 ports in /healthz, got %+v", hr.Ports)
	}
	if hr.Ports[0].Source != manualPort || hr.Ports[0].VIPService != "" || !hr.Ports[0].Up {
		t.Errorf("unexpected manual port status: %+v", hr.Ports[0])
	}
	if hr.Ports[1].Source != vipStart || hr.Ports[1].Protocol != "tcp" || hr.Ports[1].VIPService != "dns" || !hr.Ports[1].Up {
		t.Errorf("unexpected dns tcp VIP port status: %+v", hr.Ports[1])
	}
	if hr.Ports[2].Source != vipStart || hr.Ports[2].Protocol != "udp" || hr.Ports[2].VIPService != "dns" || !hr.Ports[2].Up {
		t.Errorf("unexpected dns udp VIP port status: %+v", hr.Ports[2])
	}
	if hr.Ports[3].Source != vipStart+1 || hr.Ports[3].Protocol != "tcp" || hr.Ports[3].VIPService != "web" || !hr.Ports[3].Up {
		t.Errorf("unexpected web tcp VIP port status: %+v", hr.Ports[3])
	}
}

func TestVIPStickyDynamicReconciliationAndBackgroundLoop(t *testing.T) {
	alphaBackend := startTCPEchoBackend(t, "alpha:")
	betaBackend := startTCPEchoBackend(t, "beta:")
	gammaBackend := startTCPEchoBackend(t, "gamma:")

	dialer := newVIPRoutingDialer()
	dialer.setRoute("tcp", "100.101.102.1:80", alphaBackend)
	dialer.setRoute("tcp", "100.101.102.2:80", betaBackend)
	dialer.setRoute("tcp", "100.101.102.3:80", gammaBackend)

	vipStart, vipEnd := getFreePortBlock(t, 4)

	cfg, err := config.Parse([]byte(fmt.Sprintf(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
proxy:
  vip_port_range: '%d-%d'
`, vipStart, vipEnd)))
	if err != nil {
		t.Fatalf("config.Parse failed: %v", err)
	}

	var vipMu sync.Mutex
	currentVIPs := map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		"svc:alpha": {
			Name:  "svc:alpha",
			Addrs: []netip.Addr{netip.MustParseAddr("100.101.102.1")},
			Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 80, Last: 80}}},
		},
		"svc:beta": {
			Name:  "svc:beta",
			Addrs: []netip.Addr{netip.MustParseAddr("100.101.102.2")},
			Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 80, Last: 80}}},
		},
	}

	tm := newTailnetManager()
	tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) {
		return &tsnet.Server{Hostname: c.Hostname}, nil
	}
	tm.cleanupFunc = func(*tsnet.Server, string) {}
	tm.vipRefreshOverride = 30 * time.Millisecond
	tm.vipDebounceDelay = 10 * time.Millisecond
	tm.vipDiscoverFunc = func(_ context.Context, _ string, _ *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
		vipMu.Lock()
		defer vipMu.Unlock()
		return maps.Clone(currentVIPs), nil
	}
	defer tm.closeAll()

	servers, _, _, err := tm.syncTailnets(cfg.Tailnets)
	if err != nil {
		t.Fatalf("syncTailnets failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initialPorts, err := tm.prepareInitialPorts(ctx, cfg, servers)
	if err != nil {
		t.Fatalf("prepareInitialPorts failed: %v", err)
	}

	p := proxy.NewProxyWithDialers(initialPorts, map[string]proxy.Dialer{"prod": dialer}, 32*1024, 50)
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()
	<-p.Started()

	tm.startVIPReconciler(ctx, p)

	// alpha -> vipStart, beta -> vipStart+1
	assertTCPRoundTrip(t, vipStart, "1", "alpha:1")
	assertTCPRoundTrip(t, vipStart+1, "1", "beta:1")

	// Open a persistent TCP connection to beta (vipStart+1) across dynamic VIP churn
	persistentBeta, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(vipStart+1)), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to open persistent connection to beta: %v", err)
	}
	defer persistentBeta.Close()

	// Remove alpha (freeing vipStart), keep beta (sticky on vipStart+1), add gamma (reuses freed vipStart)
	vipMu.Lock()
	delete(currentVIPs, "svc:alpha")
	currentVIPs["svc:gamma"] = tailcfg.ServiceDetails{
		Name:  "svc:gamma",
		Addrs: []netip.Addr{netip.MustParseAddr("100.101.102.3")},
		Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 80, Last: 80}}},
	}
	vipMu.Unlock()
	tm.triggerVIPReconcile()

	deadline := time.Now().Add(2 * time.Second)
	gammaReady := false
	for time.Now().Before(deadline) {
		statuses := p.GetPortStatuses()
		for _, st := range statuses {
			if st.Source == vipStart && st.VIPService == "gamma" && st.Up {
				gammaReady = true
				break
			}
		}
		if gammaReady {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !gammaReady {
		t.Fatalf("expected gamma to bind freed port %d, got statuses: %+v", vipStart, p.GetPortStatuses())
	}

	// Verify gamma now answers on vipStart
	assertTCPRoundTrip(t, vipStart, "new", "gamma:new")

	// Verify beta stayed sticky on vipStart+1 AND its in-flight TCP connection was never dropped
	_ = persistentBeta.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := persistentBeta.Write([]byte("still-alive")); err != nil {
		t.Fatalf("persistent beta connection write failed after reconciliation: %v", err)
	}
	reply := make([]byte, len("beta:still-alive"))
	if _, err := io.ReadFull(persistentBeta, reply); err != nil || string(reply) != "beta:still-alive" {
		t.Fatalf("persistent beta connection read = %q (err=%v), want 'beta:still-alive'", string(reply), err)
	}

	// Remove all VIP services and verify background periodic ticker unbinds all VIP listeners
	vipMu.Lock()
	clear(currentVIPs)
	vipMu.Unlock()

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.ActiveListenerCount() == 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if p.ActiveListenerCount() != 0 {
		t.Fatalf("expected 0 active listeners after removing all VIP services, got %d", p.ActiveListenerCount())
	}
}

func TestVIPHotReloadToggleAutoProxyAndRangeChange(t *testing.T) {
	prodBackend := startTCPEchoBackend(t, "prod-vip:")
	stagingBackend := startTCPEchoBackend(t, "staging-vip:")

	dialer := newVIPRoutingDialer()
	dialer.setRoute("tcp", "100.101.1.1:443", prodBackend)
	dialer.setRoute("tcp", "100.101.2.1:443", stagingBackend)

	range1Start, range1End := getFreePortBlock(t, 3)
	range2Start, range2End := getFreePortBlock(t, 3)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	// Initial config: prod auto_proxy_vips defaults to true; staging explicitly false
	yaml1 := fmt.Sprintf(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
  - profile: 'staging'
    listen_address: '127.0.0.1'
    auto_proxy_vips: false
proxy:
  vip_port_range: '%d-%d'
`, range1Start, range1End)
	if err := os.WriteFile(cfgPath, []byte(yaml1), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	tm := newTailnetManager()
	tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) {
		return &tsnet.Server{Hostname: c.Hostname}, nil
	}
	tm.cleanupFunc = func(*tsnet.Server, string) {}
	tm.dialerForServer = func(_ string, _ *tsnet.Server) proxy.Dialer {
		return dialer
	}
	tm.vipDiscoverFunc = func(_ context.Context, tailnetName string, _ *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
		switch tailnetName {
		case "prod":
			return map[tailcfg.ServiceName]tailcfg.ServiceDetails{
				"svc:prod-api": {
					Name:  "svc:prod-api",
					Addrs: []netip.Addr{netip.MustParseAddr("100.101.1.1")},
					Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 443, Last: 443}}},
				},
			}, nil
		case "staging":
			return map[tailcfg.ServiceName]tailcfg.ServiceDetails{
				"svc:staging-api": {
					Name:  "svc:staging-api",
					Addrs: []netip.Addr{netip.MustParseAddr("100.101.2.1")},
					Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 443, Last: 443}}},
				},
			}, nil
		default:
			return nil, nil
		}
	}
	defer tm.closeAll()

	servers, _, _, err := tm.syncTailnets(cfg.Tailnets)
	if err != nil {
		t.Fatalf("syncTailnets failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initialPorts, err := tm.prepareInitialPorts(ctx, cfg, servers)
	if err != nil {
		t.Fatalf("prepareInitialPorts failed: %v", err)
	}
	if len(initialPorts) != 1 || initialPorts[0].VIPService != "prod-api" {
		t.Fatalf("expected only prod-api VIP to be proxied initially, got %+v", initialPorts)
	}

	p := proxy.NewProxyWithDialers(initialPorts, map[string]proxy.Dialer{"prod": dialer, "staging": dialer}, 32*1024, 50)
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()
	<-p.Started()

	assertTCPRoundTrip(t, range1Start, "hi", "prod-vip:hi")

	// Reload 1: Enable auto_proxy_vips on staging too -> prod stays on range1Start, staging gets range1Start+1
	yaml2 := fmt.Sprintf(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
  - profile: 'staging'
    listen_address: '127.0.0.1'
    auto_proxy_vips: true
proxy:
  vip_port_range: '%d-%d'
`, range1Start, range1End)
	if err := os.WriteFile(cfgPath, []byte(yaml2), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloadConfig(cfgPath, p, tm); err != nil {
		t.Fatalf("reloadConfig enabling staging auto_proxy_vips failed: %v", err)
	}
	assertTCPRoundTrip(t, range1Start, "hi", "prod-vip:hi")
	assertTCPRoundTrip(t, range1Start+1, "hi", "staging-vip:hi")

	// Reload 2: Disable prod auto_proxy_vips and migrate vip_port_range to range2Start-range2End
	yaml3 := fmt.Sprintf(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
    auto_proxy_vips: false
  - profile: 'staging'
    listen_address: '127.0.0.1'
    auto_proxy_vips: true
proxy:
  vip_port_range: '%d-%d'
`, range2Start, range2End)
	if err := os.WriteFile(cfgPath, []byte(yaml3), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloadConfig(cfgPath, p, tm); err != nil {
		t.Fatalf("reloadConfig migrating vip_port_range failed: %v", err)
	}
	if p.ActiveListenerCount() != 1 {
		t.Fatalf("expected 1 active listener after disabling prod VIPs, got %d", p.ActiveListenerCount())
	}
	assertTCPRoundTrip(t, range2Start, "migrated", "staging-vip:migrated")
}

func TestVIPFailureAndAtomicRollback(t *testing.T) {
	manualBackend := startTCPEchoBackend(t, "manual:")
	vip1Backend := startTCPEchoBackend(t, "vip1:")
	vip2Backend := startTCPEchoBackend(t, "vip2:")

	dialer := newVIPRoutingDialer()
	dialer.setRoute("tcp", "100.64.0.1:8080", manualBackend)
	dialer.setRoute("tcp", "100.101.0.1:80", vip1Backend)
	dialer.setRoute("tcp", "100.101.0.2:80", vip2Backend)

	t.Run("wildcard port range rejected on startup", func(t *testing.T) {
		vipStart, vipEnd := getFreePortBlock(t, 3)
		cfg, err := config.Parse([]byte(fmt.Sprintf(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
proxy:
  vip_port_range: '%d-%d'
`, vipStart, vipEnd)))
		if err != nil {
			t.Fatal(err)
		}
		tm := newTailnetManager()
		tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) { return &tsnet.Server{Hostname: c.Hostname}, nil }
		tm.cleanupFunc = func(*tsnet.Server, string) {}
		tm.vipDiscoverFunc = func(_ context.Context, _ string, _ *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
			return map[tailcfg.ServiceName]tailcfg.ServiceDetails{
				"svc:wildcard": {
					Name:  "svc:wildcard",
					Addrs: []netip.Addr{netip.MustParseAddr("100.101.0.1")},
					Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRangeAny}},
				},
			}, nil
		}
		defer tm.closeAll()

		servers, _, _, _ := tm.syncTailnets(cfg.Tailnets)
		_, err = tm.prepareInitialPorts(context.Background(), cfg, servers)
		if err == nil || !strings.Contains(err.Error(), "wildcard port range") {
			t.Fatalf("expected wildcard port range error, got %v", err)
		}
	})

	t.Run("vip_port_range exhaustion rejected", func(t *testing.T) {
		vipStart, _ := getFreePortBlock(t, 1)
		cfg, err := config.Parse([]byte(fmt.Sprintf(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
proxy:
  vip_port_range: '%d-%d'
`, vipStart, vipStart)))
		if err != nil {
			t.Fatal(err)
		}
		tm := newTailnetManager()
		tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) { return &tsnet.Server{Hostname: c.Hostname}, nil }
		tm.cleanupFunc = func(*tsnet.Server, string) {}
		tm.vipDiscoverFunc = func(_ context.Context, _ string, _ *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
			return map[tailcfg.ServiceName]tailcfg.ServiceDetails{
				"svc:one": {
					Name:  "svc:one",
					Addrs: []netip.Addr{netip.MustParseAddr("100.101.0.1")},
					Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 80, Last: 80}}},
				},
				"svc:two": {
					Name:  "svc:two",
					Addrs: []netip.Addr{netip.MustParseAddr("100.101.0.2")},
					Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 80, Last: 80}}},
				},
			}, nil
		}
		defer tm.closeAll()

		servers, _, _, _ := tm.syncTailnets(cfg.Tailnets)
		_, err = tm.prepareInitialPorts(context.Background(), cfg, servers)
		if err == nil || !strings.Contains(err.Error(), "exhausted") {
			t.Fatalf("expected vip_port_range exhausted error, got %v", err)
		}
	})

	t.Run("startup bind failure rolls back all listeners", func(t *testing.T) {
		vipStart, vipEnd := getFreePortBlock(t, 2)
		// Occupy vipStart+1 externally so the second VIP port fails to bind during Proxy.Start
		blocker, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(vipStart+1)))
		if err != nil {
			t.Fatalf("failed to bind external blocker: %v", err)
		}
		defer blocker.Close()

		cfg, err := config.Parse([]byte(fmt.Sprintf(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
proxy:
  vip_port_range: '%d-%d'
`, vipStart, vipEnd)))
		if err != nil {
			t.Fatal(err)
		}

		tm := newTailnetManager()
		tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) { return &tsnet.Server{Hostname: c.Hostname}, nil }
		tm.cleanupFunc = func(*tsnet.Server, string) {}
		tm.vipDiscoverFunc = func(_ context.Context, _ string, _ *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
			return map[tailcfg.ServiceName]tailcfg.ServiceDetails{
				"svc:first": {
					Name:  "svc:first",
					Addrs: []netip.Addr{netip.MustParseAddr("100.101.0.1")},
					Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 80, Last: 80}}},
				},
				"svc:second": {
					Name:  "svc:second",
					Addrs: []netip.Addr{netip.MustParseAddr("100.101.0.2")},
					Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 80, Last: 80}}},
				},
			}, nil
		}
		defer tm.closeAll()

		servers, _, _, _ := tm.syncTailnets(cfg.Tailnets)
		initialPorts, err := tm.prepareInitialPorts(context.Background(), cfg, servers)
		if err != nil {
			t.Fatalf("prepareInitialPorts failed: %v", err)
		}

		p := proxy.NewProxyWithDialers(initialPorts, map[string]proxy.Dialer{"prod": dialer}, 32*1024, 50)
		startErr := p.Start(context.Background())
		if startErr == nil {
			p.Stop()
			t.Fatal("expected Proxy.Start to fail when VIP port is already occupied")
		}
		if p.ActiveListenerCount() != 0 {
			t.Fatalf("expected 0 active listeners after startup rollback, got %d", p.ActiveListenerCount())
		}
	})

	t.Run("runtime reconciliation partial bind failure rolls back atomically without affecting manual ports", func(t *testing.T) {
		vipStart, vipEnd := getFreePortBlock(t, 2)
		manualPort := getFreePort(t)

		cfg, err := config.Parse([]byte(fmt.Sprintf(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
    ports:
      - port: %d
        destination: '100.64.0.1:8080'
proxy:
  vip_port_range: '%d-%d'
`, manualPort, vipStart, vipEnd)))
		if err != nil {
			t.Fatal(err)
		}

		var vipMu sync.Mutex
		vips := map[tailcfg.ServiceName]tailcfg.ServiceDetails{}

		tm := newTailnetManager()
		tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) { return &tsnet.Server{Hostname: c.Hostname}, nil }
		tm.cleanupFunc = func(*tsnet.Server, string) {}
		tm.vipDiscoverFunc = func(_ context.Context, _ string, _ *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
			vipMu.Lock()
			defer vipMu.Unlock()
			return maps.Clone(vips), nil
		}
		defer tm.closeAll()

		servers, _, _, _ := tm.syncTailnets(cfg.Tailnets)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		initialPorts, err := tm.prepareInitialPorts(ctx, cfg, servers)
		if err != nil {
			t.Fatal(err)
		}
		p := proxy.NewProxyWithDialers(initialPorts, map[string]proxy.Dialer{"prod": dialer}, 32*1024, 50)
		go func() { _ = p.Start(ctx) }()
		defer p.Stop()
		<-p.Started()

		// Occupy vipStart+1 externally while vipStart is free
		blocker, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(vipStart+1)))
		if err != nil {
			t.Fatalf("failed to bind external blocker: %v", err)
		}
		defer blocker.Close()

		// Advertise 2 VIP services: vipStart would succeed, vipStart+1 will fail -> entire batch must roll back!
		vipMu.Lock()
		vips["svc:first"] = tailcfg.ServiceDetails{
			Name:  "svc:first",
			Addrs: []netip.Addr{netip.MustParseAddr("100.101.0.1")},
			Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 80, Last: 80}}},
		}
		vips["svc:second"] = tailcfg.ServiceDetails{
			Name:  "svc:second",
			Addrs: []netip.Addr{netip.MustParseAddr("100.101.0.2")},
			Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 80, Last: 80}}},
		}
		vipMu.Unlock()

		recErr := tm.reconcileVIPs(ctx, p)
		if recErr == nil {
			t.Fatal("expected reconcileVIPs to fail when vipStart+1 is occupied")
		}

		// Verify atomic rollback: only the 1 manual port remains configured & active, and vipStart is NOT left bound
		if p.ActiveListenerCount() != 1 || p.ConfiguredPortCount() != 1 {
			t.Fatalf("expected atomic rollback to 1 manual port, got active=%d configured=%d",
				p.ActiveListenerCount(), p.ConfiguredPortCount())
		}
		if probeLn, lErr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(vipStart))); lErr != nil {
			t.Fatalf("expected rolled-back port %d to be free, got bind error: %v", vipStart, lErr)
		} else {
			_ = probeLn.Close()
		}
		assertTCPRoundTrip(t, manualPort, "ok", "manual:ok")

		// Release external blocker and retry reconciliation -> both VIP ports now bind and forward traffic!
		_ = blocker.Close()
		if err := tm.reconcileVIPs(ctx, p); err != nil {
			t.Fatalf("expected reconcileVIPs to succeed once port conflict cleared, got: %v", err)
		}
		assertTCPRoundTrip(t, manualPort, "ok", "manual:ok")
		assertTCPRoundTrip(t, vipStart, "a", "vip1:a")
		assertTCPRoundTrip(t, vipStart+1, "b", "vip2:b")
	})
}

func TestVIPInitialNoNetmapGracefulThenDiscovers(t *testing.T) {
	vipBackend := startTCPEchoBackend(t, "vip:")
	dialer := newVIPRoutingDialer()
	dialer.setRoute("tcp", "100.101.9.9:80", vipBackend)

	vipStart, vipEnd := getFreePortBlock(t, 2)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
proxy:
  vip_port_range: '%d-%d'
`, vipStart, vipEnd)))
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	hasNetmap := false

	tm := newTailnetManager()
	tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) { return &tsnet.Server{Hostname: c.Hostname}, nil }
	tm.cleanupFunc = func(*tsnet.Server, string) {}
	tm.vipDiscoverFunc = func(_ context.Context, _ string, _ *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
		mu.Lock()
		ready := hasNetmap
		mu.Unlock()
		if !ready {
			return nil, errNoNetmap
		}
		return map[tailcfg.ServiceName]tailcfg.ServiceDetails{
			"svc:app": {
				Name:  "svc:app",
				Addrs: []netip.Addr{netip.MustParseAddr("100.101.9.9")},
				Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 80, Last: 80}}},
			},
		}, nil
	}
	defer tm.closeAll()

	servers, _, _, _ := tm.syncTailnets(cfg.Tailnets)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Before netmap arrives: startup succeeds with 0 initial VIP ports
	initialPorts, err := tm.prepareInitialPorts(ctx, cfg, servers)
	if err != nil {
		t.Fatalf("expected errNoNetmap on initial startup to be tolerated, got: %v", err)
	}
	if len(initialPorts) != 0 {
		t.Fatalf("expected 0 initial ports before netmap arrives, got %d", len(initialPorts))
	}

	p := proxy.NewProxyWithDialers(initialPorts, map[string]proxy.Dialer{"prod": dialer}, 32*1024, 50)
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()
	<-p.Started()

	// 2. Netmap arrives -> VIP service binds immediately
	mu.Lock()
	hasNetmap = true
	mu.Unlock()
	if err := tm.reconcileVIPs(ctx, p); err != nil {
		t.Fatalf("reconcileVIPs failed after netmap arrived: %v", err)
	}
	assertTCPRoundTrip(t, vipStart, "ping", "vip:ping")

	// 3. Transient errNoNetmap at runtime preserves last known VIP listeners without flapping
	mu.Lock()
	hasNetmap = false
	mu.Unlock()
	if err := tm.reconcileVIPs(ctx, p); err != nil {
		t.Fatalf("reconcileVIPs failed during transient errNoNetmap: %v", err)
	}
	if p.ActiveListenerCount() != 1 {
		t.Fatalf("expected active VIP listener to be preserved across transient errNoNetmap, got %d", p.ActiveListenerCount())
	}
	assertTCPRoundTrip(t, vipStart, "still-up", "vip:still-up")
}

func TestMultiTailnetPerAddressVIPPortAllocationAndParallelDiscovery(t *testing.T) {
	t.Run("disjoint listen_address tailnets reuse same VIP port range and discover in parallel", func(t *testing.T) {
		cfg, err := config.Parse([]byte(`
tailnets:
  - profile: 'prod'
    listen_address: '127.0.0.1'
  - profile: 'staging'
    listen_address: '127.0.0.2'
proxy:
  vip_port_range: '31000-31005'
`))
		if err != nil {
			t.Fatalf("config.Parse failed: %v", err)
		}

		var inFlight int32
		var maxInFlight int32
		barrier := make(chan struct{})

		tm := newTailnetManager()
		tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) {
			return &tsnet.Server{Hostname: c.Hostname}, nil
		}
		tm.cleanupFunc = func(*tsnet.Server, string) {}
		tm.vipDiscoverFunc = func(_ context.Context, tailnetName string, _ *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				prev := atomic.LoadInt32(&maxInFlight)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxInFlight, prev, cur) {
					break
				}
			}
			if cur == 2 {
				close(barrier)
			} else {
				select {
				case <-barrier:
				case <-time.After(500 * time.Millisecond):
				}
			}
			atomic.AddInt32(&inFlight, -1)

			ip := "100.101.1.1"
			if tailnetName == "staging" {
				ip = "100.101.2.1"
			}
			return map[tailcfg.ServiceName]tailcfg.ServiceDetails{
				"svc:api": {
					Name:  "svc:api",
					Addrs: []netip.Addr{netip.MustParseAddr(ip)},
					Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 443, Last: 443}}},
				},
			}, nil
		}
		defer tm.closeAll()

		servers, _, _, err := tm.syncTailnets(cfg.Tailnets)
		if err != nil {
			t.Fatalf("syncTailnets failed: %v", err)
		}

		ports, err := tm.prepareInitialPorts(context.Background(), cfg, servers)
		if err != nil {
			t.Fatalf("prepareInitialPorts failed: %v", err)
		}
		if atomic.LoadInt32(&maxInFlight) < 2 {
			t.Errorf("expected parallel VIP discovery across tailnets (maxInFlight >= 2), got %d", atomic.LoadInt32(&maxInFlight))
		}
		if len(ports) != 2 {
			t.Fatalf("expected 2 VIP port mappings, got %d: %+v", len(ports), ports)
		}
		// Both prod (127.0.0.1) and staging (127.0.0.2) should independently allocate port 31000
		if ports[0].Tailnet != "prod" || ports[0].ListenAddress != "127.0.0.1" || ports[0].Source != 31000 {
			t.Errorf("unexpected prod VIP mapping: %+v", ports[0])
		}
		if ports[1].Tailnet != "staging" || ports[1].ListenAddress != "127.0.0.2" || ports[1].Source != 31000 {
			t.Errorf("unexpected staging VIP mapping: %+v", ports[1])
		}
	})

	t.Run("overlapping wildcard and loopback listen_address tailnets allocate distinct VIP ports", func(t *testing.T) {
		cfg, err := config.Parse([]byte(`
tailnets:
  - profile: 'prod'
    listen_address: '0.0.0.0'
  - profile: 'staging'
    listen_address: '127.0.0.1'
proxy:
  vip_port_range: '31010-31015'
`))
		if err != nil {
			t.Fatalf("config.Parse failed: %v", err)
		}

		tm := newTailnetManager()
		tm.initFunc = func(c config.TailnetConfig) (*tsnet.Server, error) {
			return &tsnet.Server{Hostname: c.Hostname}, nil
		}
		tm.cleanupFunc = func(*tsnet.Server, string) {}
		tm.vipDiscoverFunc = func(_ context.Context, tailnetName string, _ *tsnet.Server) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
			return map[tailcfg.ServiceName]tailcfg.ServiceDetails{
				"svc:api": {
					Name:  "svc:api",
					Addrs: []netip.Addr{netip.MustParseAddr("100.101.1.1")},
					Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 443, Last: 443}}},
				},
			}, nil
		}
		defer tm.closeAll()

		servers, _, _, err := tm.syncTailnets(cfg.Tailnets)
		if err != nil {
			t.Fatalf("syncTailnets failed: %v", err)
		}

		ports, err := tm.prepareInitialPorts(context.Background(), cfg, servers)
		if err != nil {
			t.Fatalf("prepareInitialPorts failed: %v", err)
		}
		if len(ports) != 2 {
			t.Fatalf("expected 2 VIP port mappings, got %d: %+v", len(ports), ports)
		}
		if ports[0].Source != 31010 || ports[1].Source != 31011 {
			t.Errorf("expected overlapping tailnets to allocate 31010 and 31011, got %d and %d", ports[0].Source, ports[1].Source)
		}
	})
}

