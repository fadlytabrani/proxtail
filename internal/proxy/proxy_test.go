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
	if stats.UDPPackets < 1 {
		t.Errorf("expected UDPPackets >= 1, got %d", stats.UDPPackets)
	}
}
