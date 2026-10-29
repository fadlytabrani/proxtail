package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

type localDialer struct{}

func (d *localDialer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, network, address)
}

func getFreeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to get free TCP port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func getFreeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatalf("failed to get free UDP port: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func TestTCPForwardingAndHotReload(t *testing.T) {
	// Start a mock backend TCP echo server
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend TCP listener: %v", err)
	}
	defer backendListener.Close()

	go func() {
		for {
			conn, err := backendListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	proxyPort1 := getFreeTCPPort(t)
	proxyPort2 := getFreeTCPPort(t)

	initialPorts := []PortMapping{
		{
			Source:            proxyPort1,
			Destination:       backendListener.Addr().String(),
			Protocol:          "tcp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 5,
		},
	}

	dialers := map[string]Dialer{
		"test-tailnet": &localDialer{},
	}

	p := NewProxyWithDialers(initialPorts, dialers, 64*1024, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = p.Start(ctx)
	}()
	defer p.Stop()

	time.Sleep(100 * time.Millisecond)

	// Verify TCP forwarding on proxyPort1
	clientConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort1), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to proxy port 1: %v", err)
	}
	defer clientConn.Close()

	payload := []byte("hello proxtail tcp")
	if _, err := clientConn.Write(payload); err != nil {
		t.Fatalf("failed to write payload: %v", err)
	}

	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(clientConn, reply); err != nil {
		t.Fatalf("failed to read reply: %v", err)
	}
	if !bytes.Equal(payload, reply) {
		t.Fatalf("expected %q, got %q", payload, reply)
	}

	// Test hot reload: add proxyPort2 while keeping proxyPort1
	updatedPorts := []PortMapping{
		initialPorts[0],
		{
			Source:            proxyPort2,
			Destination:       backendListener.Addr().String(),
			Protocol:          "tcp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 5,
		},
	}
	if err := p.ReloadConfig(updatedPorts); err != nil {
		t.Fatalf("ReloadConfig failed: %v", err)
	}

	clientConn2, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort2), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to newly reloaded proxy port 2: %v", err)
	}
	defer clientConn2.Close()

	if _, err := clientConn2.Write(payload); err != nil {
		t.Fatalf("failed to write to port 2: %v", err)
	}
	if _, err := io.ReadFull(clientConn2, reply); err != nil {
		t.Fatalf("failed to read from port 2: %v", err)
	}

	stats := p.GetStats()
	if stats.BytesTransferred == 0 {
		t.Errorf("expected BytesTransferred > 0, got 0")
	}
}

func TestUDPForwarding(t *testing.T) {
	// Start a mock backend UDP echo server
	backendConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend UDP server: %v", err)
	}
	defer backendConn.Close()

	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := backendConn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = backendConn.WriteTo(buf[:n], addr)
		}
	}()

	proxyUDPPort := getFreeUDPPort(t)
	ports := []PortMapping{
		{
			Source:            proxyUDPPort,
			Destination:       backendConn.LocalAddr().String(),
			Protocol:          "udp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 5,
		},
	}

	dialers := map[string]Dialer{
		"test-tailnet": &localDialer{},
	}

	p := NewProxyWithDialers(ports, dialers, 64*1024, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = p.Start(ctx)
	}()
	defer p.Stop()

	time.Sleep(100 * time.Millisecond)

	clientConn, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", proxyUDPPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial UDP proxy: %v", err)
	}
	defer clientConn.Close()

	msg := []byte("hello proxtail udp")
	if _, err := clientConn.Write(msg); err != nil {
		t.Fatalf("failed to write UDP packet: %v", err)
	}

	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp := make([]byte, len(msg))
	if _, err := io.ReadFull(clientConn, resp); err != nil {
		t.Fatalf("failed to read UDP response: %v", err)
	}
	if !bytes.Equal(msg, resp) {
		t.Fatalf("expected UDP response %q, got %q", msg, resp)
	}

	stats := p.GetStats()
	if stats.UDPPackets != 1 {
		t.Errorf("expected UDPPackets == 1, got %d", stats.UDPPackets)
	}
	expectedBytes := int64(len(msg) * 2) // 1x client->backend + 1x backend->client
	if stats.BytesTransferred != expectedBytes {
		t.Errorf("expected exact BytesTransferred == %d (no double counting), got %d", expectedBytes, stats.BytesTransferred)
	}
}

func TestUDPConcurrentClientsAndMultiPacket(t *testing.T) {
	// Backend UDP server sends 2 replies for every incoming datagram, echoing client payload with prefix
	backendConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend UDP server: %v", err)
	}
	defer backendConn.Close()

	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := backendConn.ReadFrom(buf)
			if err != nil {
				return
			}
			payload := string(buf[:n])
			if payload == "one-way" {
				// Unidirectional packet: do not reply
				continue
			}
			_, _ = backendConn.WriteTo([]byte("reply1:"+payload), addr)
			_, _ = backendConn.WriteTo([]byte("reply2:"+payload), addr)
		}
	}()

	proxyUDPPort := getFreeUDPPort(t)
	ports := []PortMapping{
		{
			Source:            proxyUDPPort,
			Destination:       backendConn.LocalAddr().String(),
			Protocol:          "udp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 2,
		},
	}

	dialers := map[string]Dialer{
		"test-tailnet": &localDialer{},
	}

	p := NewProxyWithDialers(ports, dialers, 64*1024, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = p.Start(ctx)
	}()
	defer p.Stop()

	time.Sleep(100 * time.Millisecond)

	// Send unidirectional packet first (should not block or error)
	oneWayConn, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", proxyUDPPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial UDP proxy: %v", err)
	}
	defer oneWayConn.Close()
	if _, err := oneWayConn.Write([]byte("one-way")); err != nil {
		t.Fatalf("failed to write unidirectional packet: %v", err)
	}

	// Test 4 concurrent clients to ensure per-client session isolation and multi-packet reception
	const numClients = 4
	errCh := make(chan error, numClients)
	for i := 0; i < numClients; i++ {
		clientID := i
		go func() {
			c, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", proxyUDPPort), 2*time.Second)
			if err != nil {
				errCh <- err
				return
			}
			defer c.Close()

			token := fmt.Sprintf("client-%d", clientID)
			if _, err := c.Write([]byte(token)); err != nil {
				errCh <- err
				return
			}

			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 128)
			for r := 1; r <= 2; r++ {
				n, err := c.Read(buf)
				if err != nil {
					errCh <- fmt.Errorf("client %d read %d error: %w", clientID, r, err)
					return
				}
				got := string(buf[:n])
				want := fmt.Sprintf("reply%d:%s", r, token)
				if got != want {
					errCh <- fmt.Errorf("client %d packet mixup: got %q, want %q", clientID, got, want)
					return
				}
			}
			errCh <- nil
		}()
	}

	for i := 0; i < numClients; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
}

type chunkWriter struct {
	maxChunk int
	buf      bytes.Buffer
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	if w.maxChunk <= 0 {
		return 0, nil
	}
	n := len(p)
	if n > w.maxChunk {
		n = w.maxChunk
	}
	return w.buf.Write(p[:n])
}

func TestOptimizedBufferShortWriteHandling(t *testing.T) {
	ob := NewOptimizedBuffer(64, nil)
	payload := []byte("0123456789abcdef")
	_, _ = ob.Write(payload)

	cw := &chunkWriter{maxChunk: 3}
	written, err := ob.WriteTo(cw)
	if err != nil {
		t.Fatalf("expected WriteTo to succeed across partial writes, got error: %v", err)
	}
	if written != int64(len(payload)) {
		t.Fatalf("expected written=%d, got %d", len(payload), written)
	}
	if !bytes.Equal(cw.buf.Bytes(), payload) {
		t.Fatalf("expected %q, got %q", payload, cw.buf.Bytes())
	}

	// Verify zero-progress writer returns io.ErrShortWrite
	ob.Reset()
	_, _ = ob.Write(payload)
	zeroWriter := &chunkWriter{maxChunk: 0}
	if _, err := ob.WriteTo(zeroWriter); err != io.ErrShortWrite {
		t.Fatalf("expected io.ErrShortWrite on zero-length write, got %v", err)
	}
}

func TestStartRollbackOnPortConflict(t *testing.T) {
	port1 := getFreeTCPPort(t)
	// Hold port2 occupied so Proxy.Start fails on the second listener
	occupiedListener, err := net.Listen("tcp", fmt.Sprintf(":%d", 0))
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer occupiedListener.Close()
	occupiedPort := occupiedListener.Addr().(*net.TCPAddr).Port

	ports := []PortMapping{
		{
			Source:      port1,
			Destination: "127.0.0.1:8080",
			Protocol:    "tcp",
			Tailnet:     "test-tailnet",
		},
		{
			Source:      occupiedPort,
			Destination: "127.0.0.1:8081",
			Protocol:    "tcp",
			Tailnet:     "test-tailnet",
		},
	}

	dialers := map[string]Dialer{
		"test-tailnet": &localDialer{},
	}

	p := NewProxyWithDialers(ports, dialers, 64*1024, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := p.Start(ctx); err == nil {
		t.Fatalf("expected Start to fail due to occupied port %d, got nil", occupiedPort)
	}

	// Verify port1 was rolled back and can be bound immediately
	checkListener, err := net.Listen("tcp", fmt.Sprintf(":%d", port1))
	if err != nil {
		t.Fatalf("expected port1 (%d) to be rolled back and free after failed Start, got: %v", port1, err)
	}
	_ = checkListener.Close()
}

func TestMultiTailnetRoutingAndLargePayload(t *testing.T) {
	// Backend 1 (simulating "prod" tailnet)
	prodBackend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start prod backend: %v", err)
	}
	defer prodBackend.Close()

	go func() {
		for {
			conn, err := prodBackend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	// Backend 2 (simulating "staging" tailnet, prepends "STAGING:" to responses)
	stagingBackend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start staging backend: %v", err)
	}
	defer stagingBackend.Close()

	go func() {
		for {
			conn, err := stagingBackend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				data, _ := io.ReadAll(c)
				_, _ = c.Write(append([]byte("STAGING:"), data...))
			}(conn)
		}
	}()

	prodProxyPort := getFreeTCPPort(t)
	stagingProxyPort := getFreeTCPPort(t)

	ports := []PortMapping{
		{
			Source:            prodProxyPort,
			Destination:       prodBackend.Addr().String(),
			Protocol:          "tcp",
			Tailnet:           "prod",
			ConnectionTimeout: 5,
		},
		{
			Source:            stagingProxyPort,
			Destination:       stagingBackend.Addr().String(),
			Protocol:          "tcp",
			Tailnet:           "staging",
			ConnectionTimeout: 5,
		},
	}

	dialers := map[string]Dialer{
		"prod":    &localDialer{},
		"staging": &localDialer{},
	}

	// Use a small 16KB buffer to exercise multi-chunk streaming with a 512KB payload
	p := NewProxyWithDialers(ports, dialers, 16*1024, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = p.Start(ctx)
	}()
	defer p.Stop()

	time.Sleep(100 * time.Millisecond)

	// 1. Stream 512KB through prod tailnet (32x buffer size)
	largePayload := bytes.Repeat([]byte("abcdefgh01234567"), 32*1024) // 512 KB
	prodConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", prodProxyPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to prod proxy port: %v", err)
	}
	defer prodConn.Close()

	writeErrCh := make(chan error, 1)
	go func() {
		_, err := prodConn.Write(largePayload)
		if tcpConn, ok := prodConn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
		writeErrCh <- err
	}()

	received, err := io.ReadAll(prodConn)
	if err != nil {
		t.Fatalf("failed reading large payload from prod proxy: %v", err)
	}
	if err := <-writeErrCh; err != nil {
		t.Fatalf("failed writing large payload to prod proxy: %v", err)
	}
	if !bytes.Equal(received, largePayload) {
		t.Fatalf("large payload mismatch: got %d bytes, want %d bytes", len(received), len(largePayload))
	}

	// 2. Verify staging tailnet routes to staging backend and handles half-close
	stagingConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", stagingProxyPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to staging proxy port: %v", err)
	}
	defer stagingConn.Close()

	if _, err := stagingConn.Write([]byte("ping")); err != nil {
		t.Fatalf("failed to write to staging proxy: %v", err)
	}
	if tcpConn, ok := stagingConn.(*net.TCPConn); ok {
		_ = tcpConn.CloseWrite()
	}

	stagingResp, err := io.ReadAll(stagingConn)
	if err != nil {
		t.Fatalf("failed reading from staging proxy: %v", err)
	}
	if string(stagingResp) != "STAGING:ping" {
		t.Fatalf("expected 'STAGING:ping', got %q", string(stagingResp))
	}
}

func TestZeroDowntimeHotReloadModifyRemoveAndTailnetRestart(t *testing.T) {
	// Backend A returns "BACKEND-A:<msg>"
	backendA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend A: %v", err)
	}
	defer backendA.Close()
	go func() {
		for {
			conn, err := backendA.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						_, _ = c.Write(append([]byte("A:"), buf[:n]...))
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	// Backend B returns "B:<msg>"
	backendB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend B: %v", err)
	}
	defer backendB.Close()
	go func() {
		for {
			conn, err := backendB.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						_, _ = c.Write(append([]byte("B:"), buf[:n]...))
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	keptPort := getFreeTCPPort(t)
	modifiedPort := getFreeTCPPort(t)
	removedPort := getFreeTCPPort(t)

	initialPorts := []PortMapping{
		{Source: keptPort, Destination: backendA.Addr().String(), Protocol: "tcp", Tailnet: "t1", ConnectionTimeout: 5},
		{Source: modifiedPort, Destination: backendA.Addr().String(), Protocol: "tcp", Tailnet: "t1", ConnectionTimeout: 5},
		{Source: removedPort, Destination: backendA.Addr().String(), Protocol: "tcp", Tailnet: "t1", ConnectionTimeout: 5},
	}

	dialers := map[string]Dialer{"t1": &localDialer{}}
	p := NewProxyWithDialers(initialPorts, dialers, 32*1024, 100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(100 * time.Millisecond)

	// Open a long-lived connection on keptPort BEFORE hot reload
	activeConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", keptPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial keptPort: %v", err)
	}
	defer activeConn.Close()

	if _, err := activeConn.Write([]byte("before")); err != nil {
		t.Fatalf("write failed on activeConn: %v", err)
	}
	reply := make([]byte, 8)
	if _, err := io.ReadFull(activeConn, reply); err != nil || string(reply) != "A:before" {
		t.Fatalf("unexpected reply before reload: %q (err=%v)", string(reply), err)
	}

	// Hot reload:
	// - keptPort stays untouched
	// - modifiedPort switches destination from backendA to backendB
	// - removedPort is removed
	reloadedPorts := []PortMapping{
		{Source: keptPort, Destination: backendA.Addr().String(), Protocol: "tcp", Tailnet: "t1", ConnectionTimeout: 5},
		{Source: modifiedPort, Destination: backendB.Addr().String(), Protocol: "tcp", Tailnet: "t1", ConnectionTimeout: 5},
	}
	if err := p.ReloadConfig(reloadedPorts); err != nil {
		t.Fatalf("ReloadConfig failed: %v", err)
	}

	// 1. Verify activeConn on keptPort was NOT dropped during hot reload
	if _, err := activeConn.Write([]byte("after!")); err != nil {
		t.Fatalf("write failed on activeConn after reload: %v", err)
	}
	if _, err := io.ReadFull(activeConn, reply); err != nil || string(reply) != "A:after!" {
		t.Fatalf("unexpected reply after reload on activeConn: %q (err=%v)", string(reply), err)
	}

	// 2. Verify modifiedPort now routes to backendB
	modConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", modifiedPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial modifiedPort: %v", err)
	}
	defer modConn.Close()
	if _, err := modConn.Write([]byte("hello")); err != nil {
		t.Fatalf("failed writing to modifiedPort: %v", err)
	}
	modReply := make([]byte, 7)
	if _, err := io.ReadFull(modConn, modReply); err != nil || string(modReply) != "B:hello" {
		t.Fatalf("expected modifiedPort to route to backend B ('B:hello'), got %q (err=%v)", string(modReply), err)
	}

	// 3. Verify removedPort is closed and refuses connections
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", removedPort), 300*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("expected removedPort (%d) to refuse connections after reload", removedPort)
	}

	// 4. Verify RestartTailnetPorts restarts listeners for tailnet "t1"
	if err := p.RestartTailnetPorts(map[string]bool{"t1": true}); err != nil {
		t.Fatalf("RestartTailnetPorts failed: %v", err)
	}
	postRestartConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", modifiedPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial modifiedPort after RestartTailnetPorts: %v", err)
	}
	defer postRestartConn.Close()
	if _, err := postRestartConn.Write([]byte("again")); err != nil {
		t.Fatalf("failed writing after RestartTailnetPorts: %v", err)
	}
	if _, err := io.ReadFull(postRestartConn, modReply); err != nil || string(modReply) != "B:again" {
		t.Fatalf("unexpected reply after RestartTailnetPorts: %q (err=%v)", string(modReply), err)
	}
}

func TestDialFailureIncrementsErrorStats(t *testing.T) {
	closedBackendPort := getFreeTCPPort(t)
	proxyPort := getFreeTCPPort(t)

	ports := []PortMapping{
		{
			Source:            proxyPort,
			Destination:       fmt.Sprintf("127.0.0.1:%d", closedBackendPort),
			Protocol:          "tcp",
			Tailnet:           "t1",
			ConnectionTimeout: 1,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"t1": &localDialer{}}, 16*1024, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(100 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to proxy port: %v", err)
	}
	// Proxy accepts the client connection, fails to dial the closed backend, and closes client conn
	_, _ = io.ReadAll(conn)
	_ = conn.Close()

	time.Sleep(50 * time.Millisecond)
	stats := p.GetStats()
	if stats.ErrorCount == 0 {
		t.Errorf("expected ErrorCount > 0 when backend is unreachable, got 0")
	}
	if stats.TCPConnections != 0 {
		t.Errorf("expected active TCPConnections to return to 0 after failed dial, got %d", stats.TCPConnections)
	}
}

// virtualTailnetDialer simulates an isolated tailnet namespace where virtual addresses
// (such as overlapping "100.64.0.10:8080" or MagicDNS names) resolve to tailnet-specific local listeners.
type virtualTailnetDialer struct {
	name      string
	tcpRoutes map[string]string
	udpRoutes map[string]string
}

func (d *virtualTailnetDialer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	var target string
	switch network {
	case "tcp":
		target = d.tcpRoutes[address]
	case "udp":
		target = d.udpRoutes[address]
	}
	if target == "" {
		return nil, fmt.Errorf("tailnet %s: unknown %s destination %s", d.name, network, address)
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, target)
}

func startTaggedTCPServer(t *testing.T, tag string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start TCP server (%s): %v", tag, err)
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
						_, _ = c.Write([]byte(tag + ":" + string(buf[:n])))
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln
}

func startTaggedUDPServer(t *testing.T, tag string) net.PacketConn {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start UDP server (%s): %v", tag, err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n > 0 {
				_, _ = pc.WriteTo([]byte(tag+":"+string(buf[:n])), addr)
			}
		}
	}()
	return pc
}

func TestMultiTailnetIsolationOverlappingDestinationsAndDynamicLifecycle(t *testing.T) {
	// All three tailnets expose the EXACT same virtual Tailscale IP/port ("100.64.0.10:8080" and "100.64.0.10:5353")
	const sharedTCPDest = "100.64.0.10:8080"
	const sharedUDPDest = "100.64.0.10:5353"

	alphaTCP := startTaggedTCPServer(t, "ALPHA-TCP")
	alphaUDP := startTaggedUDPServer(t, "ALPHA-UDP")

	betaTCPv1 := startTaggedTCPServer(t, "BETA-V1-TCP")
	betaUDPv1 := startTaggedUDPServer(t, "BETA-V1-UDP")

	betaTCPv2 := startTaggedTCPServer(t, "BETA-V2-TCP")
	betaUDPv2 := startTaggedUDPServer(t, "BETA-V2-UDP")

	gammaTCP := startTaggedTCPServer(t, "GAMMA-TCP")
	gammaUDP := startTaggedUDPServer(t, "GAMMA-UDP")

	alphaDialer := &virtualTailnetDialer{
		name:      "tailnet-alpha",
		tcpRoutes: map[string]string{sharedTCPDest: alphaTCP.Addr().String()},
		udpRoutes: map[string]string{sharedUDPDest: alphaUDP.LocalAddr().String()},
	}
	betaDialerV1 := &virtualTailnetDialer{
		name:      "tailnet-beta",
		tcpRoutes: map[string]string{sharedTCPDest: betaTCPv1.Addr().String()},
		udpRoutes: map[string]string{sharedUDPDest: betaUDPv1.LocalAddr().String()},
	}
	betaDialerV2 := &virtualTailnetDialer{
		name:      "tailnet-beta-v2",
		tcpRoutes: map[string]string{sharedTCPDest: betaTCPv2.Addr().String()},
		udpRoutes: map[string]string{sharedUDPDest: betaUDPv2.LocalAddr().String()},
	}
	gammaDialer := &virtualTailnetDialer{
		name:      "tailnet-gamma",
		tcpRoutes: map[string]string{sharedTCPDest: gammaTCP.Addr().String()},
		udpRoutes: map[string]string{sharedUDPDest: gammaUDP.LocalAddr().String()},
	}

	alphaTCPPort := getFreeTCPPort(t)
	alphaUDPPort := getFreeUDPPort(t)
	betaTCPPort := getFreeTCPPort(t)
	betaUDPPort := getFreeUDPPort(t)
	reassignTCPPort := getFreeTCPPort(t)
	gammaTCPPort := getFreeTCPPort(t)
	gammaUDPPort := getFreeUDPPort(t)

	initialPorts := []PortMapping{
		{Source: alphaTCPPort, Destination: sharedTCPDest, Protocol: "tcp", Tailnet: "tailnet-alpha", ConnectionTimeout: 5},
		{Source: alphaUDPPort, Destination: sharedUDPDest, Protocol: "udp", Tailnet: "tailnet-alpha", ConnectionTimeout: 5},
		{Source: betaTCPPort, Destination: sharedTCPDest, Protocol: "tcp", Tailnet: "tailnet-beta", ConnectionTimeout: 5},
		{Source: betaUDPPort, Destination: sharedUDPDest, Protocol: "udp", Tailnet: "tailnet-beta", ConnectionTimeout: 5},
		{Source: reassignTCPPort, Destination: sharedTCPDest, Protocol: "tcp", Tailnet: "tailnet-alpha", ConnectionTimeout: 5},
	}

	dialers := map[string]Dialer{
		"tailnet-alpha": alphaDialer,
		"tailnet-beta":  betaDialerV1,
	}

	p := NewProxyWithDialers(initialPorts, dialers, 32*1024, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(100 * time.Millisecond)

	assertTCP := func(port int, msg, want string) {
		t.Helper()
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
		if err != nil {
			t.Fatalf("TCP dial port %d failed: %v", port, err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatalf("TCP write port %d failed: %v", port, err)
		}
		buf := make([]byte, len(want))
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("TCP read port %d failed: %v", port, err)
		}
		if string(buf) != want {
			t.Fatalf("TCP port %d: got %q, want %q", port, string(buf), want)
		}
	}

	assertUDP := func(port int, msg, want string) {
		t.Helper()
		c, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
		if err != nil {
			t.Fatalf("UDP dial port %d failed: %v", port, err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatalf("UDP write port %d failed: %v", port, err)
		}
		buf := make([]byte, len(want))
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("UDP read port %d failed: %v", port, err)
		}
		if string(buf) != want {
			t.Fatalf("UDP port %d: got %q, want %q", port, string(buf), want)
		}
	}

	// 1. Verify TCP & UDP isolation across tailnet-alpha and tailnet-beta despite identical Destination strings
	assertTCP(alphaTCPPort, "ping", "ALPHA-TCP:ping")
	assertUDP(alphaUDPPort, "ping", "ALPHA-UDP:ping")
	assertTCP(betaTCPPort, "ping", "BETA-V1-TCP:ping")
	assertUDP(betaUDPPort, "ping", "BETA-V1-UDP:ping")
	assertTCP(reassignTCPPort, "check", "ALPHA-TCP:check")

	// Open a persistent TCP connection on alphaTCPPort to verify it survives multi-tailnet reloads and restarts
	persistentAlphaConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", alphaTCPPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to open persistent connection on alphaTCPPort: %v", err)
	}
	defer persistentAlphaConn.Close()

	// 2. Replace tailnet-beta's dialer (v1 -> v2) and restart ONLY tailnet-beta's ports
	p.UpdateTailnetDialers(map[string]Dialer{
		"tailnet-alpha": alphaDialer,
		"tailnet-beta":  betaDialerV2,
	})
	if err := p.RestartTailnetPorts(map[string]bool{"tailnet-beta": true}); err != nil {
		t.Fatalf("RestartTailnetPorts for tailnet-beta failed: %v", err)
	}

	assertTCP(betaTCPPort, "ping", "BETA-V2-TCP:ping")
	assertUDP(betaUDPPort, "ping", "BETA-V2-UDP:ping")

	// Verify persistentAlphaConn on tailnet-alpha was completely unaffected by tailnet-beta restart
	if _, err := persistentAlphaConn.Write([]byte("still-up")); err != nil {
		t.Fatalf("persistentAlphaConn write failed after beta restart: %v", err)
	}
	alphaBuf := make([]byte, len("ALPHA-TCP:still-up"))
	if _, err := io.ReadFull(persistentAlphaConn, alphaBuf); err != nil || string(alphaBuf) != "ALPHA-TCP:still-up" {
		t.Fatalf("persistentAlphaConn read mismatch after beta restart: got %q, err=%v", string(alphaBuf), err)
	}

	// 3. Hot reload:
	//    - Add a 3rd tailnet ("tailnet-gamma") with TCP and UDP ports
	//    - Reassign reassignTCPPort from "tailnet-alpha" to "tailnet-beta" (same destination "100.64.0.10:8080")
	p.UpdateTailnetDialers(map[string]Dialer{
		"tailnet-alpha": alphaDialer,
		"tailnet-beta":  betaDialerV2,
		"tailnet-gamma": gammaDialer,
	})
	reloadedPorts := []PortMapping{
		{Source: alphaTCPPort, Destination: sharedTCPDest, Protocol: "tcp", Tailnet: "tailnet-alpha", ConnectionTimeout: 5},
		{Source: alphaUDPPort, Destination: sharedUDPDest, Protocol: "udp", Tailnet: "tailnet-alpha", ConnectionTimeout: 5},
		{Source: betaTCPPort, Destination: sharedTCPDest, Protocol: "tcp", Tailnet: "tailnet-beta", ConnectionTimeout: 5},
		{Source: betaUDPPort, Destination: sharedUDPDest, Protocol: "udp", Tailnet: "tailnet-beta", ConnectionTimeout: 5},
		{Source: reassignTCPPort, Destination: sharedTCPDest, Protocol: "tcp", Tailnet: "tailnet-beta", ConnectionTimeout: 5},
		{Source: gammaTCPPort, Destination: sharedTCPDest, Protocol: "tcp", Tailnet: "tailnet-gamma", ConnectionTimeout: 5},
		{Source: gammaUDPPort, Destination: sharedUDPDest, Protocol: "udp", Tailnet: "tailnet-gamma", ConnectionTimeout: 5},
	}
	if err := p.ReloadConfig(reloadedPorts); err != nil {
		t.Fatalf("ReloadConfig for 3 tailnets failed: %v", err)
	}

	// Verify reassigned port now routes to tailnet-beta (BETA-V2-TCP) and new tailnet-gamma ports work
	assertTCP(reassignTCPPort, "switched", "BETA-V2-TCP:switched")
	assertTCP(gammaTCPPort, "hello", "GAMMA-TCP:hello")
	assertUDP(gammaUDPPort, "hello", "GAMMA-UDP:hello")

	// Verify persistentAlphaConn is STILL alive after adding tailnet-gamma and reassigning reassignTCPPort
	if _, err := persistentAlphaConn.Write([]byte("alive-2")); err != nil {
		t.Fatalf("persistentAlphaConn write failed after 3-tailnet reload: %v", err)
	}
	alphaBuf2 := make([]byte, len("ALPHA-TCP:alive-2"))
	if _, err := io.ReadFull(persistentAlphaConn, alphaBuf2); err != nil || string(alphaBuf2) != "ALPHA-TCP:alive-2" {
		t.Fatalf("persistentAlphaConn read mismatch after 3-tailnet reload: got %q, err=%v", string(alphaBuf2), err)
	}
}

func BenchmarkTCPThroughput(b *testing.B) {
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start backend listener: %v", err)
	}
	defer backendListener.Close()

	go func() {
		for {
			conn, err := backendListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to get free port: %v", err)
	}
	proxyPort := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	ports := []PortMapping{
		{
			Source:            proxyPort,
			Destination:       backendListener.Addr().String(),
			Protocol:          "tcp",
			Tailnet:           "bench",
			ConnectionTimeout: 30,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"bench": &localDialer{}}, 256*1024, 400)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	clientConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 2*time.Second)
	if err != nil {
		b.Fatalf("failed to dial proxy: %v", err)
	}
	defer clientConn.Close()

	chunk := bytes.Repeat([]byte("X"), 64*1024)
	recvBuf := make([]byte, len(chunk))

	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := clientConn.Write(chunk); err != nil {
			b.Fatalf("write failed: %v", err)
		}
		if _, err := io.ReadFull(clientConn, recvBuf); err != nil {
			b.Fatalf("read failed: %v", err)
		}
	}
}

func BenchmarkUDPThroughput(b *testing.B) {
	backendConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start backend UDP server: %v", err)
	}
	defer backendConn.Close()

	backendUDP := backendConn.(*net.UDPConn)
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, addrPort, err := backendUDP.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			_, _ = backendUDP.WriteToUDPAddrPort(buf[:n], addrPort)
		}
	}()

	u, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to get free UDP port: %v", err)
	}
	proxyUDPPort := u.LocalAddr().(*net.UDPAddr).Port
	_ = u.Close()

	ports := []PortMapping{
		{
			Source:            proxyUDPPort,
			Destination:       backendConn.LocalAddr().String(),
			Protocol:          "udp",
			Tailnet:           "bench",
			ConnectionTimeout: 30,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"bench": &localDialer{}}, 256*1024, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	clientConn, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", proxyUDPPort), 2*time.Second)
	if err != nil {
		b.Fatalf("failed to dial UDP proxy: %v", err)
	}
	defer clientConn.Close()

	payload := bytes.Repeat([]byte("U"), 1024)
	recvBuf := make([]byte, len(payload))

	// Warm up the UDP session before timing
	_, _ = clientConn.Write(payload)
	_, _ = io.ReadFull(clientConn, recvBuf)

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := clientConn.Write(payload); err != nil {
			b.Fatalf("udp write failed: %v", err)
		}
		if _, err := io.ReadFull(clientConn, recvBuf); err != nil {
			b.Fatalf("udp read failed: %v", err)
		}
	}
}

func BenchmarkMultiClientParallelUDPThroughput(b *testing.B) {
	backendConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start backend UDP server: %v", err)
	}
	defer backendConn.Close()

	backendUDP := backendConn.(*net.UDPConn)
	_ = backendUDP.SetReadBuffer(4 * 1024 * 1024)
	_ = backendUDP.SetWriteBuffer(4 * 1024 * 1024)

	var readerWg sync.WaitGroup
	for range 8 {
		readerWg.Go(func() {
			buf := make([]byte, 64*1024)
			for {
				n, addrPort, err := backendUDP.ReadFromUDPAddrPort(buf)
				if err != nil {
					return
				}
				_, _ = backendUDP.WriteToUDPAddrPort(buf[:n], addrPort)
			}
		})
	}

	u, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to get free UDP port: %v", err)
	}
	proxyUDPPort := u.LocalAddr().(*net.UDPAddr).Port
	_ = u.Close()

	ports := []PortMapping{
		{
			Source:            proxyUDPPort,
			ListenAddress:     "127.0.0.1",
			Destination:       backendConn.LocalAddr().String(),
			Protocol:          "udp",
			Tailnet:           "bench",
			ConnectionTimeout: 30,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"bench": &localDialer{}}, 256*1024, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	payload := bytes.Repeat([]byte("U"), 1024)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		clientConn, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", proxyUDPPort), 2*time.Second)
		if err != nil {
			b.Fatalf("failed to dial UDP proxy: %v", err)
		}
		defer clientConn.Close()

		recvBuf := make([]byte, len(payload))
		// Warm up session
		_, _ = clientConn.Write(payload)
		_, _ = io.ReadFull(clientConn, recvBuf)

		for pb.Next() {
			if _, err := clientConn.Write(payload); err != nil {
				b.Fatalf("parallel udp write failed: %v", err)
			}
			if _, err := io.ReadFull(clientConn, recvBuf); err != nil {
				b.Fatalf("parallel udp read failed: %v", err)
			}
		}
	})
}

func BenchmarkMultiTailnetParallelTCPThroughput(b *testing.B) {
	const numTailnets = 3
	dialers := make(map[string]Dialer, numTailnets)
	ports := make([]PortMapping, 0, numTailnets)
	proxyPorts := make([]int, numTailnets)

	for tIdx := 0; tIdx < numTailnets; tIdx++ {
		backendListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatalf("failed to start backend listener %d: %v", tIdx, err)
		}
		b.Cleanup(func() { _ = backendListener.Close() })

		go func(ln net.Listener) {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer c.Close()
					_, _ = io.Copy(c, c)
				}(conn)
			}
		}(backendListener)

		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatalf("failed to get free port: %v", err)
		}
		proxyPorts[tIdx] = l.Addr().(*net.TCPAddr).Port
		_ = l.Close()

		tailnetName := fmt.Sprintf("tailnet-%d", tIdx)
		dialers[tailnetName] = &localDialer{}
		ports = append(ports, PortMapping{
			Source:            proxyPorts[tIdx],
			ListenAddress:     "127.0.0.1",
			Destination:       backendListener.Addr().String(),
			Protocol:          "tcp",
			Tailnet:           tailnetName,
			ConnectionTimeout: 30,
		})
	}

	p := NewProxyWithDialers(ports, dialers, 256*1024, 400)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	chunk := bytes.Repeat([]byte("M"), 64*1024)
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()

	var clientSeq atomic.Uint32
	b.RunParallel(func(pb *testing.PB) {
		idx := int(clientSeq.Add(1)-1) % numTailnets
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPorts[idx]), 2*time.Second)
		if err != nil {
			b.Fatalf("failed to dial proxy port %d: %v", proxyPorts[idx], err)
		}
		defer conn.Close()

		recvBuf := make([]byte, len(chunk))
		for pb.Next() {
			if _, err := conn.Write(chunk); err != nil {
				b.Fatalf("parallel write failed: %v", err)
			}
			if _, err := io.ReadFull(conn, recvBuf); err != nil {
				b.Fatalf("parallel read failed: %v", err)
			}
		}
	})
}

// startRealTSNetPair spins up an in-process Tailscale control plane, STUN/DERP relay,
// and two real userspace tsnet.Server nodes (proxyNode and backendNode) that communicate
// over real WireGuard (ChaCha20-Poly1305) and gVisor netstack (gonet).
func startRealTSNetPair(tb testing.TB) (proxyNode *tsnet.Server, backendNode *tsnet.Server, backendIP netip.Addr) {
	tb.Helper()

	netns.SetEnabled(false)
	tb.Cleanup(func() {
		netns.SetEnabled(true)
	})

	derpMap := integration.RunDERPAndSTUN(tb, logger.Discard, "127.0.0.1")
	control := &testcontrol.Server{
		DERPMap: derpMap,
		DNSConfig: &tailcfg.DNSConfig{
			Proxied: true,
		},
		MagicDNSDomain: "tail-scale.ts.net",
		Logf:           logger.Discard,
	}
	controlSrv := httptest.NewServer(control)
	tb.Cleanup(controlSrv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmpDir := tb.TempDir()
	newNode := func(hostname string) *tsnet.Server {
		dir := filepath.Join(tmpDir, hostname)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			tb.Fatalf("mkdir %s: %v", dir, err)
		}
		s := &tsnet.Server{
			Dir:        dir,
			ControlURL: controlSrv.URL,
			Hostname:   hostname,
			Store:      new(mem.Store),
			Ephemeral:  true,
			Logf:       logger.Discard,
			UserLogf:   logger.Discard,
		}
		tb.Cleanup(func() { _ = s.Close() })
		return s
	}

	proxyNode = newNode("proxtail-node")
	backendNode = newNode("backend-node")

	var wg sync.WaitGroup
	var proxyErr, backendErr error
	var backendIPs []netip.Addr

	wg.Go(func() {
		_, proxyErr = proxyNode.Up(ctx)
	})
	wg.Go(func() {
		st, err := backendNode.Up(ctx)
		if err != nil {
			backendErr = err
			return
		}
		backendIPs = st.TailscaleIPs
	})
	wg.Wait()

	if proxyErr != nil {
		tb.Fatalf("proxyNode.Up failed: %v", proxyErr)
	}
	if backendErr != nil || len(backendIPs) == 0 {
		tb.Fatalf("backendNode.Up failed: %v", backendErr)
	}

	waitDERP := func(s *tsnet.Server) {
		h := s.Sys().HealthTracker.Get()
		ms := s.Sys().MagicSock.Get()
		for {
			if r := ms.GetLastNetcheckReport(ctx); r != nil &&
				r.PreferredDERP != 0 &&
				!h.GetDERPRegionReceivedTime(r.PreferredDERP).IsZero() {
				return
			}
			select {
			case <-ctx.Done():
				tb.Fatalf("timed out waiting for DERP on %s", s.Hostname)
			case <-time.After(15 * time.Millisecond):
			}
		}
	}
	waitDERP(proxyNode)
	waitDERP(backendNode)

	return proxyNode, backendNode, backendIPs[0]
}

// BenchmarkRealTSNetWireGuardTCPThroughput measures end-to-end TCP throughput through
// proxtail -> tsnet (gVisor gonet + wireguard-go ChaCha20-Poly1305) -> peer tsnet node.
func BenchmarkRealTSNetWireGuardTCPThroughput(b *testing.B) {
	proxyNode, backendNode, backendIP := startRealTSNetPair(b)

	backendLn, err := backendNode.Listen("tcp", ":8080")
	if err != nil {
		b.Fatalf("backendNode.Listen failed: %v", err)
	}
	defer backendLn.Close()

	go func() {
		for {
			conn, err := backendLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to get free TCP port: %v", err)
	}
	proxyPort := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	ports := []PortMapping{
		{
			Source:            proxyPort,
			ListenAddress:     "127.0.0.1",
			Destination:       net.JoinHostPort(backendIP.String(), "8080"),
			Protocol:          "tcp",
			Tailnet:           "real-tsnet",
			ConnectionTimeout: 60,
		},
	}

	p := NewProxyWithMultiTailnet(ports, map[string]*tsnet.Server{"real-tsnet": proxyNode}, 512*1024, 200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	clientConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 10*time.Second)
	if err != nil {
		b.Fatalf("failed to dial proxy: %v", err)
	}
	defer clientConn.Close()

	chunk := bytes.Repeat([]byte("W"), 256*1024)
	recvBuf := make([]byte, len(chunk))

	// Warm up WireGuard handshake & direct peer-to-peer UDP upgrade before timing
	for range 4 {
		if _, err := clientConn.Write(chunk); err != nil {
			b.Fatalf("warmup write failed: %v", err)
		}
		if _, err := io.ReadFull(clientConn, recvBuf); err != nil {
			b.Fatalf("warmup read failed: %v", err)
		}
	}

	// Each iteration streams chunk outbound and receives chunk inbound (full-duplex pipelined, like iperf3 --bidir)
	b.SetBytes(int64(2 * len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()

	readErrCh := make(chan error, 1)
	go func() {
		buf := make([]byte, len(chunk))
		for i := 0; i < b.N; i++ {
			if _, err := io.ReadFull(clientConn, buf); err != nil {
				readErrCh <- err
				return
			}
		}
		readErrCh <- nil
	}()

	for i := 0; i < b.N; i++ {
		if _, err := clientConn.Write(chunk); err != nil {
			b.Fatalf("write failed: %v", err)
		}
	}
	if err := <-readErrCh; err != nil {
		b.Fatalf("read failed: %v", err)
	}
	b.StopTimer()
}

// BenchmarkRealTSNetWireGuardTCPParallelThroughput measures multi-stream concurrent TCP
// throughput across a real encrypted tsnet WireGuard tunnel.
func BenchmarkRealTSNetWireGuardTCPParallelThroughput(b *testing.B) {
	proxyNode, backendNode, backendIP := startRealTSNetPair(b)

	backendLn, err := backendNode.Listen("tcp", ":8080")
	if err != nil {
		b.Fatalf("backendNode.Listen failed: %v", err)
	}
	defer backendLn.Close()

	go func() {
		for {
			conn, err := backendLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to get free TCP port: %v", err)
	}
	proxyPort := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	ports := []PortMapping{
		{
			Source:            proxyPort,
			ListenAddress:     "127.0.0.1",
			Destination:       net.JoinHostPort(backendIP.String(), "8080"),
			Protocol:          "tcp",
			Tailnet:           "real-tsnet",
			ConnectionTimeout: 60,
		},
	}

	p := NewProxyWithMultiTailnet(ports, map[string]*tsnet.Server{"real-tsnet": proxyNode}, 512*1024, 200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	chunk := bytes.Repeat([]byte("P"), 128*1024)

	// Warm up WireGuard tunnel with one initial round-trip
	warmConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 10*time.Second)
	if err != nil {
		b.Fatalf("warmup dial failed: %v", err)
	}
	warmBuf := make([]byte, len(chunk))
	if _, err := warmConn.Write(chunk); err != nil {
		b.Fatalf("warmup write failed: %v", err)
	}
	if _, err := io.ReadFull(warmConn, warmBuf); err != nil {
		b.Fatalf("warmup read failed: %v", err)
	}
	_ = warmConn.Close()

	b.SetBytes(int64(2 * len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 10*time.Second)
		if err != nil {
			b.Fatalf("parallel dial failed: %v", err)
		}
		defer conn.Close()

		recvBuf := make([]byte, len(chunk))
		for pb.Next() {
			if _, err := conn.Write(chunk); err != nil {
				b.Fatalf("parallel write failed: %v", err)
			}
			if _, err := io.ReadFull(conn, recvBuf); err != nil {
				b.Fatalf("parallel read failed: %v", err)
			}
		}
	})
}

// BenchmarkRealTSNetWireGuardUDPThroughput measures end-to-end UDP datagram forwarding
// through proxtail -> tsnet WireGuard -> peer tsnet node.
func BenchmarkRealTSNetWireGuardUDPThroughput(b *testing.B) {
	proxyNode, backendNode, backendIP := startRealTSNetPair(b)

	backendAddr := net.JoinHostPort(backendIP.String(), "5353")
	backendPC, err := backendNode.ListenPacket("udp", backendAddr)
	if err != nil {
		b.Fatalf("backendNode.ListenPacket failed: %v", err)
	}
	defer backendPC.Close()

	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, addr, err := backendPC.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = backendPC.WriteTo(buf[:n], addr)
		}
	}()

	u, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to get free UDP port: %v", err)
	}
	proxyUDPPort := u.LocalAddr().(*net.UDPAddr).Port
	_ = u.Close()

	ports := []PortMapping{
		{
			Source:            proxyUDPPort,
			ListenAddress:     "127.0.0.1",
			Destination:       backendAddr,
			Protocol:          "udp",
			Tailnet:           "real-tsnet",
			ConnectionTimeout: 60,
		},
	}

	p := NewProxyWithMultiTailnet(ports, map[string]*tsnet.Server{"real-tsnet": proxyNode}, 512*1024, 200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	clientConn, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", proxyUDPPort), 5*time.Second)
	if err != nil {
		b.Fatalf("failed to dial UDP proxy: %v", err)
	}
	defer clientConn.Close()

	payload := bytes.Repeat([]byte("U"), 1024)
	recvBuf := make([]byte, len(payload))

	// Warm up WireGuard tunnel over UDP (first packet triggers DISCO/WireGuard handshake)
	for range 10 {
		_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = clientConn.Write(payload)
		if _, err := io.ReadFull(clientConn, recvBuf); err == nil {
			break
		}
	}
	_ = clientConn.SetDeadline(time.Time{})

	b.SetBytes(int64(2 * len(payload)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := clientConn.Write(payload); err != nil {
			b.Fatalf("udp write failed: %v", err)
		}
		if _, err := io.ReadFull(clientConn, recvBuf); err != nil {
			b.Fatalf("udp read failed: %v", err)
		}
	}
}

// BenchmarkRealTSNetWireGuardUDPParallelThroughput measures multi-client concurrent UDP
// datagram forwarding across a real encrypted tsnet WireGuard tunnel using the multi-core reader pool.
func BenchmarkRealTSNetWireGuardUDPParallelThroughput(b *testing.B) {
	proxyNode, backendNode, backendIP := startRealTSNetPair(b)

	backendAddr := net.JoinHostPort(backendIP.String(), "5353")
	backendPC, err := backendNode.ListenPacket("udp", backendAddr)
	if err != nil {
		b.Fatalf("backendNode.ListenPacket failed: %v", err)
	}
	defer backendPC.Close()

	for range 8 {
		go func() {
			buf := make([]byte, 64*1024)
			for {
				n, addr, err := backendPC.ReadFrom(buf)
				if err != nil {
					return
				}
				_, _ = backendPC.WriteTo(buf[:n], addr)
			}
		}()
	}

	u, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to get free UDP port: %v", err)
	}
	proxyUDPPort := u.LocalAddr().(*net.UDPAddr).Port
	_ = u.Close()

	ports := []PortMapping{
		{
			Source:            proxyUDPPort,
			ListenAddress:     "127.0.0.1",
			Destination:       backendAddr,
			Protocol:          "udp",
			Tailnet:           "real-tsnet",
			ConnectionTimeout: 60,
		},
	}

	p := NewProxyWithMultiTailnet(ports, map[string]*tsnet.Server{"real-tsnet": proxyNode}, 512*1024, 200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	payload := bytes.Repeat([]byte("U"), 1024)

	// Warm up WireGuard tunnel with one UDP client before starting parallel workers
	warmConn, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", proxyUDPPort), 5*time.Second)
	if err != nil {
		b.Fatalf("warmup dial failed: %v", err)
	}
	warmBuf := make([]byte, len(payload))
	for range 10 {
		_ = warmConn.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = warmConn.Write(payload)
		if _, err := io.ReadFull(warmConn, warmBuf); err == nil {
			break
		}
	}
	_ = warmConn.Close()

	b.SetBytes(int64(2 * len(payload)))
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		clientConn, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", proxyUDPPort), 5*time.Second)
		if err != nil {
			b.Fatalf("parallel udp dial failed: %v", err)
		}
		defer clientConn.Close()

		recvBuf := make([]byte, len(payload))
		for pb.Next() {
			if _, err := clientConn.Write(payload); err != nil {
				b.Fatalf("parallel udp write failed: %v", err)
			}
			if _, err := io.ReadFull(clientConn, recvBuf); err != nil {
				b.Fatalf("parallel udp read failed: %v", err)
			}
		}
	})
}

func TestTCPUpstreamProbeSuspendAndResume(t *testing.T) {
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend TCP listener: %v", err)
	}
	backendAddr := backendListener.Addr().String()

	serveEcho := func(ln net.Listener) {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}
	go serveEcho(backendListener)

	proxyPort := getFreeTCPPort(t)
	ports := []PortMapping{
		{
			Source:            proxyPort,
			ListenAddress:     "127.0.0.1",
			Destination:       backendAddr,
			Protocol:          "tcp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 2,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"test-tailnet": &localDialer{}}, 32*1024, 50)
	p.forceUpstreamProbe = true
	p.probeInitialDelay = 20 * time.Millisecond
	p.probeInterval = 40 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	// 1. Initially UP
	statuses := p.GetPortStatuses()
	if len(statuses) != 1 || !statuses[0].Up {
		t.Fatalf("expected TCP listener to be initially UP, got %+v", statuses)
	}

	// 2. Stop backend -> probe detects unreachable upstream and suspends local TCP listener
	_ = backendListener.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := p.GetPortStatuses()
		if len(st) == 1 && !st[0].Up {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	statuses = p.GetPortStatuses()
	if len(statuses) != 1 || statuses[0].Up {
		t.Fatalf("expected TCP listener to be suspended (Up=false) when upstream is down, got %+v", statuses)
	}

	// Verify local port refuses TCP connections while suspended
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("expected local TCP listener on port %d to refuse connections while suspended", proxyPort)
	}

	// 3. Restart backend on the same address -> probe detects recovery and resumes local TCP listener
	restartedBackend, err := net.Listen("tcp", backendAddr)
	if err != nil {
		t.Fatalf("failed to restart backend TCP listener on %s: %v", backendAddr, err)
	}
	defer restartedBackend.Close()
	go serveEcho(restartedBackend)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := p.GetPortStatuses()
		if len(st) == 1 && st[0].Up {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	statuses = p.GetPortStatuses()
	if len(statuses) != 1 || !statuses[0].Up {
		t.Fatalf("expected TCP listener to resume (Up=true) after upstream recovery, got %+v", statuses)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), time.Second)
	if err != nil {
		t.Fatalf("failed to connect to resumed TCP listener: %v", err)
	}
	defer conn.Close()
	msg := []byte("resumed-tcp")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	reply := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, reply); err != nil || !bytes.Equal(reply, msg) {
		t.Fatalf("unexpected reply after TCP resume: %q (err=%v)", reply, err)
	}
}

func TestUDPUpstreamProbeSuspendAndResume(t *testing.T) {
	backendConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend UDP server: %v", err)
	}
	backendAddr := backendConn.LocalAddr().String()

	serveUDPEcho := func(pc net.PacketConn) {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n > 0 {
				_, _ = pc.WriteTo(buf[:n], addr)
			}
		}
	}
	go serveUDPEcho(backendConn)

	proxyUDPPort := getFreeUDPPort(t)
	ports := []PortMapping{
		{
			Source:            proxyUDPPort,
			ListenAddress:     "127.0.0.1",
			Destination:       backendAddr,
			Protocol:          "udp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 2,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"test-tailnet": &localDialer{}}, 32*1024, 50)
	p.forceUpstreamProbe = true
	p.probeInitialDelay = 20 * time.Millisecond
	p.probeInterval = 40 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	// 1. Initially UP
	statuses := p.GetPortStatuses()
	if len(statuses) != 1 || !statuses[0].Up {
		t.Fatalf("expected UDP socket to be initially UP, got %+v", statuses)
	}

	// 2. Close backend UDP socket -> probe receives ICMP Port Unreachable (ECONNREFUSED) and suspends local UDP socket
	_ = backendConn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := p.GetPortStatuses()
		if len(st) == 1 && !st[0].Up {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	statuses = p.GetPortStatuses()
	if len(statuses) != 1 || statuses[0].Up {
		t.Fatalf("expected UDP socket to be suspended (Up=false) when upstream UDP port is closed, got %+v", statuses)
	}

	// 3. Restart backend UDP socket on the same address -> probe detects recovery and re-binds local UDP socket
	restartedUDP, err := net.ListenPacket("udp", backendAddr)
	if err != nil {
		t.Fatalf("failed to restart backend UDP server on %s: %v", backendAddr, err)
	}
	defer restartedUDP.Close()
	go serveUDPEcho(restartedUDP)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := p.GetPortStatuses()
		if len(st) == 1 && st[0].Up {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	statuses = p.GetPortStatuses()
	if len(statuses) != 1 || !statuses[0].Up {
		t.Fatalf("expected UDP socket to resume (Up=true) after upstream UDP recovery, got %+v", statuses)
	}

	// Verify UDP forwarding works after resumption
	clientConn, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", proxyUDPPort), time.Second)
	if err != nil {
		t.Fatalf("failed to dial resumed UDP proxy: %v", err)
	}
	defer clientConn.Close()
	msg := []byte("resumed-udp")
	if _, err := clientConn.Write(msg); err != nil {
		t.Fatalf("failed writing UDP packet after resume: %v", err)
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(time.Second))
	reply := make([]byte, len(msg))
	if _, err := io.ReadFull(clientConn, reply); err != nil || !bytes.Equal(reply, msg) {
		t.Fatalf("unexpected UDP reply after resume: %q (err=%v)", reply, err)
	}
}
