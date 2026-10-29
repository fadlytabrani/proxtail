package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

const maxUDPBufferSize = 64 * 1024

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

// udpSessionKey identifies a client UDP endpoint without heap allocations when using UDPAddrPort.
type udpSessionKey struct {
	addrPort netip.AddrPort
	fallback string
}

// udpSession represents a per-client-address UDP flow to a tailnet destination.
type udpSession struct {
	key              udpSessionKey
	addrPort         netip.AddrPort
	hasAddrPort      bool
	clientAddr       net.Addr
	destConn         net.Conn
	ready            chan struct{}
	readyOK          atomic.Bool
	failed           atomic.Bool
	lastDeadlineNano atomic.Int64
	closeOnce        sync.Once
}

func (s *udpSession) close() {
	s.closeOnce.Do(func() {
		if s.destConn != nil {
			_ = s.destConn.Close()
		}
	})
}

func (s *udpSession) refreshDeadlinesIfNeeded(timeout, refreshStep time.Duration) {
	now := time.Now()
	nowNano := now.UnixNano()
	last := s.lastDeadlineNano.Load()
	if nowNano-last >= int64(refreshStep) {
		if s.lastDeadlineNano.CompareAndSwap(last, nowNano) {
			deadline := now.Add(timeout)
			_ = s.destConn.SetWriteDeadline(deadline)
			_ = s.destConn.SetReadDeadline(deadline)
		}
	}
}

const udpSessionShardCount = 16

type udpSessionShard struct {
	mu       sync.RWMutex
	sessions map[udpSessionKey]*udpSession
	_        [32]byte // Pad shard to prevent false sharing across CPU cores
}

type udpSessionTable struct {
	shards                    [udpSessionShardCount]udpSessionShard
	lastConnWriteDeadlineNano atomic.Int64
	_                         [56]byte
}

func newUDPSessionTable() *udpSessionTable {
	t := &udpSessionTable{}
	for i := range t.shards {
		t.shards[i].sessions = make(map[udpSessionKey]*udpSession, 16)
	}
	return t
}

func (t *udpSessionTable) refreshConnWriteDeadline(conn net.PacketConn, timeout, refreshStep time.Duration) {
	nowNano := time.Now().UnixNano()
	last := t.lastConnWriteDeadlineNano.Load()
	if nowNano-last >= int64(refreshStep) {
		if t.lastConnWriteDeadlineNano.CompareAndSwap(last, nowNano) {
			_ = conn.SetWriteDeadline(time.Unix(0, nowNano).Add(timeout))
		}
	}
}

func (t *udpSessionTable) shardIndexFor(key udpSessionKey) uint32 {
	if key.addrPort.IsValid() {
		p := uint32(key.addrPort.Port())
		b := key.addrPort.Addr().As16()
		h := (p * 2654435761) ^ uint32(b[15]) ^ (uint32(b[14]) << 8)
		return h & (udpSessionShardCount - 1)
	}
	var h uint32 = 2166136261
	for i := 0; i < len(key.fallback); i++ {
		h ^= uint32(key.fallback[i])
		h *= 16777619
	}
	return h & (udpSessionShardCount - 1)
}

func (t *udpSessionTable) shardFor(key udpSessionKey) *udpSessionShard {
	return &t.shards[t.shardIndexFor(key)]
}

func (t *udpSessionTable) get(key udpSessionKey) *udpSession {
	sh := t.shardFor(key)
	sh.mu.RLock()
	s := sh.sessions[key]
	sh.mu.RUnlock()
	return s
}

func (t *udpSessionTable) getOrCreate(key udpSessionKey, addrPort netip.AddrPort, hasAddrPort bool, clientAddr net.Addr) (*udpSession, bool) {
	sh := t.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if existing, ok := sh.sessions[key]; ok {
		return existing, false
	}
	sess := &udpSession{
		key:         key,
		addrPort:    addrPort,
		hasAddrPort: hasAddrPort,
		clientAddr:  clientAddr,
		ready:       make(chan struct{}),
	}
	sh.sessions[key] = sess
	return sess, true
}

func (t *udpSessionTable) remove(key udpSessionKey, target *udpSession) {
	sh := t.shardFor(key)
	sh.mu.Lock()
	if current, ok := sh.sessions[key]; ok && current == target {
		delete(sh.sessions, key)
	}
	sh.mu.Unlock()
}

func (t *udpSessionTable) closeAll() {
	var all []*udpSession
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for k, s := range sh.sessions {
			all = append(all, s)
			delete(sh.sessions, k)
		}
		sh.mu.Unlock()
	}

	for _, s := range all {
		s.close()
	}
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
// optional local bind address, destination address, protocol (TCP/UDP), target tailnet, and connection timeout.
type PortMapping struct {
	Source            int
	ListenAddress     string
	Destination       string
	Protocol          string
	Tailnet           string
	ConnectionTimeout int
}

// ListenAddr returns the host:port string to bind on locally.
func (pm PortMapping) ListenAddr() string {
	addr := strings.TrimSpace(pm.ListenAddress)
	if addr == "" {
		return fmt.Sprintf(":%d", pm.Source)
	}
	return net.JoinHostPort(addr, strconv.Itoa(pm.Source))
}

// ProxyStats holds a snapshot of performance counters.
type ProxyStats struct {
	TCPConnections   int64 // Active TCP connections
	UDPPackets       int64 // Total UDP packets processed
	BytesTransferred int64 // Total bytes transferred
	ErrorCount       int64 // Total errors
}

const statsShardCount = 16

type paddedAtomicInt64 struct {
	val atomic.Int64
	_   [56]byte
}

// proxyStatsCounters pads and stripes hot atomic counters across separate 64-byte cache lines
// to eliminate cross-core false sharing and atomic contention under high concurrency.
type proxyStatsCounters struct {
	tcpConnections   atomic.Int64
	_                [56]byte
	errorCount       atomic.Int64
	_                [56]byte
	nextShard        atomic.Uint32
	_                [60]byte
	udpPackets       [statsShardCount]paddedAtomicInt64
	bytesTransferred [statsShardCount]paddedAtomicInt64
}

// Proxy manages TCP and UDP listeners and forwards traffic across configured tailnets.
type Proxy struct {
	Ports          []PortMapping
	BufferSize     int
	bufPool        *sync.Pool
	udpBufPool     *sync.Pool
	tailnetDialers map[string]Dialer

	// Performance optimization fields
	workerPool chan struct{}
	stats      proxyStatsCounters

	// Hot reload and lifecycle management
	mu           sync.RWMutex
	tcpListeners map[string]*listenerInfo
	udpConns     map[string]*listenerInfo
	ctx          context.Context
	cancel       context.CancelFunc
	stopOnce     sync.Once
}

// NewProxy creates a new Proxy instance with default worker pool sizing.
func NewProxy(ports []PortMapping, bufferSize int) *Proxy {
	return NewProxyWithWorkerPool(ports, bufferSize, 0)
}

// NewProxyWithWorkerPool creates a new Proxy instance with custom worker pool size.
func NewProxyWithWorkerPool(ports []PortMapping, bufferSize int, workerPoolSize int) *Proxy {
	return NewProxyWithDialers(ports, nil, bufferSize, workerPoolSize)
}

// NewProxyWithMultiTailnet creates a new Proxy instance backed by a map of *tsnet.Server instances.
func NewProxyWithMultiTailnet(ports []PortMapping, tailnetServers map[string]*tsnet.Server, bufferSize int, workerPoolSize int) *Proxy {
	dialers := make(map[string]Dialer, len(tailnetServers))
	for name, srv := range tailnetServers {
		if srv != nil {
			dialers[name] = srv
		}
	}
	return NewProxyWithDialers(ports, dialers, bufferSize, workerPoolSize)
}

// NewProxyWithDialers creates a new Proxy instance with custom Dialer implementations per tailnet.
func NewProxyWithDialers(ports []PortMapping, tailnetDialers map[string]Dialer, bufferSize int, workerPoolSize int) *Proxy {
	if bufferSize <= 0 {
		bufferSize = 512 * 1024
	}
	if workerPoolSize <= 0 {
		workerPoolSize = 200
	}

	if tailnetDialers == nil {
		tailnetDialers = make(map[string]Dialer)
	}

	var bufPool *sync.Pool
	bufPool = &sync.Pool{
		New: func() interface{} {
			return NewOptimizedBuffer(bufferSize, bufPool)
		},
	}

	udpBufSize := bufferSize
	if udpBufSize > maxUDPBufferSize {
		udpBufSize = maxUDPBufferSize
	}
	var udpBufPool *sync.Pool
	udpBufPool = &sync.Pool{
		New: func() interface{} {
			return NewOptimizedBuffer(udpBufSize, udpBufPool)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Proxy{
		Ports:          ports,
		BufferSize:     bufferSize,
		bufPool:        bufPool,
		udpBufPool:     udpBufPool,
		tailnetDialers: tailnetDialers,
		workerPool:     make(chan struct{}, workerPoolSize),
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

// deadlineRefreshStep returns the coalesced interval at which socket deadlines should be refreshed.
func deadlineRefreshStep(timeout time.Duration) time.Duration {
	step := timeout / 4
	if step < 100*time.Millisecond {
		step = 100 * time.Millisecond
	}
	if step > timeout {
		step = timeout
	}
	return step
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

// forwardTCP accepts incoming TCP connections and dispatches them directly to the worker pool.
func (p *Proxy) forwardTCP(ctx context.Context, dialer Dialer, port PortMapping, listener net.Listener, connWg *sync.WaitGroup) {
	timeoutSec := port.ConnectionTimeout
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	log.Printf("TCP listening on %d and forwarding to %s (tailnet: %s) with connection timeout %ds",
		port.Source, port.Destination, port.Tailnet, timeoutSec)

	stopClose := context.AfterFunc(ctx, func() {
		_ = listener.Close()
	})
	defer stopClose()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection") {
				log.Printf("TCP listener closed for port %d, exiting accept loop", port.Source)
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			log.Printf("TCP accept error on port %d: %v", port.Source, err)
			continue
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

// handleTCPConnection dials the destination over Tailscale and bridges both streams.
func (p *Proxy) handleTCPConnection(ctx context.Context, dialer Dialer, port PortMapping, conn net.Conn) {
	p.stats.tcpConnections.Add(1)
	defer p.stats.tcpConnections.Add(-1)
	defer conn.Close()

	if err := optimizeTCPConn(conn); err != nil {
		p.stats.errorCount.Add(1)
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
		p.stats.errorCount.Add(1)
		log.Printf("TCP dial error to %s (tailnet %s): %v", port.Destination, port.Tailnet, err)
		return
	}
	defer destConn.Close()

	if err := optimizeTCPConn(destConn); err != nil {
		log.Printf("TCP optimization error for destination %s: %v", port.Destination, err)
	}

	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		_ = destConn.Close()
	})
	defer stopCancel()

	var wg sync.WaitGroup

	wg.Go(func() {
		p.forwardStreamBuffered(conn, destConn, port.Source, timeout)
		if cw, ok := destConn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	})

	p.forwardStreamBuffered(destConn, conn, port.Source, timeout)
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}

	wg.Wait()
}

// forwardUDP reads UDP datagrams across a multi-core reader pool using zero-allocation
// ReadFromUDPAddrPort when available, forwarding packets inline for established sessions
// and dispatching new sessions to the bounded worker pool.
func (p *Proxy) forwardUDP(ctx context.Context, dialer Dialer, port PortMapping, conn net.PacketConn, sessionWg *sync.WaitGroup) {
	timeoutSec := port.ConnectionTimeout
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	timeout := time.Duration(timeoutSec) * time.Second
	refreshStep := deadlineRefreshStep(timeout)

	numReaders := min(max(runtime.GOMAXPROCS(0), 1), 8)
	log.Printf("UDP listening on %d (%d reader workers) and forwarding to %s (tailnet: %s) with connection timeout %ds",
		port.Source, numReaders, port.Destination, port.Tailnet, timeoutSec)

	table := newUDPSessionTable()
	defer table.closeAll()

	stopClose := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})
	defer stopClose()

	udpConn, isUDPConn := conn.(*net.UDPConn)
	if isUDPConn {
		_ = udpConn.SetReadBuffer(4 * 1024 * 1024)
		_ = udpConn.SetWriteBuffer(4 * 1024 * 1024)
	}

	var closeLogOnce sync.Once
	var readerWg sync.WaitGroup

	for readerIdx := range numReaders {
		shardIdx := uint32(readerIdx) & (statsShardCount - 1)
		pktCounter := &p.stats.udpPackets[shardIdx].val
		bytesCounter := &p.stats.bytesTransferred[shardIdx].val

		readerWg.Go(func() {
			readBuf := make([]byte, maxUDPBufferSize)
			var pktCount uint32

			for {
				var (
					n           int
					addrPort    netip.AddrPort
					hasAddrPort bool
					clientAddr  net.Addr
					err         error
				)

				if isUDPConn {
					n, addrPort, err = udpConn.ReadFromUDPAddrPort(readBuf)
					hasAddrPort = true
				} else {
					n, clientAddr, err = conn.ReadFrom(readBuf)
					if uaddr, ok := clientAddr.(*net.UDPAddr); ok {
						addrPort = uaddr.AddrPort()
						hasAddrPort = true
					}
				}

				if err != nil {
					if ctx.Err() != nil || errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection") {
						closeLogOnce.Do(func() {
							log.Printf("UDP connection closed for port %d, exiting read loop", port.Source)
						})
						return
					}
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					}
					log.Printf("UDP read error on port %d: %v", port.Source, err)
					continue
				}

				if n <= 0 {
					continue
				}

				pktCounter.Add(1)

				var key udpSessionKey
				if hasAddrPort {
					key.addrPort = addrPort
				} else {
					key.fallback = clientAddr.String()
				}

				// Fast path: established UDP session forwards inline with zero allocations and zero goroutine spawns.
				if sess := table.get(key); sess != nil && sess.readyOK.Load() {
					if pktCount&7 == 0 {
						sess.refreshDeadlinesIfNeeded(timeout, refreshStep)
					}
					pktCount++
					written, writeErr := sess.destConn.Write(readBuf[:n])
					if written > 0 {
						bytesCounter.Add(int64(written))
					}
					if writeErr != nil {
						table.remove(key, sess)
						sess.close()
						if !isConnectionClosed(writeErr) {
							p.stats.errorCount.Add(1)
							log.Printf("UDP write to destination error on port %d: %v", port.Source, writeErr)
						}
					}
					continue
				}

				// Slow path: new session (or session still dialing) is dispatched to the bounded worker pool.
				pktBuf := p.udpBufPool.Get().(*OptimizedBuffer)
				pktBuf.pool = p.udpBufPool
				pktBuf.Reset()
				_, _ = pktBuf.Write(readBuf[:n])

				sessionWg.Add(1)
				go func(buf *OptimizedBuffer, k udpSessionKey, ap netip.AddrPort, hasAP bool, cAddr net.Addr) {
					defer sessionWg.Done()
					p.acquireWorker()
					defer p.releaseWorker()
					p.handleUDPSessionSlowPath(ctx, dialer, port, conn, udpConn, isUDPConn, buf, k, ap, hasAP, cAddr, table, sessionWg, timeout, refreshStep)
				}(pktBuf, key, addrPort, hasAddrPort, clientAddr)
			}
		})
	}

	readerWg.Wait()
}

// handleUDPSessionSlowPath establishes a new UDP session (or waits for an in-flight dial) and writes the packet.
func (p *Proxy) handleUDPSessionSlowPath(
	ctx context.Context,
	dialer Dialer,
	port PortMapping,
	conn net.PacketConn,
	udpConn *net.UDPConn,
	isUDPConn bool,
	buf *OptimizedBuffer,
	key udpSessionKey,
	addrPort netip.AddrPort,
	hasAddrPort bool,
	clientAddr net.Addr,
	table *udpSessionTable,
	sessionWg *sync.WaitGroup,
	timeout time.Duration,
	refreshStep time.Duration,
) {
	defer buf.Release()

	sess, created := table.getOrCreate(key, addrPort, hasAddrPort, clientAddr)
	if created {
		dialCtx, dialCancel := context.WithTimeout(ctx, timeout)
		destConn, err := dialer.Dial(dialCtx, "udp", port.Destination)
		dialCancel()
		if err != nil {
			sess.failed.Store(true)
			close(sess.ready)
			table.remove(key, sess)
			p.stats.errorCount.Add(1)
			log.Printf("UDP dial error to %s (tailnet %s): %v", port.Destination, port.Tailnet, err)
			return
		}

		sess.destConn = destConn
		now := time.Now()
		sess.lastDeadlineNano.Store(now.UnixNano())
		deadline := now.Add(timeout)
		_ = destConn.SetWriteDeadline(deadline)
		_ = destConn.SetReadDeadline(deadline)

		sess.readyOK.Store(true)
		close(sess.ready)

		sessionWg.Add(1)
		go func() {
			defer sessionWg.Done()
			p.readUDPBackendSession(ctx, port, conn, udpConn, isUDPConn, sess, table, timeout, refreshStep)
		}()
	} else {
		select {
		case <-sess.ready:
			if sess.failed.Load() {
				return
			}
		case <-ctx.Done():
			return
		}
	}

	sess.refreshDeadlinesIfNeeded(timeout, refreshStep)
	bytesWritten, err := buf.WriteTo(sess.destConn)
	if bytesWritten > 0 {
		shardIdx := table.shardIndexFor(key) & (statsShardCount - 1)
		p.stats.bytesTransferred[shardIdx].val.Add(bytesWritten)
	}
	if err != nil {
		table.remove(key, sess)
		sess.close()
		if !isConnectionClosed(err) {
			p.stats.errorCount.Add(1)
			log.Printf("UDP write to destination error on port %d: %v", port.Source, err)
		}
	}
}

// readUDPBackendSession continuously reads backend UDP responses for a specific client session
// and forwards them back to the client using zero-alloc WriteToUDPAddrPort when available.
func (p *Proxy) readUDPBackendSession(
	ctx context.Context,
	port PortMapping,
	conn net.PacketConn,
	udpConn *net.UDPConn,
	isUDPConn bool,
	sess *udpSession,
	table *udpSessionTable,
	timeout time.Duration,
	refreshStep time.Duration,
) {
	defer func() {
		table.remove(sess.key, sess)
		sess.close()
	}()

	stopSession := context.AfterFunc(ctx, func() {
		sess.close()
	})
	defer stopSession()

	respBuffer := p.udpBufPool.Get().(*OptimizedBuffer)
	respBuffer.pool = p.udpBufPool
	defer respBuffer.Release()

	data := respBuffer.data
	shardIdx := (table.shardIndexFor(sess.key) + 8) & (statsShardCount - 1)
	bytesCounter := &p.stats.bytesTransferred[shardIdx].val
	var iter uint32

	for {
		if iter&7 == 0 {
			sess.refreshDeadlinesIfNeeded(timeout, refreshStep)
		}
		iter++

		bytesRead, err := sess.destConn.Read(data)
		if bytesRead > 0 {
			if iter&7 == 1 {
				table.refreshConnWriteDeadline(conn, timeout, refreshStep)
			}

			payload := data[:bytesRead]
			var (
				written  int
				writeErr error
			)
			if isUDPConn && sess.hasAddrPort {
				written, writeErr = udpConn.WriteToUDPAddrPort(payload, sess.addrPort)
			} else if sess.clientAddr != nil {
				written, writeErr = conn.WriteTo(payload, sess.clientAddr)
			} else {
				written, writeErr = conn.WriteTo(payload, net.UDPAddrFromAddrPort(sess.addrPort))
			}

			if written > 0 {
				bytesCounter.Add(int64(written))
			}
			if writeErr != nil {
				if !isConnectionClosed(writeErr) {
					p.stats.errorCount.Add(1)
					log.Printf("UDP write back to source error on port %d: %v", port.Source, writeErr)
				}
				return
			}
		}

		if err != nil {
			if !isConnectionClosed(err) {
				p.stats.errorCount.Add(1)
				log.Printf("UDP read from destination error on port %d: %v", port.Source, err)
			}
			return
		}
	}
}

// forwardStream transfers data from src to dst using pooled OptimizedBuffers,
// coalescing deadline refreshes and updating BytesTransferred in real time.
func (p *Proxy) forwardStream(src, dst net.Conn, port int, timeout time.Duration) {
	p.forwardStreamBuffered(src, dst, port, timeout)
}

func (p *Proxy) forwardStreamBuffered(src, dst net.Conn, port int, timeout time.Duration) {
	buf := p.bufPool.Get().(*OptimizedBuffer)
	buf.pool = p.bufPool
	defer buf.Release()

	data := buf.data
	refreshStep := deadlineRefreshStep(timeout)
	shardIdx := p.stats.nextShard.Add(1) & (statsShardCount - 1)
	bytesCounter := &p.stats.bytesTransferred[shardIdx].val
	var lastReadDeadlineNano int64
	var lastWriteDeadlineNano int64
	var iter uint32

	for {
		if iter&7 == 0 {
			nowNano := time.Now().UnixNano()
			if nowNano-lastReadDeadlineNano >= int64(refreshStep) {
				lastReadDeadlineNano = nowNano
				_ = src.SetReadDeadline(time.Unix(0, nowNano).Add(timeout))
			}
			if nowNano-lastWriteDeadlineNano >= int64(refreshStep) {
				lastWriteDeadlineNano = nowNano
				_ = dst.SetWriteDeadline(time.Unix(0, nowNano).Add(timeout))
			}
		}
		iter++

		bytesRead, err := src.Read(data)

		if bytesRead > 0 {
			writtenTotal := 0
			for writtenTotal < bytesRead {
				n, writeErr := dst.Write(data[writtenTotal:bytesRead])
				if n > 0 {
					writtenTotal += n
				}
				if writeErr != nil {
					if writtenTotal > 0 {
						bytesCounter.Add(int64(writtenTotal))
					}
					if !isConnectionClosed(writeErr) {
						p.stats.errorCount.Add(1)
						log.Printf("Stream write error on port %d: %v", port, writeErr)
					}
					return
				}
				if n == 0 {
					if writtenTotal > 0 {
						bytesCounter.Add(int64(writtenTotal))
					}
					p.stats.errorCount.Add(1)
					return
				}
			}
			bytesCounter.Add(int64(writtenTotal))
		}

		if err != nil {
			if !isConnectionClosed(err) {
				p.stats.errorCount.Add(1)
				log.Printf("Stream read error on port %d: %v", port, err)
			}
			break
		}
	}
}

// GetStats returns a snapshot of the proxy's performance statistics.
func (p *Proxy) GetStats() ProxyStats {
	var totalUDP, totalBytes int64
	for i := range statsShardCount {
		totalUDP += p.stats.udpPackets[i].val.Load()
		totalBytes += p.stats.bytesTransferred[i].val.Load()
	}
	return ProxyStats{
		TCPConnections:   p.stats.tcpConnections.Load(),
		UDPPackets:       totalUDP,
		BytesTransferred: totalBytes,
		ErrorCount:       p.stats.errorCount.Load(),
	}
}

// ReloadConfig updates the proxy port mappings at runtime without dropping unaffected active connections.
// If newPorts is empty, all currently active listeners are gracefully stopped.
func (p *Proxy) ReloadConfig(newPorts []PortMapping) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	oldPortsMap := make(map[string]PortMapping, len(p.Ports))
	newPortsMap := make(map[string]PortMapping, len(newPorts))

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

// RestartTailnetPorts stops and restarts any active port listeners bound to the specified tailnets
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
		strings.TrimSpace(a.ListenAddress) == strings.TrimSpace(b.ListenAddress) &&
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

	listenAddr := port.ListenAddr()
	lc := net.ListenConfig{
		KeepAlive: 30 * time.Second,
	}
	listener, err := lc.Listen(p.ctx, "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to start TCP listener on %s: %w", listenAddr, err)
	}

	ctx, cancel := context.WithCancel(p.ctx)
	info := &listenerInfo{
		listener: listener,
		ctx:      ctx,
		cancel:   cancel,
	}

	p.tcpListeners[key] = info

	info.wg.Go(func() {
		p.forwardTCP(ctx, dialer, port, listener, &info.wg)
	})

	log.Printf("TCP listener started on %s for tailnet %s", listenAddr, port.Tailnet)
	return nil
}

func (p *Proxy) startUDPListener(port PortMapping, key string) error {
	dialer, exists := p.tailnetDialers[port.Tailnet]
	if !exists {
		return fmt.Errorf("tailnet server '%s' not found for port %d", port.Tailnet, port.Source)
	}

	listenAddr := port.ListenAddr()
	conn, err := net.ListenPacket("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to start UDP listener on %s: %w", listenAddr, err)
	}

	ctx, cancel := context.WithCancel(p.ctx)
	info := &listenerInfo{
		conn:   conn,
		ctx:    ctx,
		cancel: cancel,
	}

	p.udpConns[key] = info

	info.wg.Go(func() {
		p.forwardUDP(ctx, dialer, port, conn, &info.wg)
	})

	log.Printf("UDP listener started on %s for tailnet %s", listenAddr, port.Tailnet)
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

// ActiveListenerCount returns the total number of currently bound TCP and UDP listeners.
func (p *Proxy) ActiveListenerCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.tcpListeners) + len(p.udpConns)
}

// IsReady reports whether the proxy is running and all configured port listeners are bound.
func (p *Proxy) IsReady() bool {
	if p.ctx.Err() != nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.Ports) == 0 || (len(p.tcpListeners)+len(p.udpConns) >= len(p.Ports))
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
