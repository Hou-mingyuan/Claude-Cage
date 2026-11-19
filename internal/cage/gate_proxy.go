package cage

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// GateProxy adapts explicit HTTP proxy clients to the existing egress gate.
// It never uses host DNS or a direct transport, and exposes no gate controls.
// token is a dedicated workload credential, not the upstream proxy credential.
func GateProxy(gate *EgressGate, token string) (http.Handler, error) {
	if len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" {
		return nil, errors.New("invalid workload proxy credential")
	}
	authorization := []byte("Basic " + base64.StdEncoding.EncodeToString([]byte("proxy:"+token)))
	// Existing remote sessions may still have the previous username in their environment.
	legacyAuthorization := []byte("Basic " + base64.StdEncoding.EncodeToString([]byte("cage:"+token)))
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrGateClosed
		}
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return nil, ErrGateClosed
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			return gate.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, uint16(p)).String())
		}
		if _, ok := dns.IsDomainName(host); !ok {
			return nil, ErrGateClosed
		}
		query := new(dns.Msg).SetQuestion(dns.Fqdn(host), dns.TypeA)
		reply, err := gate.ExchangeDNS(ctx, query)
		if err != nil || reply == nil || reply.Rcode != dns.RcodeSuccess {
			return nil, ErrGateClosed
		}
		for _, rr := range reply.Answer {
			if record, ok := rr.(*dns.A); ok {
				connection, err := gate.DialContext(ctx, "tcp", net.JoinHostPort(record.A.String(), port))
				if err == nil {
					return connection, nil
				}
			}
		}
		return nil, ErrGateClosed
	}
	transport := &http.Transport{Proxy: nil, DialContext: dial, DisableKeepAlives: true,
		ResponseHeaderTimeout: 30 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	slots := make(chan struct{}, 64)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := []byte(r.Header.Get("Proxy-Authorization"))
		if subtle.ConstantTimeCompare(provided, authorization)|subtle.ConstantTimeCompare(provided, legacyAuthorization) != 1 {
			w.Header().Set("Proxy-Authenticate", `Basic realm="proxy"`)
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "busy", 503)
			return
		}
		if r.Method == http.MethodConnect {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			upstream, err := dial(ctx, "tcp", r.Host)
			cancel()
			if err != nil {
				http.Error(w, "egress unavailable", http.StatusForbidden)
				return
			}
			defer upstream.Close()
			client, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer client.Close()
			_ = client.SetDeadline(time.Time{})
			if _, err = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
				return
			}
			if err = buffer.Flush(); err != nil {
				return
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				// A client may signal end-of-request with FIN and still await a reply.
				if _, err := io.Copy(upstream, buffer); err != nil || closeWrite(upstream) != nil {
					_ = upstream.Close()
				}
			}()
			_, _ = io.Copy(client, upstream)
			_ = upstream.Close()
			_ = client.Close()
			<-done
			return
		}
		if r.URL.Scheme != "http" || r.URL.Host == "" || r.URL.User != nil || r.URL.Fragment != "" {
			http.Error(w, "explicit HTTP proxy required", 400)
			return
		}
		request := r.Clone(r.Context())
		request.RequestURI = ""
		request.Host = request.URL.Host
		stripProxyHeaders(request.Header)
		response, err := transport.RoundTrip(request)
		if err != nil {
			http.Error(w, "egress unavailable", http.StatusForbidden)
			return
		}
		defer response.Body.Close()
		stripProxyHeaders(response.Header)
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}), nil
}

func stripProxyHeaders(header http.Header) {
	for _, connection := range header.Values("Connection") {
		for _, name := range strings.Split(connection, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}
