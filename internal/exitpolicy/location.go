package exitpolicy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

// LookupOptions holds the user's explicit location-lookup choices. The zero
// value is the default: only the five built-in providers are queried.
type LookupOptions struct {
	// IPInfoFallback, off by default, lets ipinfo.io receive the measured
	// exit IP after the five built-in providers have all failed. Its answer is
	// used only at country level: region and city are left empty, so it can
	// never bind or fail a state, province or city rule.
	IPInfoFallback bool
}

// IPInfoFallbackHost is the third party contacted when the opt-in fallback is on.
const IPInfoFallbackHost = "ipinfo.io"

// Lookup queries the default providers as needed for the measured address.
// The caller supplies the shared proxy-only client, including for fallback.
func Lookup(ctx context.Context, client *http.Client, address net.IP) (Location, error) {
	return LookupWithOptions(ctx, client, address, LookupOptions{})
}

// LookupWithOptions is Lookup with the user's opt-in fallbacks.
func LookupWithOptions(ctx context.Context, client *http.Client, address net.IP, options LookupOptions) (Location, error) {
	type provider struct {
		name             string
		url              string
		requireSuccess   bool
		countryFieldCode bool
		countryOnly      bool
	}
	providers := []provider{
		{"ipwho.is", "https://ipwho.is/" + address.String() + "?fields=success,ip,country,country_code,region,region_code,city,timezone.id", true, false, false},
		{"IP.SB", "https://api.ip.sb/geoip/" + address.String(), false, false, false},
		{"ipwhois.app", "https://ipwhois.app/json/" + address.String(), true, false, false},
		{"GeoJS", "https://get.geojs.io/v1/ip/geo/" + address.String() + ".json", false, false, false},
		{"ipapi.co", "https://ipapi.co/" + address.String() + "/json/", false, false, false},
	}
	if options.IPInfoFallback {
		providers = append(providers, provider{IPInfoFallbackHost, "https://" + IPInfoFallbackHost + "/" + address.String() + "/json", false, true, true})
	}
	failures := make([]string, 0, len(providers))
	for _, provider := range providers {
		if err := ctx.Err(); err != nil {
			return Location{}, err
		}
		loc, err := queryLocation(ctx, client, provider.url, provider.requireSuccess, provider.countryFieldCode, address)
		if err == nil {
			if provider.countryOnly {
				// Region and city spellings differ between providers; a
				// country-level fallback must not bind or fail finer rules.
				loc.Region, loc.RegionCode, loc.City = "", "", ""
			}
			return loc, nil
		}
		// Only our own summaries reach the UI; transport errors may contain
		// local network details and provider bodies are untrusted text.
		failures = append(failures, provider.name+"："+err.Error())
	}
	if err := ctx.Err(); err != nil {
		return Location{}, err
	}
	return Location{}, fmt.Errorf("已测得出口 IP，但位置查询均失败（%s）；请重试或切换节点。未使用直连回退", strings.Join(failures, "；"))
}

func queryLocation(ctx context.Context, client *http.Client, endpoint string, requireSuccess, countryFieldCode bool, address net.IP) (Location, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var (
		mu                  sync.Mutex
		tlsStarted, tlsDone bool
	)
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: func() {
			mu.Lock()
			tlsStarted = true
			mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				mu.Lock()
				tlsDone = true
				mu.Unlock()
			}
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, endpoint, nil)
	if err != nil {
		return Location{}, errors.New("查询地址无效")
	}
	r, err := client.Do(req)
	if err != nil {
		mu.Lock()
		inTLS := tlsStarted && !tlsDone
		mu.Unlock()
		if inTLS {
			var netErr net.Error
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
				return Location{}, errors.New("TLS 握手超时")
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return Location{}, errors.New("TLS 握手失败（EOF）")
			}
			return Location{}, errors.New("TLS 握手失败")
		}
		return Location{}, errors.New("连接失败或超时")
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return Location{}, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	var v struct {
		Success     *bool           `json:"success"`
		Error       *bool           `json:"error"`
		Bogon       *bool           `json:"bogon"`
		IP          string          `json:"ip"`
		Country     string          `json:"country"`
		CountryName string          `json:"country_name"`
		CountryCode string          `json:"country_code"`
		Region      string          `json:"region"`
		RegionCode  string          `json:"region_code"`
		City        string          `json:"city"`
		Timezone    json.RawMessage `json:"timezone"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 128<<10)).Decode(&v) != nil ||
		(v.Error != nil && *v.Error) ||
		(v.Bogon != nil && *v.Bogon) ||
		(requireSuccess && (v.Success == nil || !*v.Success)) ||
		net.ParseIP(v.IP) == nil || !net.ParseIP(v.IP).Equal(address) {
		return Location{}, errors.New("响应无效或 IP 不匹配")
	}
	countryCode := v.CountryCode
	if countryFieldCode {
		countryCode = v.Country
	}
	country := strings.ToUpper(strings.TrimSpace(countryCode))
	if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' {
		return Location{}, errors.New("缺少有效国家或地区")
	}
	// 同时兼容字符串时区（如 IP.SB/ipwhois.app/GeoJS/ipapi.co/ipinfo.io）与对象格式（ipwho.is 的 timezone.id）
	var timezone string
	if json.Unmarshal(v.Timezone, &timezone) != nil {
		var zone struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(v.Timezone, &zone) == nil {
			timezone = zone.ID
		}
	}
	if timezone == "" || timezone == "Local" {
		return Location{}, errors.New("缺少有效时区")
	}
	if _, err = time.LoadLocation(timezone); err != nil {
		return Location{}, errors.New("缺少有效时区")
	}
	// ipapi.co 的 country 字段为两位代码，完整国家名位于 country_name；
	// ipinfo.io 仅返回两位国家代码，显示时回退为代码。
	countryName := v.CountryName
	if countryName == "" {
		countryName = v.Country
	}
	return Location{IP: address.String(), Country: countryName, CountryCode: country, Region: v.Region, RegionCode: v.RegionCode, City: v.City, Timezone: timezone, CheckedAt: time.Now()}, nil
}
