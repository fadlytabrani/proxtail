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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/key"
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

func TestHotReloadToZeroPortsAndBack(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend: %v", err)
	}
	defer backend.Close()
	go func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4)
				_, _ = io.ReadFull(c, buf)
				_, _ = c.Write([]byte("ACK:" + string(buf)))
			}(conn)
		}
	}()

	proxyPort := getFreeTCPPort(t)
	initialPorts := []PortMapping{
		{
			Source:            proxyPort,
			Destination:       backend.Addr().String(),
			Protocol:          "tcp",
			Tailnet:           "t1",
			ConnectionTimeout: 5,
		},
	}

	p := NewProxyWithDialers(initialPorts, map[string]Dialer{"t1": &localDialer{}}, 16*1024, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	// 1. Initial port works
	c1, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to initial proxy port: %v", err)
	}
	_, _ = c1.Write([]byte("test"))
	reply := make([]byte, 8)
	if _, err := io.ReadFull(c1, reply); err != nil || string(reply) != "ACK:test" {
		t.Fatalf("unexpected reply: %q (err=%v)", string(reply), err)
	}
	_ = c1.Close()

	// 2. Reload to 0 ports (draining mode)
	if err := p.ReloadConfig([]PortMapping{}); err != nil {
		t.Fatalf("ReloadConfig with 0 ports failed: %v", err)
	}

	if count := p.ActiveListenerCount(); count != 0 {
		t.Fatalf("expected 0 active listeners after reload to empty ports, got %d", count)
	}
	if !p.IsReady() {
		t.Fatalf("expected IsReady() to be true with 0 ports")
	}

	// Verify old port is closed and connection refused
	_, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 200*time.Millisecond)
	if err == nil {
		t.Fatalf("expected dial to closed proxy port to fail, but succeeded")
	}

	// 3. Reload back to 1 port
	newProxyPort := getFreeTCPPort(t)
	restoredPorts := []PortMapping{
		{
			Source:            newProxyPort,
			Destination:       backend.Addr().String(),
			Protocol:          "tcp",
			Tailnet:           "t1",
			ConnectionTimeout: 5,
		},
	}
	if err := p.ReloadConfig(restoredPorts); err != nil {
		t.Fatalf("ReloadConfig back to 1 port failed: %v", err)
	}
	if count := p.ActiveListenerCount(); count != 1 {
		t.Fatalf("expected 1 active listener after reload, got %d", count)
	}

	// Verify new port works
	c2, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", newProxyPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to restored proxy port: %v", err)
	}
	defer c2.Close()
	_, _ = c2.Write([]byte("ping"))
	reply2 := make([]byte, 8)
	if _, err := io.ReadFull(c2, reply2); err != nil || string(reply2) != "ACK:ping" {
		t.Fatalf("unexpected reply on restored port: %q (err=%v)", string(reply2), err)
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
	p.probeInitialDelay = 10 * time.Millisecond
	p.SetProbeConfig(ProbeConfig{
		Enabled:             true,
		Interval:            30 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 1,
		Cooldown:            20 * time.Millisecond,
	})

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
		time.Sleep(15 * time.Millisecond)
	}
	statuses = p.GetPortStatuses()
	if len(statuses) != 1 || statuses[0].Up {
		t.Fatalf("expected TCP listener to be suspended (Up=false) when upstream is down, got %+v", statuses)
	}

	// Verify local port refuses TCP connections while suspended
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 100*time.Millisecond); err == nil {
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
		time.Sleep(15 * time.Millisecond)
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
	p.probeInitialDelay = 10 * time.Millisecond
	p.SetProbeConfig(ProbeConfig{
		Enabled:             true,
		Interval:            30 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 1,
		Cooldown:            20 * time.Millisecond,
	})

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

	// 2. Close backend UDP socket -> probe detects unreachable and suspends local UDP socket
	_ = backendConn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := p.GetPortStatuses()
		if len(st) == 1 && !st[0].Up {
			break
		}
		time.Sleep(15 * time.Millisecond)
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
		time.Sleep(15 * time.Millisecond)
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

func TestTrafficAwareProbeCooldown(t *testing.T) {
	info := &listenerInfo{
		probing: true,
		up:      true,
	}

	// 1. Without traffic, isTrafficActive is false
	if info.isTrafficActive(100 * time.Millisecond) {
		t.Errorf("expected isTrafficActive to be false before any traffic")
	}

	// 2. After recordActive, isTrafficActive is true within cooldown window
	info.recordActive()
	if !info.isTrafficActive(100 * time.Millisecond) {
		t.Errorf("expected isTrafficActive to be true immediately after recordActive")
	}

	// 3. When probing is disabled, recordActive is a no-op
	disabledInfo := &listenerInfo{probing: false, up: true}
	disabledInfo.recordActive()
	if disabledInfo.lastActiveNano.Load() != 0 {
		t.Errorf("expected recordActive to be a zero-overhead no-op when probing is disabled")
	}

	// 4. After cooldown window expires, isTrafficActive becomes false
	time.Sleep(30 * time.Millisecond)
	if info.isTrafficActive(20 * time.Millisecond) {
		t.Errorf("expected isTrafficActive to be false after cooldown elapsed")
	}
}

func TestConsecutiveFailuresThreshold(t *testing.T) {
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend TCP listener: %v", err)
	}
	backendAddr := backendListener.Addr().String()
	_ = backendListener.Close() // immediately closed

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
	p.probeInitialDelay = 10 * time.Millisecond
	p.SetProbeConfig(ProbeConfig{
		Enabled:             true,
		Interval:            25 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 3, // Requires 3 failures before suspending
		Cooldown:            20 * time.Millisecond,
	})

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

	// 2. Wait for 3 consecutive failures to trigger suspension
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := p.GetPortStatuses()
		if len(st) == 1 && !st[0].Up {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	statuses = p.GetPortStatuses()
	if len(statuses) != 1 || statuses[0].Up {
		t.Fatalf("expected TCP listener to be suspended after 3 consecutive failures, got %+v", statuses)
	}
}

func TestFastPathLiveDialFailure(t *testing.T) {
	// Destination that refuses connections
	freePort := getFreeTCPPort(t)
	destAddr := fmt.Sprintf("127.0.0.1:%d", freePort)

	proxyPort := getFreeTCPPort(t)
	ports := []PortMapping{
		{
			Source:            proxyPort,
			ListenAddress:     "127.0.0.1",
			Destination:       destAddr,
			Protocol:          "tcp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 2,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"test-tailnet": &localDialer{}}, 32*1024, 50)
	p.forceUpstreamProbe = true
	p.probeInitialDelay = 10 * time.Second
	p.SetProbeConfig(ProbeConfig{
		Enabled:             true,
		Interval:            10 * time.Second,
		Timeout:             time.Second,
		ConsecutiveFailures: 5,
		Cooldown:            time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(50 * time.Millisecond)

	// Initially UP
	st := p.GetPortStatuses()
	if len(st) != 1 || !st[0].Up {
		t.Fatalf("expected initially UP, got %+v", st)
	}

	// Real client dials proxy -> proxy fails to dial dest -> notifyUpstreamFailure fast-path instantly suspends
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), time.Second)
	if err == nil {
		_ = conn.Close()
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		statuses := p.GetPortStatuses()
		if len(statuses) == 1 && !statuses[0].Up {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	st = p.GetPortStatuses()
	if len(st) != 1 || st[0].Up {
		t.Fatalf("expected listener to be immediately suspended on live dial failure, got %+v", st)
	}
}

type countingMockPeerDialer struct {
	localDialer
	dials       atomic.Int64
	statusCalls atomic.Int64
	mu          sync.Mutex
	peers       map[string]bool
}

func (d *countingMockPeerDialer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.dials.Add(1)
	return d.localDialer.Dial(ctx, network, address)
}

func (d *countingMockPeerDialer) PeerOnlineMap(ctx context.Context) (map[string]bool, error) {
	d.statusCalls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.peers == nil {
		return nil, nil
	}
	cp := make(map[string]bool, len(d.peers))
	for k, v := range d.peers {
		cp[k] = v
	}
	return cp, nil
}

func TestActiveTrafficSkipsBackgroundProbes(t *testing.T) {
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

	dialer := &countingMockPeerDialer{}
	proxyPort := getFreeTCPPort(t)
	ports := []PortMapping{
		{
			Source:            proxyPort,
			ListenAddress:     "127.0.0.1",
			Destination:       backendListener.Addr().String(),
			Protocol:          "tcp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 5,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"test-tailnet": dialer}, 32*1024, 50)
	p.forceUpstreamProbe = true
	p.probeInitialDelay = 120 * time.Millisecond
	p.SetProbeConfig(ProbeConfig{
		Enabled:             true,
		Interval:            40 * time.Millisecond,
		Timeout:             100 * time.Millisecond,
		ConsecutiveFailures: 2,
		Cooldown:            500 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(30 * time.Millisecond)

	// Establish a single live client connection before the initial probe timer fires
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), time.Second)
	if err != nil {
		t.Fatalf("failed to connect to proxy: %v", err)
	}
	defer conn.Close()

	msg := []byte("keepalive-traffic")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	reply := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read failed: %v", err)
	}

	// Wait across multiple probe intervals while cooldown (500ms) is active
	time.Sleep(220 * time.Millisecond)

	if got := dialer.dials.Load(); got != 1 {
		t.Fatalf("expected active traffic cooldown to skip all background probe dials (want 1 dial total, got %d)", got)
	}
}

func TestPeerOfflineCacheSkipsNetworkDial(t *testing.T) {
	// 1. Verify buildPeerOnlineMap handles duplicate hostnames (stale offline node + active online node) without false-offline
	k1 := key.NewNode().Public()
	k2 := key.NewNode().Public()
	k3 := key.NewNode().Public()
	st := &ipnstate.Status{
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			k1: {
				HostName:     "api-server",
				DNSName:      "api-server-1.example.ts.net.",
				TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.10")},
				Online:       false,
			},
			k2: {
				HostName:     "api-server",
				DNSName:      "api-server.example.ts.net.",
				TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.11")},
				Online:       true,
			},
			k3: {
				HostName:     "dead-db",
				DNSName:      "dead-db.example.ts.net.",
				TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.99")},
				Online:       false,
			},
		},
	}
	peerMap := buildPeerOnlineMap(st)
	if !peerMap["api-server"] || !peerMap["api-server.example.ts.net"] || !peerMap["100.64.0.11"] {
		t.Fatalf("expected active duplicate-hostname peer to resolve as online, got %+v", peerMap)
	}
	if peerMap["dead-db"] || peerMap["dead-db.example.ts.net"] || peerMap["100.64.0.99"] {
		t.Fatalf("expected offline peer to resolve as offline, got %+v", peerMap)
	}

	// 2. Verify two ports sharing a tailnet reuse the 2s cached peer map and make 0 network Dial calls when peer is offline
	dialer := &countingMockPeerDialer{
		peers: map[string]bool{
			"100.64.0.99": false,
		},
	}
	port1 := getFreeTCPPort(t)
	port2 := getFreeUDPPort(t)
	ports := []PortMapping{
		{
			Source:            port1,
			ListenAddress:     "127.0.0.1",
			Destination:       "100.64.0.99:5432",
			Protocol:          "tcp",
			Tailnet:           "cached-tailnet",
			ConnectionTimeout: 2,
		},
		{
			Source:            port2,
			ListenAddress:     "127.0.0.1",
			Destination:       "100.64.0.99:5353",
			Protocol:          "udp",
			Tailnet:           "cached-tailnet",
			ConnectionTimeout: 2,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"cached-tailnet": dialer}, 32*1024, 50)
	p.probeInitialDelay = 10 * time.Millisecond
	p.SetProbeConfig(ProbeConfig{
		Enabled:             true,
		Interval:            30 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 1,
		Cooldown:            20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.IsReady() {
			statuses := p.GetPortStatuses()
			if len(statuses) == 2 && !statuses[0].Up && !statuses[1].Up &&
				statuses[0].LastError != "" && statuses[1].LastError != "" {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}

	statuses := p.GetPortStatuses()
	if len(statuses) != 2 || statuses[0].Up || statuses[1].Up {
		t.Fatalf("expected both TCP and UDP listeners to suspend on offline peer, got %+v", statuses)
	}
	if !strings.Contains(statuses[0].LastError, "is offline") {
		t.Errorf("expected offline peer error message, got %q", statuses[0].LastError)
	}
	if gotDials := dialer.dials.Load(); gotDials != 0 {
		t.Errorf("expected 0 network dials when peer cache reports offline, got %d", gotDials)
	}
	if gotStatusCalls := dialer.statusCalls.Load(); gotStatusCalls != 1 {
		t.Errorf("expected cached peer status map to be fetched once across both ports within TTL, got %d calls", gotStatusCalls)
	}
}

func TestUDPClientOnlyTrafficDoesNotSuppressProbe(t *testing.T) {
	// Closed UDP backend port
	closedUDP, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate UDP port: %v", err)
	}
	backendAddr := closedUDP.LocalAddr().String()
	_ = closedUDP.Close()

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
	p.probeInitialDelay = 40 * time.Millisecond
	p.SetProbeConfig(ProbeConfig{
		Enabled:             true,
		Interval:            35 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 2,
		Cooldown:            5 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(15 * time.Millisecond)

	// Simulate an ADC continuously sending outbound-only UDP health check packets to proxtail
	stopSender := make(chan struct{})
	defer close(stopSender)
	go func() {
		c, dialErr := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", proxyUDPPort))
		if dialErr != nil {
			return
		}
		defer c.Close()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSender:
				return
			case <-ticker.C:
				_, _ = c.Write([]byte("adc-udp-ping"))
			}
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.IsReady() {
			st := p.GetPortStatuses()
			if len(st) == 1 && !st[0].Up && st[0].LastError != "" {
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("expected UDP listener to suspend despite client-only outbound UDP traffic, got %+v", p.GetPortStatuses())
}

func TestHotReloadProbeEnabledToggle(t *testing.T) {
	freeDest := getFreeTCPPort(t)
	destAddr := fmt.Sprintf("127.0.0.1:%d", freeDest) // closed destination

	proxyPort := getFreeTCPPort(t)
	ports := []PortMapping{
		{
			Source:            proxyPort,
			ListenAddress:     "127.0.0.1",
			Destination:       destAddr,
			Protocol:          "tcp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 2,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"test-tailnet": &localDialer{}}, 32*1024, 50)
	p.forceUpstreamProbe = true
	p.probeInitialDelay = 10 * time.Millisecond
	// Start with probing disabled
	p.SetProbeConfig(ProbeConfig{
		Enabled:             false,
		Interval:            25 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 1,
		Cooldown:            20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	time.Sleep(60 * time.Millisecond)

	// With probing disabled, local listener stays bound even though destination is closed
	st := p.GetPortStatuses()
	if len(st) != 1 || !st[0].Up {
		t.Fatalf("expected listener to stay UP when probing is disabled, got %+v", st)
	}

	// Hot-reload to enable global probing (without changing port fields)
	p.SetProbeConfig(ProbeConfig{
		Enabled:             true,
		Interval:            25 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 1,
		Cooldown:            20 * time.Millisecond,
	})
	if err := p.ReloadConfig(ports); err != nil {
		t.Fatalf("ReloadConfig failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st = p.GetPortStatuses()
		if len(st) == 1 && !st[0].Up {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if len(st) != 1 || st[0].Up {
		t.Fatalf("expected hot-reloaded probe.enabled=true to suspend unreachable port, got %+v", st)
	}

	// Hot-reload back to probe.enabled=false -> listener re-binds and stays UP
	p.SetProbeConfig(ProbeConfig{
		Enabled:             false,
		Interval:            25 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 1,
		Cooldown:            20 * time.Millisecond,
	})
	if err := p.ReloadConfig(ports); err != nil {
		t.Fatalf("ReloadConfig back to disabled failed: %v", err)
	}

	time.Sleep(60 * time.Millisecond)
	st = p.GetPortStatuses()
	if len(st) != 1 || !st[0].Up {
		t.Fatalf("expected hot-reloaded probe.enabled=false to restore unprobed bound listener, got %+v", st)
	}
}

func TestTCPUnidirectionalStreamSharedConnectionTimeout(t *testing.T) {
	// Backend streams chunks to client for 1.6s while client->backend is completely idle,
	// then reads a final message from the client after ConnectionTimeout (1s) has elapsed.
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend listener: %v", err)
	}
	defer backendLn.Close()

	clientMsgCh := make(chan string, 1)
	go func() {
		conn, err := backendLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Stream 8 chunks every 200ms (total 1.6s > 1s ConnectionTimeout)
		for i := range 8 {
			time.Sleep(200 * time.Millisecond)
			if _, err := fmt.Fprintf(conn, "chunk-%d\n", i); err != nil {
				return
			}
		}

		buf := make([]byte, 64)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := conn.Read(buf)
		if err == nil && n > 0 {
			clientMsgCh <- string(buf[:n])
		}
	}()

	proxyPort := getFreeTCPPort(t)
	probeDisabled := false
	ports := []PortMapping{
		{
			Source:            proxyPort,
			ListenAddress:     "127.0.0.1",
			Destination:       backendLn.Addr().String(),
			Protocol:          "tcp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 1, // 1 second timeout
			Probe:             &probeDisabled,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"test-tailnet": &localDialer{}}, 32*1024, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	select {
	case <-p.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxy to start")
	}

	clientConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	defer clientConn.Close()

	// Read all 8 chunks over ~1.6s without sending anything on client->backend
	_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	received := 0
	buf := make([]byte, 256)
	for received < 8 {
		n, err := clientConn.Read(buf)
		if err != nil {
			t.Fatalf("client read failed after %d chunks: %v", received, err)
		}
		received += strings.Count(string(buf[:n]), "chunk-")
	}

	// Now send data on client->backend after 1.6s of client silence (exceeding 1s ConnectionTimeout).
	// Because backend->client was active, client->backend must NOT have timed out.
	if _, err := clientConn.Write([]byte("still-open")); err != nil {
		t.Fatalf("failed writing from client after unidirectional stream: %v", err)
	}

	select {
	case msg := <-clientMsgCh:
		if msg != "still-open" {
			t.Fatalf("expected backend to receive 'still-open', got %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backend to receive client message after unidirectional stream; client->backend was prematurely closed")
	}
}

func TestLiveProbeConfigParameterUpdate(t *testing.T) {
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend listener: %v", err)
	}
	defer backendLn.Close()

	go func() {
		for {
			c, err := backendLn.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	proxyPort := getFreeTCPPort(t)
	ports := []PortMapping{
		{
			Source:            proxyPort,
			ListenAddress:     "127.0.0.1",
			Destination:       backendLn.Addr().String(),
			Protocol:          "tcp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 2,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"test-tailnet": &localDialer{}}, 32*1024, 50)
	p.forceUpstreamProbe = true
	p.probeInitialDelay = 10 * time.Millisecond
	// Start with a very long interval (30s) so the monitor sleeps after the first probe
	p.SetProbeConfig(ProbeConfig{
		Enabled:             true,
		Interval:            30 * time.Second,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 1,
		Cooldown:            10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	select {
	case <-p.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxy to start")
	}

	// Wait for initial probe to complete and enter 30s sleep
	time.Sleep(50 * time.Millisecond)

	// Shut down backend and live-update ProbeConfig to a 20ms interval
	_ = backendLn.Close()
	p.SetProbeConfig(ProbeConfig{
		Enabled:             true,
		Interval:            20 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 1,
		Cooldown:            10 * time.Millisecond,
	})

	// Monitor should wake up immediately on probeWakeCh and suspend the listener well under 1s
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		st := p.GetPortStatuses()
		if len(st) == 1 && !st[0].Up {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("expected live SetProbeConfig update to wake monitor and suspend port within 500ms, got %+v", p.GetPortStatuses())
}

func TestReloadConfigPartialFailureAndRollback(t *testing.T) {
	port1 := getFreeTCPPort(t)
	occupiedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind occupied listener: %v", err)
	}
	defer occupiedLn.Close()
	occupiedPort := occupiedLn.Addr().(*net.TCPAddr).Port

	probeDisabled := false
	initialPorts := []PortMapping{
		{
			Source:            port1,
			ListenAddress:     "127.0.0.1",
			Destination:       "127.0.0.1:9999",
			Protocol:          "tcp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 2,
			Probe:             &probeDisabled,
		},
	}

	p := NewProxyWithDialers(initialPorts, map[string]Dialer{"test-tailnet": &localDialer{}}, 32*1024, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	select {
	case <-p.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxy to start")
	}

	if p.ActiveListenerCount() != 1 {
		t.Fatalf("expected 1 active listener initially, got %d", p.ActiveListenerCount())
	}

	// Attempt to reload to occupiedPort (which fails to bind)
	badPorts := []PortMapping{
		{
			Source:            occupiedPort,
			ListenAddress:     "127.0.0.1",
			Destination:       "127.0.0.1:9999",
			Protocol:          "tcp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 2,
			Probe:             &probeDisabled,
		},
	}
	if err := p.ReloadConfig(badPorts); err == nil {
		t.Fatal("expected ReloadConfig with occupied port to fail")
	}

	// Rolling back to initialPorts must re-bind port1
	if err := p.ReloadConfig(initialPorts); err != nil {
		t.Fatalf("rollback ReloadConfig(initialPorts) failed: %v", err)
	}
	if p.ActiveListenerCount() != 1 {
		t.Fatalf("expected 1 active listener after rollback, got %d", p.ActiveListenerCount())
	}
}

func TestUDPProbeModeNetmapSkipsZeroBytePayload(t *testing.T) {
	backendUDP, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend UDP listener: %v", err)
	}
	defer backendUDP.Close()

	var zeroBytePackets atomic.Int32
	go func() {
		buf := make([]byte, 64)
		for {
			n, _, err := backendUDP.ReadFrom(buf)
			if err != nil {
				return
			}
			if n == 0 {
				zeroBytePackets.Add(1)
			}
		}
	}()

	proxyUDPPort := getFreeUDPPort(t)
	ports := []PortMapping{
		{
			Source:            proxyUDPPort,
			ListenAddress:     "127.0.0.1",
			Destination:       backendUDP.LocalAddr().String(),
			Protocol:          "udp",
			Tailnet:           "test-tailnet",
			ConnectionTimeout: 2,
		},
	}

	p := NewProxyWithDialers(ports, map[string]Dialer{"test-tailnet": &localDialer{}}, 32*1024, 50)
	p.forceUpstreamProbe = true
	p.probeInitialDelay = 10 * time.Millisecond
	p.SetProbeConfig(ProbeConfig{
		Enabled:             true,
		Interval:            25 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		ConsecutiveFailures: 1,
		Cooldown:            10 * time.Millisecond,
		UDPProbeMode:        "netmap",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()
	defer p.Stop()

	select {
	case <-p.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for proxy to start")
	}

	// Let several probe intervals elapse
	time.Sleep(100 * time.Millisecond)

	st := p.GetPortStatuses()
	if len(st) != 1 || !st[0].Up {
		t.Fatalf("expected UDP listener to stay UP in netmap probe mode, got %+v", st)
	}
	if got := zeroBytePackets.Load(); got != 0 {
		t.Fatalf("expected 0 zero-byte UDP packets in netmap probe mode, got %d", got)
	}
}
