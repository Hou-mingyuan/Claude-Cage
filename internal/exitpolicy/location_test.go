package exitpolicy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type locationTransport func(*http.Request) (*http.Response, error)

func (f locationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const validLocationJSON = `{"ip":"8.8.8.8","country":"US","country_name":"United States","country_code":"US","timezone":"America/Los_Angeles"}`

func TestLocationTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deadline    time.Duration
		before      time.Duration
		lastWorks   bool
		wantElapsed time.Duration
		wantCalls   int
		wantError   error
	}{
		{name: "last provider succeeds after four timeouts", lastWorks: true, wantElapsed: 39 * time.Second, wantCalls: 5},
		{name: "all providers time out", wantElapsed: 40 * time.Second, wantCalls: 5},
		{name: "caller deadline stops further providers", deadline: 20 * time.Second, wantElapsed: 20 * time.Second, wantCalls: 3, wantError: context.DeadlineExceeded},
		// A monitoring round also spends its 45-second budget on exit sampling
		// and environment checks; the last provider cannot extend that deadline.
		{name: "spent monitoring budget interrupts available last provider", deadline: 45 * time.Second, before: 9 * time.Second, lastWorks: true, wantElapsed: 45 * time.Second, wantCalls: 5, wantError: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				ctx := context.Background()
				if tc.deadline != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.deadline)
					defer cancel()
				}
				time.Sleep(tc.before)
				var hosts []string
				client := &http.Client{Timeout: 8 * time.Second, Transport: locationTransport(func(r *http.Request) (*http.Response, error) {
					hosts = append(hosts, r.URL.Host)
					if tc.lastWorks && r.URL.Host == "ipapi.co" {
						select {
						case <-time.After(7 * time.Second):
							return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(validLocationJSON))}, nil
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				})}
				loc, err := Lookup(ctx, client, net.ParseIP("8.8.8.8"))
				wantHosts := []string{"ipwho.is", "api.ip.sb", "ipwhois.app", "get.geojs.io", "ipapi.co"}
				if !slices.Equal(hosts, wantHosts[:tc.wantCalls]) {
					t.Fatalf("unexpected fallback requests: %v", hosts)
				}
				if elapsed := time.Since(started); elapsed != tc.wantElapsed {
					t.Fatalf("lookup used %v, want %v", elapsed, tc.wantElapsed)
				}
				switch {
				case tc.wantError != nil:
					if !errors.Is(err, tc.wantError) || loc != (Location{}) {
						t.Fatalf("caller deadline returned location=%+v, error=%v", loc, err)
					}
				case tc.lastWorks:
					if err != nil || loc.IP != "8.8.8.8" || loc.CountryCode != "US" || loc.Timezone != "America/Los_Angeles" {
						t.Fatalf("last provider did not recover the lookup: %+v, %v", loc, err)
					}
				default:
					if err == nil || loc != (Location{}) {
						t.Fatalf("timed-out providers returned location=%+v, error=%v", loc, err)
					}
					for _, host := range []string{"ipwho.is", "IP.SB", "ipwhois.app", "GeoJS", "ipapi.co"} {
						if !strings.Contains(err.Error(), host+"：连接失败或超时") {
							t.Fatalf("missing sanitized timeout for %s: %v", host, err)
						}
					}
				}
			})
		})
	}
}

func TestLocationRequestsDoNotSendProjectIdentity(t *testing.T) {
	requests := make(chan *http.Request, 5)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		if r.Host != "ipapi.co" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, validLocationJSON)
	}))
	defer upstream.Close()
	endpoint, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := upstream.Client().Transport
	client := &http.Client{Transport: locationTransport(func(r *http.Request) (*http.Response, error) {
		forward := r.Clone(r.Context())
		forward.Host = r.URL.Host
		forward.URL.Scheme, forward.URL.Host = endpoint.Scheme, endpoint.Host
		return transport.RoundTrip(forward)
	})}
	if _, err := Lookup(context.Background(), client, net.ParseIP("8.8.8.8")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ipwho.is/8.8.8.8?fields=success,ip,country,country_code,region,region_code,city,timezone.id",
		"api.ip.sb/geoip/8.8.8.8",
		"ipwhois.app/json/8.8.8.8",
		"get.geojs.io/v1/ip/geo/8.8.8.8.json",
		"ipapi.co/8.8.8.8/json/",
	} {
		select {
		case r := <-requests:
			if got := r.Host + r.URL.RequestURI(); got != want || r.Method != http.MethodGet {
				t.Fatalf("unexpected provider request: %s %s", r.Method, got)
			}
			if r.UserAgent() != "Go-http-client/1.1" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" {
				t.Fatalf("provider received project identity or credentials: %v", r.Header)
			}
		default:
			t.Fatal("provider did not receive request", want)
		}
	}
}

func TestLocationRejectsBogonResponse(t *testing.T) {
	client := &http.Client{Transport: locationTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "ipapi.co" {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ip":"8.8.8.8","bogon":true,"country_code":"US","timezone":"America/Los_Angeles"}`))}, nil
		}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if _, err := Lookup(context.Background(), client, net.ParseIP("8.8.8.8")); err == nil {
		t.Fatal("bogon response must be rejected")
	}
}

// ipinfo.io is a third party the user must opt into. With the switch off
// (the default) no request reaches it, matching the five-provider behavior.
func TestLocationIPInfoFallbackIsOptIn(t *testing.T) {
	const ipinfoBody = `{"ip":"8.8.8.8","country":"US","region":"California","city":"Mountain View","timezone":"America/Los_Angeles"}`
	run := func(options LookupOptions) ([]string, Location, error) {
		var hosts []string
		client := &http.Client{Transport: locationTransport(func(r *http.Request) (*http.Response, error) {
			hosts = append(hosts, r.URL.Host)
			if r.URL.Host == "ipinfo.io" {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(ipinfoBody))}, nil
			}
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		loc, err := LookupWithOptions(context.Background(), client, net.ParseIP("8.8.8.8"), options)
		return hosts, loc, err
	}
	defaults := []string{"ipwho.is", "api.ip.sb", "ipwhois.app", "get.geojs.io", "ipapi.co"}
	for name, options := range map[string]LookupOptions{"zero options": {}, "explicit off": {IPInfoFallback: false}} {
		hosts, loc, err := run(options)
		if !slices.Equal(hosts, defaults) || err == nil || loc != (Location{}) || strings.Contains(err.Error(), "ipinfo") {
			t.Fatalf("%s: hosts=%v location=%+v err=%v", name, hosts, loc, err)
		}
	}
	var hosts []string
	client := &http.Client{Transport: locationTransport(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if _, err := Lookup(context.Background(), client, net.ParseIP("8.8.8.8")); err == nil || !slices.Equal(hosts, defaults) {
		t.Fatalf("Lookup must keep the default providers: hosts=%v err=%v", hosts, err)
	}

	hosts, loc, err := run(LookupOptions{IPInfoFallback: true})
	if err != nil || !slices.Equal(hosts, append(slices.Clone(defaults), "ipinfo.io")) {
		t.Fatalf("opt-in fallback: hosts=%v err=%v", hosts, err)
	}
	// Country-level only: finer fields from another provider's spelling must
	// never bind or fail a region or city rule.
	if loc.CountryCode != "US" || loc.Country != "US" || loc.Region != "" || loc.RegionCode != "" || loc.City != "" || loc.Timezone != "America/Los_Angeles" {
		t.Fatalf("opt-in fallback must keep only country-level fields: %+v", loc)
	}
}

func TestLocationIPInfoFallbackCannotBindOrFailRegion(t *testing.T) {
	loc := Location{IP: "8.8.8.8", CountryCode: "US"}
	for _, scope := range []string{"region", "city"} {
		unbound := Policy{Mode: "dynamic", Scope: scope}
		bound := unbound.BindMissing(loc)
		if bound.Region != "" || bound.City != "" {
			t.Fatalf("%s: country-only location bound finer fields: %+v", scope, bound)
		}
		configured := Policy{Mode: "dynamic", Scope: scope, CountryCode: "US", Region: "California", City: "Mountain View"}
		if state, _ := configured.Evaluate(loc.IP, &loc); state == Failed {
			t.Fatalf("%s: country-only location must not be judged a violation", scope)
		}
	}
	if state, _ := (Policy{Mode: "dynamic", Scope: "country", CountryCode: "US"}).Evaluate(loc.IP, &loc); state != Passed {
		t.Fatal("country-level rule must pass with the fallback result")
	}
}

func TestLocationDistinguishesTLSHandshakeEOF(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				// Read the client's ClientHello before closing so the peer
				// observes a clean FIN (io.EOF) instead of a TCP RST race.
				buf := make([]byte, 4096)
				_, _ = conn.Read(buf)
			}(c)
		}
	}()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, ln.Addr().String())
		},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	_, err = Lookup(context.Background(), &http.Client{Transport: transport, Timeout: 2 * time.Second}, net.ParseIP("8.8.8.8"))
	if err == nil || !strings.Contains(err.Error(), "TLS 握手失败（EOF）") {
		t.Fatalf("expected TLS handshake EOF classification, got %v", err)
	}
}
