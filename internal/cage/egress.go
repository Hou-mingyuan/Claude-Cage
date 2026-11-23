package cage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/sylvinus/agent-vm/internal/netguard"
	"github.com/sylvinus/agent-vm/internal/usererror"
	"golang.org/x/net/proxy"
)

// Egress has one upstream and no direct fallback. It lives in the hostagent,
// outside the VM. The guest never receives the upstream credentials.
type Egress struct {
	upstream *url.URL
	dnsURLs  []string
	// dnsPreferred is the endpoint that answered last, so a blocked primary
	// does not cost every query a timeout. dnsPrimaryRetry (Unix nanoseconds)
	// is when one query next tries the primary endpoint first again, so a
	// single transient failure does not leave every later query on a backup.
	dnsPreferred     atomic.Int32
	dnsPrimaryRetry  atomic.Int64
	client           *http.Client
	public           netguard.Policy
	connect          proxy.ContextDialer
	splitClientHello bool
}

// The SOCKS handshake runs on our socket so TCP half-close remains available
// to the relay after authentication. DialContext returns a net.Conn wrapper.
type socksHandshaker interface {
	DialWithConn(context.Context, net.Conn, string, string) (net.Addr, error)
}

// dnsPrimaryRetryInterval is how long queries stay on a backup DoH endpoint
// before one of them tries the primary first again.
var dnsPrimaryRetryInterval = 5 * time.Minute

// dnsStart returns the endpoint a query tries first: the one that answered
// last, or, once dnsPrimaryRetryInterval has passed since the switch to a
// backup, the primary. Only the query that claims the retry slot tries the
// primary; concurrent queries keep the backup meanwhile.
func (e *Egress) dnsStart() int {
	start := int(e.dnsPreferred.Load())
	if start == 0 {
		return 0
	}
	due := e.dnsPrimaryRetry.Load()
	now := time.Now()
	if now.UnixNano() >= due && e.dnsPrimaryRetry.CompareAndSwap(due, now.Add(dnsPrimaryRetryInterval).UnixNano()) {
		return 0
	}
	return start
}

func (e *Egress) dnsAnswered(index int) {
	if index != 0 && e.dnsPreferred.Swap(int32(index)) == 0 {
		e.dnsPrimaryRetry.Store(time.Now().Add(dnsPrimaryRetryInterval).UnixNano())
		return
	}
	e.dnsPreferred.Store(int32(index))
}

func NewEgress(c Config) (*Egress, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	u, _ := url.Parse(c.ProxyURL)
	e := &Egress{
		upstream:         u,
		dnsURLs:          c.DNSEndpoints(),
		connect:          &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second},
		splitClientHello: c.TLSClientHelloSplit,
	}
	e.client = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: e.DialContext, ForceAttemptHTTP2: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("DNS redirect refused") }}
	return e, nil
}

func (e *Egress) wrapConn(conn net.Conn) net.Conn {
	if e.splitClientHello {
		return newTLSClientHelloSplitConn(conn)
	}
	return conn
}

func (e *Egress) Allows(network string, dst netip.AddrPort) bool {
	// UDP and ICMP have no fallback. Guest DNS uses the dedicated DoH path.
	return network == "tcp" && dst.Addr().Unmap().Is4() && e.public.Allows(network, dst)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *bufferedConn) CloseWrite() error          { return closeWrite(c.Conn) }

func closeWrite(c net.Conn) error {
	if w, ok := c.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return errors.ErrUnsupported
}

func proxyConnectionError(ctx context.Context, action string, err error) error {
	// Closing a stalled handshake on cancellation can report net.ErrClosed.
	// Preserve the operation deadline so exit monitoring can classify a timeout.
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		// The socket deadline can fire before the context timer publishes Err.
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			err = errors.Join(context.DeadlineExceeded, err)
		}
	}
	return usererror.Wrap(action, err, "请检查代理；请求不会改为直连。")
}

// DialContext is used for all guest TCP connections and DoH. DNS names are
// passed to the proxy without resolving them on the host.
func (e *Egress) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("cage permits TCP only")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if strings.HasPrefix(e.upstream.Scheme, "socks5") {
		var auth *proxy.Auth
		if e.upstream.User != nil {
			p, _ := e.upstream.User.Password()
			auth = &proxy.Auth{User: e.upstream.User.Username(), Password: p}
		}
		d, err := proxy.SOCKS5("tcp", e.upstream.Host, auth, nil)
		if err != nil {
			return nil, errors.New("invalid SOCKS5 configuration")
		}
		handshake, ok := d.(socksHandshaker)
		if !ok {
			return nil, errors.New("SOCKS5 dialer cannot negotiate on an existing connection")
		}
		c, err := e.connect.DialContext(ctx, "tcp", e.upstream.Host)
		if err != nil {
			return nil, proxyConnectionError(ctx, "连接指定 SOCKS5 代理", err)
		}
		if _, err := handshake.DialWithConn(ctx, c, "tcp", addr); err != nil {
			_ = c.Close()
			return nil, proxyConnectionError(ctx, "连接指定 SOCKS5 代理", err)
		}
		return e.wrapConn(c), nil
	}
	conn, err := e.connect.DialContext(ctx, "tcp", e.upstream.Host)
	if err != nil {
		return nil, proxyConnectionError(ctx, "连接指定代理", err)
	}
	// Cancellation must interrupt a stalled CONNECT response, not only the
	// initial TCP connect. Stop the callback before returning a live tunnel.
	upstreamConn := conn
	stopCancel := context.AfterFunc(ctx, func() { _ = upstreamConn.Close() })
	defer stopCancel()
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	if deadline, yes := ctx.Deadline(); yes {
		conn.SetDeadline(deadline)
	}
	if e.upstream.Scheme == "https" {
		t := tls.Client(conn, &tls.Config{ServerName: e.upstream.Hostname(), MinVersion: tls.VersionTLS12})
		if err = t.HandshakeContext(ctx); err != nil {
			return nil, proxyConnectionError(ctx, "验证 HTTPS 代理的加密连接", err)
		}
		conn = t
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: make(http.Header)}
	if e.upstream.User != nil {
		p, _ := e.upstream.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(e.upstream.User.Username()+":"+p)))
	}
	if err = req.Write(conn); err != nil {
		return nil, proxyConnectionError(ctx, "发送代理 CONNECT 请求", err)
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, req)
	if err != nil {
		return nil, proxyConnectionError(ctx, "读取代理 CONNECT 响应", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("代理拒绝 CONNECT 请求（HTTP %d）；请求不会改为直连", resp.StatusCode)
	}
	conn.SetDeadline(time.Time{})
	ok = true
	return e.wrapConn(&bufferedConn{Conn: conn, reader: r}), nil
}

// ExchangeDNS forwards DNS wire messages over HTTPS via the same upstream.
// It never calls the host resolver, even when the upstream fails.
func (e *Egress) ExchangeDNS(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
	for _, question := range q.Question {
		// The initial exit policy monitors IPv4. Do not offer an unmeasured
		// guest IPv6 route; literal IPv6 destinations are denied by Allows too.
		if question.Qtype == dns.TypeAAAA {
			r := new(dns.Msg)
			r.SetReply(q)
			return r, nil
		}
		n := strings.ToLower(strings.TrimSuffix(question.Name, "."))
		if n == "localhost" || strings.HasSuffix(n, ".localhost") || strings.HasSuffix(n, ".internal") || strings.HasSuffix(n, ".local") {
			r := new(dns.Msg)
			r.SetRcode(q, dns.RcodeNameError)
			return r, nil
		}
	}
	wire, err := q.Pack()
	if err != nil {
		return nil, err
	}
	// Keep the original total deadline, but allow a fresh connection after an
	// intermittent EOF or overloaded DoH server. Every attempt uses this same
	// proxy-only transport. A retryable failure moves to the next configured
	// endpoint; with one endpoint every attempt reuses it.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	endpoints := e.dnsURLs
	start := e.dnsStart()
	attempts := max(3, len(endpoints))
	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			return nil, e.dnsUnavailable(ctx.Err())
		}
		index := (start + attempt) % len(endpoints)
		attemptCtx, done := context.WithTimeout(ctx, 6*time.Second)
		var answer *dns.Msg
		var retry bool
		answer, retry, err = e.exchangeDNSOnce(attemptCtx, endpoints[index], q, wire)
		done()
		if err == nil {
			e.dnsAnswered(index)
			return answer, nil
		}
		if !retry {
			return answer, err
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return nil, e.dnsUnavailable(ctx.Err())
			case <-time.After(time.Duration(attempt+1) * 150 * time.Millisecond):
			}
		}
	}
	return nil, e.dnsUnavailable(err)
}

// dnsUnavailable names the endpoints (hosts only) and where to change them.
func (e *Egress) dnsUnavailable(cause error) error {
	hosts := make([]string, 0, len(e.dnsURLs))
	for _, endpoint := range e.dnsURLs {
		hosts = append(hosts, DNSHost(endpoint))
	}
	return fmt.Errorf("经所选节点无法使用加密 DNS（%s）；可在“设置 → 网络与代理 → 加密 DNS”改用其他端点、添加备用端点，或开启公共 DNS 备用：%w", strings.Join(hosts, "、"), cause)
}

func (e *Egress) exchangeDNSOnce(ctx context.Context, endpoint string, q *dns.Msg, wire []byte) (*dns.Msg, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(wire))
	if err != nil {
		return nil, false, errors.New("invalid DNS request")
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, true, errors.New("cage DNS upstream unavailable; check the configured DoH endpoint through the selected proxy")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode == 429 || resp.StatusCode >= 500, fmt.Errorf("cage DNS upstream returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, true, errors.New("incomplete DNS upstream response")
	}
	r := new(dns.Msg)
	if err = r.Unpack(body); err != nil {
		return nil, false, errors.New("invalid DNS upstream response")
	}
	if !r.Response || r.Id != q.Id {
		return nil, false, errors.New("mismatched DNS upstream response")
	}
	return r, false, nil
}
