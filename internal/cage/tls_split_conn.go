package cage

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// tlsClientHelloSplitThreshold is the payload size above which a TLS
	// ClientHello is written in two segments when compatibility mode is enabled.
	// Standard TLS 1.3 ClientHello packets with hybrid post-quantum key shares
	// (e.g. X25519MLKEM768) are around 1500 bytes and can be dropped with EOF by
	// some proxy chains when sent as a single large initial write.
	tlsClientHelloSplitThreshold = 1000
	tlsClientHelloFirstChunk     = 600
	tlsClientHelloSplitDelay     = 30 * time.Millisecond
)

func isLargeTLSClientHello(p []byte) bool {
	return len(p) > tlsClientHelloSplitThreshold &&
		len(p) >= 6 &&
		p[0] == 0x16 && // TLS Handshake record
		p[1] == 0x03 && // TLS major version 3.x (SSL 3.0 / TLS 1.0-1.3 record layer)
		p[2] <= 0x04 &&
		p[5] == 0x01 // HandshakeType ClientHello
}

type tlsClientHelloSplitConn struct {
	net.Conn
	delay     time.Duration
	doneFirst atomic.Bool
	mu        sync.Mutex
	closed    chan struct{}
	closeOnce sync.Once
}

func newTLSClientHelloSplitConn(conn net.Conn) net.Conn {
	return newTLSClientHelloSplitConnWithDelay(conn, tlsClientHelloSplitDelay)
}

func newTLSClientHelloSplitConnWithDelay(conn net.Conn, delay time.Duration) net.Conn {
	return &tlsClientHelloSplitConn{
		Conn:   conn,
		delay:  delay,
		closed: make(chan struct{}),
	}
}

func (c *tlsClientHelloSplitConn) Write(p []byte) (int, error) {
	if c.doneFirst.Load() {
		return c.Conn.Write(p)
	}
	c.mu.Lock()
	if c.doneFirst.Load() {
		c.mu.Unlock()
		return c.Conn.Write(p)
	}
	defer func() {
		c.doneFirst.Store(true)
		c.mu.Unlock()
	}()
	if !isLargeTLSClientHello(p) {
		return c.Conn.Write(p)
	}
	disableNagle(c.Conn)
	n1, err := c.Conn.Write(p[:tlsClientHelloFirstChunk])
	if err != nil {
		return n1, err
	}
	if c.delay > 0 {
		timer := time.NewTimer(c.delay)
		select {
		case <-timer.C:
		case <-c.closed:
			timer.Stop()
			return n1, net.ErrClosed
		}
	}
	n2, err := c.Conn.Write(p[n1:])
	return n1 + n2, err
}

func (c *tlsClientHelloSplitConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *tlsClientHelloSplitConn) CloseWrite() error {
	return closeWrite(c.Conn)
}

func disableNagle(conn net.Conn) {
	for conn != nil {
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
			return
		}
		if b, ok := conn.(*bufferedConn); ok {
			conn = b.Conn
			continue
		}
		return
	}
}
