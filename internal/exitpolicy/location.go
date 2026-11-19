package exitpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	_ "time/tzdata"
)

// Lookup queries the configured providers as needed for the measured address.
// The caller supplies the shared proxy-only client, including for fallback.
func Lookup(ctx context.Context, client *http.Client, address net.IP) (Location, error) {
	providers := []struct {
		name           string
		url            string
		requireSuccess bool
	}{
		{"ipwho.is", "https://ipwho.is/" + address.String() + "?fields=success,ip,country,country_code,region,region_code,city,timezone.id", true},
		{"IP.SB", "https://api.ip.sb/geoip/" + address.String(), false},
		{"ipwhois.app", "https://ipwhois.app/json/" + address.String(), true},
		{"GeoJS", "https://get.geojs.io/v1/ip/geo/" + address.String() + ".json", false},
		{"ipapi.co", "https://ipapi.co/" + address.String() + "/json/", false},
	}
	failures := make([]string, 0, len(providers))
	for _, provider := range providers {
		if err := ctx.Err(); err != nil {
			return Location{}, err
		}
		loc, err := queryLocation(ctx, client, provider.url, provider.requireSuccess, address)
		if err == nil {
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

func queryLocation(ctx context.Context, client *http.Client, endpoint string, requireSuccess bool, address net.IP) (Location, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Location{}, errors.New("查询地址无效")
	}
	r, err := client.Do(req)
	if err != nil {
		return Location{}, errors.New("连接失败或超时")
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return Location{}, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	var v struct {
		Success     *bool           `json:"success"`
		Error       *bool           `json:"error"`
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
		(requireSuccess && (v.Success == nil || !*v.Success)) ||
		net.ParseIP(v.IP) == nil || !net.ParseIP(v.IP).Equal(address) {
		return Location{}, errors.New("响应无效或 IP 不匹配")
	}
	country := strings.ToUpper(strings.TrimSpace(v.CountryCode))
	if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' {
		return Location{}, errors.New("缺少有效国家或地区")
	}
	// 同时兼容字符串时区（如 IP.SB/ipwhois.app/GeoJS/ipapi.co）与对象格式（ipwho.is 的 timezone.id）
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
	// ipapi.co 的 country 字段为两位代码，完整国家名位于 country_name
	countryName := v.CountryName
	if countryName == "" {
		countryName = v.Country
	}
	return Location{IP: address.String(), Country: countryName, CountryCode: country, Region: v.Region, RegionCode: v.RegionCode, City: v.City, Timezone: timezone, CheckedAt: time.Now()}, nil
}
