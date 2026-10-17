package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

type localDialer struct{}

func (d *localDialer) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, network, address)
}

func getFreeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free TCP port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func getFreeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
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

	p := NewProxyWithDialers(initialPorts, dialers, 64*1024, 50, 100)
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

	p := NewProxyWithDialers(ports, dialers, 64*1024, 50, 100)
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

	p := NewProxyWithDialers(ports, dialers, 64*1024, 50, 100)
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

	p := NewProxyWithDialers(ports, dialers, 64*1024, 50, 100)
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
	p := NewProxyWithDialers(ports, dialers, 16*1024, 50, 100)
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
	p := NewProxyWithDialers(initialPorts, dialers, 32*1024, 50, 100)

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

	p := NewProxyWithDialers(ports, map[string]Dialer{"t1": &localDialer{}}, 16*1024, 50, 100)
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

	p := NewProxyWithDialers(ports, map[string]Dialer{"bench": &localDialer{}}, 256*1024, 200, 400)
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

	p := NewProxyWithDialers(ports, map[string]Dialer{"bench": &localDialer{}}, 256*1024, 500, 1000)
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
