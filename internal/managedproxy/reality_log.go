package managedproxy

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"go.yaml.in/yaml/v3"
)

// RealityHopMarker prefixes the proxy log record that names which hop of the
// applied route failed REALITY authentication. Only a fixed role follows it, so
// it survives diagnostic redaction while node names and addresses do not.
const RealityHopMarker = "cage-reality-hop="

const (
	RealityHopSingle = "single"
	RealityHopRelay  = "relay"
	RealityHopExit   = "exit"
)

const realityFailure = `reality\b[\s:]*(?:authentication|auth|verification)\s+failed`

var (
	realityFailureLine = regexp.MustCompile(`(?i)\b` + realityFailure)
	// Mihomo's outbounds wrap dial failures as "<server:port> connect error:
	// <cause>"; through dialer-proxy the relay's endpoint is the innermost one.
	realityFailedEndpoint = regexp.MustCompile(`(?i)(\S+) connect error:\s*` + realityFailure)
)

// RealityAuthFailed reports whether a proxy core log line records a failed
// REALITY handshake with a proxy server. It also matches redacted records.
func RealityAuthFailed(line string) bool {
	return realityFailureLine.MatchString(line)
}

// CoreLogStream records the core's output under the "proxy" source. Before
// redaction it maps the endpoint of a REALITY failure to its role in the
// applied route, so a later probe can name the hop without the address.
func CoreLogStream(store *diagnostics.Store, profile, level string) *diagnostics.Stream {
	runtimePath := filepath.Join(Dir(profile), "config.yaml")
	return diagnostics.NewStreamWithConsumer(store, "proxy", level, nil, func(line string) bool {
		if !RealityAuthFailed(line) {
			return false
		}
		if hop := realityFailedHop(line, runtimePath); hop != "" {
			_ = store.Record("warn", "proxy", RealityHopMarker+hop)
		}
		return false
	})
}

// realityFailedHop returns "" unless the failed endpoint belongs to exactly one
// hop of the selected route.
func realityFailedHop(line, runtimePath string) string {
	match := realityFailedEndpoint.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	route := runtimeRouteEndpoints(runtimePath)
	failed, found := -1, 0
	for i, endpoint := range route {
		if strings.EqualFold(endpoint, match[1]) {
			failed, found = i, found+1
		}
	}
	switch {
	case found != 1:
		return ""
	case len(route) == 1:
		return RealityHopSingle
	case failed == len(route)-1:
		return RealityHopExit
	default:
		return RealityHopRelay
	}
}

// runtimeRouteEndpoints lists the server endpoints from entry to exit of the
// group's selected node, following dialer-proxy as the core does.
func runtimeRouteEndpoints(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var config struct {
		Proxies []struct {
			Name   string `yaml:"name"`
			Server string `yaml:"server"`
			Port   any    `yaml:"port"`
			Dialer string `yaml:"dialer-proxy"`
		} `yaml:"proxies"`
		Groups []struct {
			Name    string   `yaml:"name"`
			Proxies []string `yaml:"proxies"`
		} `yaml:"proxy-groups"`
	}
	if yaml.Unmarshal(data, &config) != nil {
		return nil
	}
	selected := ""
	for _, group := range config.Groups {
		if group.Name == Group && len(group.Proxies) == 1 {
			selected = group.Proxies[0]
		}
	}
	var reversed []string
	seen := map[string]bool{}
	for name := selected; name != "" && !seen[name]; {
		seen[name] = true
		next := ""
		for _, proxy := range config.Proxies {
			if proxy.Name == name {
				reversed = append(reversed, net.JoinHostPort(strings.Trim(proxy.Server, "[]"), fmt.Sprint(proxy.Port)))
				next = proxy.Dialer
				break
			}
		}
		name = next
	}
	route := make([]string, len(reversed))
	for i, endpoint := range reversed {
		route[len(reversed)-1-i] = endpoint
	}
	return route
}
