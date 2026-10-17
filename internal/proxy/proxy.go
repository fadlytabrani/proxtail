package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

// Dialer abstracts establishing outbound network connections over a tailnet.
// *tsnet.Server implements this interface directly.
type Dialer interface {
	Dial(ctx context.Context, network, address string) (net.Conn, error)
}

// listenerInfo holds listener details with proper lifecycle management.
type listenerInfo struct {
	listener net.Listener
	conn     net.PacketConn
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// udpSession represents a per-client-address UDP flow to a tailnet destination.
type udpSession struct {
	key        string
	clientAddr net.Addr
	destConn   net.Conn
	closeOnce  sync.Once
}

func (s *udpSession) close() {
	s.closeOnce.Do(func() {
		if s.destConn != nil {
			_ = s.destConn.Close()
		}
	})
}

// OptimizedBuffer provides efficient buffer management with pooling.
type OptimizedBuffer struct {
	data   []byte
	offset int
	length int
	pool   *sync.Pool
}

// NewOptimizedBuffer creates a new buffer with the specified capacity.
func NewOptimizedBuffer(capacity int, pool *sync.Pool) *OptimizedBuffer {
	return &OptimizedBuffer{
		data: make([]byte, capacity),
		pool: pool,
	}
}

// Write writes data to the buffer and returns the number of bytes written.
func (b *OptimizedBuffer) Write(p []byte) (int, error) {
	n := copy(b.data[b.length:], p)
	b.length += n
	return n, nil
}

// Read reads unread data from the buffer.
func (b *OptimizedBuffer) Read(p []byte) (int, error) {
	if b.offset >= b.length {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.offset:b.length])
	b.offset += n
	return n, nil
}

// WriteTo efficiently writes all unread buffer content to a writer, handling partial writes.
func (b *OptimizedBuffer) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for b.offset < b.length {
		n, err := w.Write(b.data[b.offset:b.length])
		if n > 0 {
			b.offset += n
			total += int64(n)
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

// ReadFrom efficiently reads a single chunk or datagram from a reader into the buffer.
func (b *OptimizedBuffer) ReadFrom(r io.Reader) (int64, error) {
	b.offset = 0
	n, err := r.Read(b.data)
	if n < 0 {
		n = 0
	}
	b.length = n
	return int64(n), err
}

// Bytes returns the unread portion of the buffer's data as a slice.
func (b *OptimizedBuffer) Bytes() []byte {
	return b.data[b.offset:b.length]
}

// Len returns the number of unread bytes in the buffer.
func (b *OptimizedBuffer) Len() int {
	return b.length - b.offset
}

// Reset resets the buffer to be empty.
func (b *OptimizedBuffer) Reset() {
	b.offset = 0
	b.length = 0
}

// Release returns the buffer to its pool for reuse.
func (b *OptimizedBuffer) Release() {
	b.Reset()
	if b.pool != nil {
		b.pool.Put(b)
	}
}

// PortMapping defines the configuration for a port to be proxied, including source port,
// destination address, protocol (TCP/UDP), target tailnet, and connection timeout.
type PortMapping struct {
	Source            int
	Destination       string
	Protocol          string
	Tailnet           string
	ConnectionTimeout int
}

// ProxyStats holds performance counters.
type ProxyStats struct {
	TCPConnections   int64 // Active TCP connections
	UDPPackets       int64 // Total UDP packets processed
	BytesTransferred int64 // Total bytes transferred
	ErrorCount       int64 // Total errors
}

// Proxy manages TCP and UDP listeners and forwards traffic across configured tailnets.
type Proxy struct {
	Ports          []PortMapping
	BufferSize     int
	QueueSize      int
	bufPool        *sync.Pool
	tailnetDialers map[string]Dialer

	// Performance optimization fields
	workerPool chan struct{}
	stats      *ProxyStats

	// Hot reload and lifecycle management
	mu           sync.RWMutex
	tcpListeners map[string]*listenerInfo
	udpConns     map[string]*listenerInfo
	ctx          context.Context
	cancel       context.CancelFunc
	stopOnce     sync.Once
}

// NewProxy creates a new Proxy instance with default worker pool sizing.
func NewProxy(ports []PortMapping, bufferSize int, queueSize int) *Proxy {
	return NewProxyWithWorkerPool(ports, bufferSize, queueSize, 0)
}

// NewProxyWithWorkerPool creates a new Proxy instance with custom worker pool size.
func NewProxyWithWorkerPool(ports []PortMapping, bufferSize int, queueSize int, workerPoolSize int) *Proxy {
	return NewProxyWithDialers(ports, nil, bufferSize, queueSize, workerPoolSize)
}

// NewProxyWithMultiTailnet creates a new Proxy instance backed by a map of *tsnet.Server instances.
func NewProxyWithMultiTailnet(ports []PortMapping, tailnetServers map[string]*tsnet.Server, bufferSize int, queueSize int, workerPoolSize int) *Proxy {
	dialers := make(map[string]Dialer, len(tailnetServers))
	for name, srv := range tailnetServers {
		if srv != nil {
			dialers[name] = srv
		}
	}
	return NewProxyWithDialers(ports, dialers, bufferSize, queueSize, workerPoolSize)
}

// NewProxyWithDialers creates a new Proxy instance with custom Dialer implementations per tailnet.
func NewProxyWithDialers(ports []PortMapping, tailnetDialers map[string]Dialer, bufferSize int, queueSize int, workerPoolSize int) *Proxy {
	if bufferSize <= 0 {
		bufferSize = 512 * 1024
	}
	if queueSize <= 0 {
		queueSize = 100
	}
	if workerPoolSize <= 0 {
		workerPoolSize = queueSize * 2
		if workerPoolSize < 50 {
			workerPoolSize = 50
		}
		if workerPoolSize > 1000 {
			workerPoolSize = 1000
		}
	}

	if tailnetDialers == nil {
		tailnetDialers = make(map[string]Dialer)
	}

	bufPool := &sync.Pool{
		New: func() interface{} {
			return NewOptimizedBuffer(bufferSize, nil)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Proxy{
		Ports:          ports,
		BufferSize:     bufferSize,
		QueueSize:      queueSize,
		bufPool:        bufPool,
		tailnetDialers: tailnetDialers,
		workerPool:     make(chan struct{}, workerPoolSize),
		stats:          &ProxyStats{},
		tcpListeners:   make(map[string]*listenerInfo),
		udpConns:       make(map[string]*listenerInfo),
		ctx:            ctx,
		cancel:         cancel,
	}
}

// UpdateTailnetServers updates the active tailnet servers map during a configuration reload.
func (p *Proxy) UpdateTailnetServers(tailnetServers map[string]*tsnet.Server) {
	dialers := make(map[string]Dialer, len(tailnetServers))
	for name, srv := range tailnetServers {
		if srv != nil {
			dialers[name] = srv
		}
	}
	p.UpdateTailnetDialers(dialers)
}

// UpdateTailnetDialers updates the active tailnet dialers map during a configuration reload.
func (p *Proxy) UpdateTailnetDialers(dialers map[string]Dialer) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cp := make(map[string]Dialer, len(dialers))
	for name, d := range dialers {
		if d != nil {
			cp[name] = d
		}
	}
	p.tailnetDialers = cp
}

// optimizeTCPConn applies TCP socket optimizations (TCP_NODELAY and 30s keepalive).
func optimizeTCPConn(conn net.Conn) error {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(true); err != nil {
			return fmt.Errorf("failed to set TCP_NODELAY: %w", err)
		}
		if err := tcpConn.SetKeepAlive(true); err != nil {
			return fmt.Errorf("failed to set TCP keepalive: %w", err)
		}
		if err := tcpConn.SetKeepAlivePeriod(30 * time.Second); err != nil {
			return fmt.Errorf("failed to set TCP keepalive period: %w", err)
		}
	}
	return nil
}

// isConnectionClosed checks if an error represents a normal connection closure or idle timeout.
func isConnectionClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed) {
		return true
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}

	errStr := err.Error()
	return strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "endpoint is closed for send") ||
		strings.Contains(errStr, "endpoint is closed for receive") ||
		strings.Contains(errStr, "connection closed") ||
		strings.Contains(errStr, "network is unreachable") ||
		strings.Contains(errStr, "no route to host")
}

func (p *Proxy) acquireWorker() {
	p.workerPool <- struct{}{}
}

func (p *Proxy) releaseWorker() {
	<-p.workerPool
}

// Start initializes and starts all configured TCP and UDP listeners, then blocks until stopped.
// If any listener fails to bind during startup, all previously started listeners are rolled back.
func (p *Proxy) Start(ctx context.Context) error {
	p.mu.Lock()
	for _, port := range p.Ports {
		key := fmt.Sprintf("%d-%s", port.Source, strings.ToLower(port.Protocol))
		switch strings.ToLower(port.Protocol) {
		case "tcp":
			if err := p.startTCPListener(port, key); err != nil {
				p.stopAllListenersLocked()
				p.mu.Unlock()
				log.Printf("Error starting TCP listener on port %d: %v", port.Source, err)
				return err
			}
		case "udp":
			if err := p.startUDPListener(port, key); err != nil {
				p.stopAllListenersLocked()
				p.mu.Unlock()
				log.Printf("Error starting UDP listener on port %d: %v", port.Source, err)
				return err
			}
		default:
			p.stopAllListenersLocked()
			p.mu.Unlock()
			return fmt.Errorf("unsupported protocol for port %d: %s", port.Source, port.Protocol)
		}
	}
	p.mu.Unlock()

	select {
	case <-ctx.Done():
	case <-p.ctx.Done():
	}

	log.Println("Shutting down proxy listeners...")
	p.Stop()
	return nil
}

// forwardTCP accepts incoming TCP connections and dispatches them to the worker pool.
func (p *Proxy) forwardTCP(ctx context.Context, dialer Dialer, port PortMapping, listener net.Listener, connWg *sync.WaitGroup) {
	timeoutSec := port.ConnectionTimeout
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	log.Printf("TCP listening on %d and forwarding to %s (tailnet: %s) with connection timeout %ds",
		port.Source, port.Destination, port.Tailnet, timeoutSec)

	connChan := make(chan net.Conn, p.QueueSize)

	go func() {
		defer close(connChan)
		for {
			select {
			case <-ctx.Done():
				log.Printf("TCP accept loop stopping for port %d due to context cancellation", port.Source)
				return
			default:
				if tcpListener, ok := listener.(*net.TCPListener); ok {
					_ = tcpListener.SetDeadline(time.Now().Add(100 * time.Millisecond))
				}

				conn, err := listener.Accept()
				if err != nil {
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					}
					if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection") {
						log.Printf("TCP listener closed for port %d, exiting accept loop", port.Source)
						return
					}
					log.Printf("TCP accept error on port %d: %v", port.Source, err)
					continue
				}

				select {
				case connChan <- conn:
				case <-ctx.Done():
					_ = conn.Close()
					return
				default:
					log.Printf("TCP queue full on port %d, dropping connection", port.Source)
					_ = conn.Close()
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			log.Printf("Stopping TCP listener on port %d", port.Source)
			for c := range connChan {
				_ = c.Close()
			}
			return
		case conn, ok := <-connChan:
			if !ok {
				log.Printf("TCP accept loop exited for port %d", port.Source)
				return
			}
			connWg.Add(1)
			go func(c net.Conn) {
				defer connWg.Done()
				p.acquireWorker()
				defer p.releaseWorker()
				p.handleTCPConnection(ctx, dialer, port, c)
			}(conn)
		}
	}
}

// handleTCPConnection dials the destination over Tailscale and bridges both streams.
func (p *Proxy) handleTCPConnection(ctx context.Context, dialer Dialer, port PortMapping, conn net.Conn) {
	atomic.AddInt64(&p.stats.TCPConnections, 1)
	defer atomic.AddInt64(&p.stats.TCPConnections, -1)
	defer conn.Close()

	if err := optimizeTCPConn(conn); err != nil {
		atomic.AddInt64(&p.stats.ErrorCount, 1)
		log.Printf("TCP optimization error for port %d: %v", port.Source, err)
	}

	timeoutSec := port.ConnectionTimeout
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	timeout := time.Duration(timeoutSec) * time.Second

	dialCtx, dialCancel := context.WithTimeout(ctx, timeout)
	destConn, err := dialer.Dial(dialCtx, "tcp", port.Destination)
	dialCancel()
	if err != nil {
		atomic.AddInt64(&p.stats.ErrorCount, 1)
		log.Printf("TCP dial error to %s (tailnet %s): %v", port.Destination, port.Tailnet, err)
		return
	}
	defer destConn.Close()

	if err := optimizeTCPConn(destConn); err != nil {
		log.Printf("TCP optimization error for destination %s: %v", port.Destination, err)
	}

	connDone := make(chan struct{})
	defer close(connDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
			_ = destConn.Close()
		case <-connDone:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		p.forwardStream(conn, destConn, port.Source, timeout)
		if cw, ok := destConn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		p.forwardStream(destConn, conn, port.Source, timeout)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	wg.Wait()
}

// forwardUDP reads UDP datagrams and dispatches them to the worker pool using a per-client session table.
func (p *Proxy) forwardUDP(ctx context.Context, dialer Dialer, port PortMapping, conn net.PacketConn, sessionWg *sync.WaitGroup) {
	timeoutSec := port.ConnectionTimeout
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	log.Printf("UDP listening on %d and forwarding to %s (tailnet: %s) with connection timeout %ds",
		port.Source, port.Destination, port.Tailnet, timeoutSec)

	var sessions sync.Map // map[string]*udpSession
	defer func() {
		sessions.Range(func(key, value interface{}) bool {
			if sess, ok := value.(*udpSession); ok {
				sess.close()
			}
			sessions.Delete(key)
			return true
		})
	}()

	type udpPacket struct {
		buf  *OptimizedBuffer
		addr net.Addr
	}
	packetChan := make(chan udpPacket, p.QueueSize)

	go func() {
		defer close(packetChan)
		for {
			select {
			case <-ctx.Done():
				log.Printf("UDP read loop stopping for port %d due to context cancellation", port.Source)
				return
			default:
				_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

				buf := p.bufPool.Get().(*OptimizedBuffer)
				buf.pool = p.bufPool

				n, addr, err := conn.ReadFrom(buf.data)
				if n > 0 {
					buf.offset = 0
					buf.length = n
				}

				if err != nil {
					buf.Release()
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					}
					if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection") {
						log.Printf("UDP connection closed for port %d, exiting read loop", port.Source)
						return
					}
					log.Printf("UDP read error on port %d: %v", port.Source, err)
					continue
				}

				if n > 0 {
					select {
					case packetChan <- udpPacket{buf, addr}:
					case <-ctx.Done():
						buf.Release()
						return
					default:
						log.Printf("UDP queue full on port %d, dropping packet", port.Source)
						buf.Release()
					}
				} else {
					buf.Release()
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			log.Printf("Stopping UDP listener on port %d", port.Source)
			for pkt := range packetChan {
				pkt.buf.Release()
			}
			return
		case pkt, ok := <-packetChan:
			if !ok {
				log.Printf("UDP read loop exited for port %d", port.Source)
				return
			}
			sessionWg.Add(1)
			go func(buf *OptimizedBuffer, addr net.Addr) {
				defer sessionWg.Done()
				p.acquireWorker()
				defer p.releaseWorker()
				p.handleUDPPacketOptimized(ctx, dialer, port, conn, buf, addr, &sessions, sessionWg)
			}(pkt.buf, pkt.addr)
		}
	}
}

// handleUDPPacketOptimized forwards a UDP datagram to the destination using a dedicated per-client session.
func (p *Proxy) handleUDPPacketOptimized(
	ctx context.Context,
	dialer Dialer,
	port PortMapping,
	conn net.PacketConn,
	buf *OptimizedBuffer,
	addr net.Addr,
	sessions *sync.Map,
	sessionWg *sync.WaitGroup,
) {
	defer buf.Release()

	atomic.AddInt64(&p.stats.UDPPackets, 1)

	timeoutSec := port.ConnectionTimeout
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	timeout := time.Duration(timeoutSec) * time.Second

	clientKey := addr.String()
	var sess *udpSession

	if existing, ok := sessions.Load(clientKey); ok {
		sess = existing.(*udpSession)
	} else {
		dialCtx, dialCancel := context.WithTimeout(ctx, timeout)
		destConn, err := dialer.Dial(dialCtx, "udp", port.Destination)
		dialCancel()
		if err != nil {
			atomic.AddInt64(&p.stats.ErrorCount, 1)
			log.Printf("UDP dial error to %s (tailnet %s): %v", port.Destination, port.Tailnet, err)
			return
		}

		newSess := &udpSession{
			key:        clientKey,
			clientAddr: addr,
			destConn:   destConn,
		}

		actual, loaded := sessions.LoadOrStore(clientKey, newSess)
		if loaded {
			newSess.close()
			sess = actual.(*udpSession)
		} else {
			sess = newSess
			sessionWg.Add(1)
			go func() {
				defer sessionWg.Done()
				p.readUDPBackendSession(ctx, port, conn, sess, sessions, timeout)
			}()
		}
	}

	now := time.Now()
	_ = sess.destConn.SetWriteDeadline(now.Add(timeout))
	_ = sess.destConn.SetReadDeadline(now.Add(timeout))

	bytesWritten, err := buf.WriteTo(sess.destConn)
	if bytesWritten > 0 {
		atomic.AddInt64(&p.stats.BytesTransferred, bytesWritten)
	}
	if err != nil {
		sessions.CompareAndDelete(clientKey, sess)
		sess.close()
		if !isConnectionClosed(err) {
			atomic.AddInt64(&p.stats.ErrorCount, 1)
			log.Printf("UDP write to destination error on port %d: %v", port.Source, err)
		}
		return
	}
}

// readUDPBackendSession continuously reads backend UDP responses for a specific client session
// and forwards them back to the client until the session times out or the listener stops.
func (p *Proxy) readUDPBackendSession(
	ctx context.Context,
	port PortMapping,
	conn net.PacketConn,
	sess *udpSession,
	sessions *sync.Map,
	timeout time.Duration,
) {
	defer func() {
		sessions.CompareAndDelete(sess.key, sess)
		sess.close()
	}()

	respBuffer := p.bufPool.Get().(*OptimizedBuffer)
	respBuffer.pool = p.bufPool
	defer respBuffer.Release()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = sess.destConn.SetReadDeadline(time.Now().Add(timeout))
		bytesRead, err := respBuffer.ReadFrom(sess.destConn)
		if bytesRead > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(timeout))
			written, writeErr := conn.WriteTo(respBuffer.Bytes(), sess.clientAddr)
			if written > 0 {
				atomic.AddInt64(&p.stats.BytesTransferred, int64(written))
			}
			respBuffer.Reset()
			if writeErr != nil {
				if !isConnectionClosed(writeErr) {
					atomic.AddInt64(&p.stats.ErrorCount, 1)
					log.Printf("UDP write back to source error on port %d: %v", port.Source, writeErr)
				}
				return
			}
		}

		if err != nil {
			if !isConnectionClosed(err) {
				atomic.AddInt64(&p.stats.ErrorCount, 1)
				log.Printf("UDP read from destination error on port %d: %v", port.Source, err)
			}
			return
		}
	}
}

// forwardStream transfers data from src to dst using pooled OptimizedBuffers,
// refreshing idle deadlines on each read/write and updating BytesTransferred in real time.
func (p *Proxy) forwardStream(src, dst net.Conn, port int, timeout time.Duration) {
	p.forwardStreamBuffered(src, dst, port, timeout)
}

func (p *Proxy) forwardStreamBuffered(src, dst net.Conn, port int, timeout time.Duration) {
	buf := p.bufPool.Get().(*OptimizedBuffer)
	buf.pool = p.bufPool
	defer buf.Release()

	for {
		_ = src.SetReadDeadline(time.Now().Add(timeout))
		bytesRead, err := buf.ReadFrom(src)

		if bytesRead > 0 {
			_ = dst.SetWriteDeadline(time.Now().Add(timeout))

			bytesWritten, writeErr := buf.WriteTo(dst)
			if bytesWritten > 0 {
				atomic.AddInt64(&p.stats.BytesTransferred, bytesWritten)
			}
			if writeErr != nil {
				if !isConnectionClosed(writeErr) {
					atomic.AddInt64(&p.stats.ErrorCount, 1)
					log.Printf("Stream write error on port %d: %v", port, writeErr)
				}
				return
			}

			buf.Reset()
		}

		if err != nil {
			if !isConnectionClosed(err) {
				atomic.AddInt64(&p.stats.ErrorCount, 1)
				log.Printf("Stream read error on port %d: %v", port, err)
			}
			break
		}
	}
}

// GetStats returns a snapshot of the proxy's performance statistics.
func (p *Proxy) GetStats() ProxyStats {
	return ProxyStats{
		TCPConnections:   atomic.LoadInt64(&p.stats.TCPConnections),
		UDPPackets:       atomic.LoadInt64(&p.stats.UDPPackets),
		BytesTransferred: atomic.LoadInt64(&p.stats.BytesTransferred),
		ErrorCount:       atomic.LoadInt64(&p.stats.ErrorCount),
	}
}

// ReloadConfig updates the proxy port mappings at runtime without dropping unaffected active connections.
func (p *Proxy) ReloadConfig(newPorts []PortMapping) error {
	if len(newPorts) == 0 {
		return fmt.Errorf("no ports configured in new configuration")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	oldPortsMap := make(map[string]PortMapping)
	newPortsMap := make(map[string]PortMapping)

	for _, port := range p.Ports {
		key := fmt.Sprintf("%d-%s", port.Source, strings.ToLower(port.Protocol))
		oldPortsMap[key] = port
	}

	for _, port := range newPorts {
		key := fmt.Sprintf("%d-%s", port.Source, strings.ToLower(port.Protocol))
		newPortsMap[key] = port
	}

	portsToAdd := make([]PortMapping, 0)
	portsToRemove := make([]PortMapping, 0)
	portsToModify := make([]PortMapping, 0)

	for key, newPort := range newPortsMap {
		if oldPort, exists := oldPortsMap[key]; exists {
			if !portsEqual(oldPort, newPort) {
				portsToModify = append(portsToModify, newPort)
			}
		} else {
			portsToAdd = append(portsToAdd, newPort)
		}
	}

	for key, oldPort := range oldPortsMap {
		if _, exists := newPortsMap[key]; !exists {
			portsToRemove = append(portsToRemove, oldPort)
		}
	}

	var reloadErrors []string

	for _, port := range portsToRemove {
		if err := p.stopListener(port); err != nil {
			reloadErrors = append(reloadErrors, fmt.Sprintf("failed to stop port %d (%s): %v", port.Source, port.Protocol, err))
		}
	}

	for _, port := range portsToModify {
		if err := p.stopListener(port); err != nil {
			reloadErrors = append(reloadErrors, fmt.Sprintf("failed to stop port %d (%s): %v", port.Source, port.Protocol, err))
		}
	}

	for _, port := range portsToAdd {
		if err := p.startListener(port); err != nil {
			reloadErrors = append(reloadErrors, fmt.Sprintf("failed to start port %d (%s): %v", port.Source, port.Protocol, err))
		}
	}

	for _, port := range portsToModify {
		if err := p.startListener(port); err != nil {
			reloadErrors = append(reloadErrors, fmt.Sprintf("failed to start port %d (%s): %v", port.Source, port.Protocol, err))
		}
	}

	p.Ports = newPorts

	if len(reloadErrors) > 0 {
		log.Printf("Configuration reloaded with %d errors: %s", len(reloadErrors), strings.Join(reloadErrors, "; "))
		return fmt.Errorf("configuration reload completed with %d errors", len(reloadErrors))
	}

	log.Printf("Configuration reloaded successfully: %d ports added, %d removed, %d modified",
		len(portsToAdd), len(portsToRemove), len(portsToModify))

	return nil
}

// StopTailnetPorts stops and restarts any active port listeners bound to the specified tailnets
// so they pick up newly replaced tailnet dialers.
func (p *Proxy) RestartTailnetPorts(tailnetNames map[string]bool) error {
	if len(tailnetNames) == 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	var errs []string
	for _, port := range p.Ports {
		if !tailnetNames[port.Tailnet] {
			continue
		}
		if err := p.stopListener(port); err != nil {
			errs = append(errs, err.Error())
		}
		if err := p.startListener(port); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("restarting ports for updated tailnets: %s", strings.Join(errs, "; "))
	}
	return nil
}

func portsEqual(a, b PortMapping) bool {
	return a.Source == b.Source &&
		a.Destination == b.Destination &&
		a.Tailnet == b.Tailnet &&
		strings.EqualFold(a.Protocol, b.Protocol) &&
		a.ConnectionTimeout == b.ConnectionTimeout
}

func (p *Proxy) stopListener(port PortMapping) error {
	key := fmt.Sprintf("%d-%s", port.Source, strings.ToLower(port.Protocol))

	if listener, exists := p.tcpListeners[key]; exists {
		listener.cancel()
		_ = listener.listener.Close()
		listener.wg.Wait()
		delete(p.tcpListeners, key)
		log.Printf("TCP listener stopped for port %d", port.Source)
	}

	if conn, exists := p.udpConns[key]; exists {
		conn.cancel()
		_ = conn.conn.Close()
		conn.wg.Wait()
		delete(p.udpConns, key)
		log.Printf("UDP listener stopped for port %d", port.Source)
	}

	return nil
}

func (p *Proxy) startListener(port PortMapping) error {
	key := fmt.Sprintf("%d-%s", port.Source, strings.ToLower(port.Protocol))

	switch strings.ToLower(port.Protocol) {
	case "tcp":
		return p.startTCPListener(port, key)
	case "udp":
		return p.startUDPListener(port, key)
	default:
		return fmt.Errorf("unsupported protocol: %s", port.Protocol)
	}
}

func (p *Proxy) startTCPListener(port PortMapping, key string) error {
	dialer, exists := p.tailnetDialers[port.Tailnet]
	if !exists {
		return fmt.Errorf("tailnet server '%s' not found for port %d", port.Tailnet, port.Source)
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port.Source))
	if err != nil {
		return fmt.Errorf("failed to start TCP listener on port %d: %w", port.Source, err)
	}

	ctx, cancel := context.WithCancel(p.ctx)
	info := &listenerInfo{
		listener: listener,
		ctx:      ctx,
		cancel:   cancel,
	}

	p.tcpListeners[key] = info

	info.wg.Add(1)
	go func() {
		defer info.wg.Done()
		p.forwardTCP(ctx, dialer, port, listener, &info.wg)
	}()

	log.Printf("TCP listener started on port %d for tailnet %s", port.Source, port.Tailnet)
	return nil
}

func (p *Proxy) startUDPListener(port PortMapping, key string) error {
	dialer, exists := p.tailnetDialers[port.Tailnet]
	if !exists {
		return fmt.Errorf("tailnet server '%s' not found for port %d", port.Tailnet, port.Source)
	}

	conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port.Source))
	if err != nil {
		return fmt.Errorf("failed to start UDP listener on port %d: %w", port.Source, err)
	}

	ctx, cancel := context.WithCancel(p.ctx)
	info := &listenerInfo{
		conn:   conn,
		ctx:    ctx,
		cancel: cancel,
	}

	p.udpConns[key] = info

	info.wg.Add(1)
	go func() {
		defer info.wg.Done()
		p.forwardUDP(ctx, dialer, port, conn, &info.wg)
	}()

	log.Printf("UDP listener started on port %d for tailnet %s", port.Source, port.Tailnet)
	return nil
}

func (p *Proxy) stopAllListenersLocked() {
	for key, listener := range p.tcpListeners {
		listener.cancel()
		if err := listener.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("Error closing TCP listener %s: %v", key, err)
		}
		listener.wg.Wait()
		delete(p.tcpListeners, key)
	}

	for key, conn := range p.udpConns {
		conn.cancel()
		if err := conn.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("Error closing UDP connection %s: %v", key, err)
		}
		conn.wg.Wait()
		delete(p.udpConns, key)
	}
}

// Stop gracefully stops the proxy and all active TCP/UDP listeners.
// Calling Stop multiple times is safe and idempotent.
func (p *Proxy) Stop() {
	p.stopOnce.Do(func() {
		p.cancel()

		p.mu.Lock()
		p.stopAllListenersLocked()
		p.mu.Unlock()

		log.Printf("All listeners stopped and goroutines cleaned up")
	})
}
