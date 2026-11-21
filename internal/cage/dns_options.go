package cage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/sylvinus/agent-vm/internal/host"
)

// DNSOptionsFile sits beside the configuration. A separate file keeps the main
// configuration readable by older versions, which reject unknown fields, so a
// rollback never fails on these options.
const DNSOptionsFile = "dns.json"

// PublicDNSFallback is the second public provider used only when the user
// turns on DNSOptions.PublicFallback. It is reached through the selected
// proxy, like the primary endpoint.
const PublicDNSFallback = "https://8.8.8.8/dns-query"

// DefaultBootstrapDNS resolves proxy server names on the host network. The
// entries are IP literals because nothing can resolve a DoH name first.
var DefaultBootstrapDNS = []string{"https://1.1.1.1/dns-query", "https://223.5.5.5/dns-query"}

const maxDNSEndpoints = 4

// DNSOptions extends Config.DNSURL. Lists are kept as newline-joined strings
// so Config stays comparable; use the accessor methods.
type DNSOptions struct {
	fallback       string
	bootstrap      string
	PublicFallback bool
}

type dnsOptionsFile struct {
	FallbackURLs   []string `json:"fallback_urls,omitempty"`
	BootstrapURLs  []string `json:"bootstrap_urls,omitempty"`
	PublicFallback bool     `json:"public_fallback,omitempty"`
}

func NewDNSOptions(fallback, bootstrap []string, publicFallback bool) DNSOptions {
	return DNSOptions{fallback: strings.Join(cleanList(fallback), "\n"), bootstrap: strings.Join(cleanList(bootstrap), "\n"), PublicFallback: publicFallback}
}

func cleanList(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, item := range list {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// FallbackURLs are the user's backup endpoints, tried in order through the
// selected proxy after DNSURL.
func (o DNSOptions) FallbackURLs() []string { return splitList(o.fallback) }

// BootstrapURLs are the user's node-name resolvers; empty means the defaults.
func (o DNSOptions) BootstrapURLs() []string { return splitList(o.bootstrap) }

// DNSEndpoints is every endpoint guest DNS, the DNS check and the proxy core's
// own nameserver use, in order. All of them go through the selected proxy.
func (c Config) DNSEndpoints() []string {
	list := append([]string{c.DNSURL}, c.DNS.FallbackURLs()...)
	if c.DNS.PublicFallback {
		list = append(list, PublicDNSFallback)
	}
	return cleanList(list)
}

// BootstrapEndpoints resolves proxy server names on the host network.
func (c Config) BootstrapEndpoints() []string {
	if list := c.DNS.BootstrapURLs(); len(list) > 0 {
		return list
	}
	return append([]string(nil), DefaultBootstrapDNS...)
}

func validDoH(raw string) (*url.URL, error) {
	d, err := url.Parse(raw)
	if err != nil || d.Scheme != "https" || d.Hostname() == "" || d.User != nil || d.Fragment != "" {
		return nil, errors.New("DNS URL must be an HTTPS DNS-over-HTTPS endpoint")
	}
	return d, nil
}

func (o DNSOptions) Validate() error {
	fallback, bootstrap := o.FallbackURLs(), o.BootstrapURLs()
	if len(fallback) > maxDNSEndpoints || len(bootstrap) > maxDNSEndpoints {
		return fmt.Errorf("备用加密 DNS 与节点域名解析各最多 %d 个", maxDNSEndpoints)
	}
	for _, raw := range fallback {
		if _, err := validDoH(raw); err != nil {
			return fmt.Errorf("备用加密 DNS 需要 HTTPS DoH 地址：%w", err)
		}
	}
	for _, raw := range bootstrap {
		d, err := validDoH(raw)
		if err != nil {
			return fmt.Errorf("节点域名解析需要 HTTPS DoH 地址：%w", err)
		}
		if net.ParseIP(d.Hostname()) == nil {
			return errors.New("节点域名解析的 DoH 地址必须使用数字 IP，例如 https://223.5.5.5/dns-query；它在没有其他解析器时运行")
		}
	}
	return nil
}

func loadDNSOptions(dir string) (DNSOptions, error) {
	return LoadDNSOptionsFile(filepath.Join(dir, DNSOptionsFile))
}

// LoadDNSOptionsFile reads options saved by SaveDNSOptionsFile; a missing file
// is the default.
func LoadDNSOptionsFile(path string) (DNSOptions, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return DNSOptions{}, nil
	}
	if err != nil {
		return DNSOptions{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return DNSOptions{}, errors.New("DNS 设置文件无效")
	}
	f, err := os.Open(path)
	if err != nil {
		return DNSOptions{}, err
	}
	defer f.Close()
	var file dnsOptionsFile
	d := json.NewDecoder(io.LimitReader(f, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&file); err != nil {
		return DNSOptions{}, errors.New("DNS 设置文件无效")
	}
	o := NewDNSOptions(file.FallbackURLs, file.BootstrapURLs, file.PublicFallback)
	return o, o.Validate()
}

// SaveDNSOptions writes the options beside configPath, removing the file when
// every option is at its default so a default profile has no extra file.
func SaveDNSOptions(configPath string, o DNSOptions) error {
	return SaveDNSOptionsFile(filepath.Join(filepath.Dir(configPath), DNSOptionsFile), o)
}

// SaveDNSOptionsFile writes o to path atomically, removing path for defaults.
func SaveDNSOptionsFile(path string, o DNSOptions) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o == (DNSOptions{}) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(dnsOptionsFile{FallbackURLs: o.FallbackURLs(), BootstrapURLs: o.BootstrapURLs(), PublicFallback: o.PublicFallback}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cage-dns-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = host.ProtectFile(f.Name()); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// DNSHost names an endpoint in messages without its path, which can carry a
// personal resolver ID.
func DNSHost(raw string) string {
	if d, err := url.Parse(raw); err == nil && d.Hostname() != "" {
		return d.Hostname()
	}
	return "DoH"
}
