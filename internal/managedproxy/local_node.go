package managedproxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// ErrLocalProxyNode distinguishes a saved route that needs migration from
// an unconfigured or temporarily unavailable upstream.
var ErrLocalProxyNode = errors.New("本机代理端口不能用作节点。请导入原客户端的具体节点，在 Cage 中按中转到最终出口的顺序重新配置并应用")

type localServers struct {
	hostname  string
	addresses map[netip.Addr]bool
}

func newLocalServers() localServers {
	host, _ := os.Hostname()
	return localServers{hostname: strings.ToLower(strings.TrimSuffix(host, "."))}
}

func (local *localServers) contains(server string) bool {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(server), "."))
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "localhost.localdomain" || host == "ip6-localhost" || host == "ip6-loopback" {
		return true
	}
	if local.hostname != "" {
		short, _, _ := strings.Cut(local.hostname, ".")
		if host == local.hostname || host == short || host == short+".local" {
			return true
		}
	}
	ip, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if err != nil {
		ip = legacyIPv4(host)
		if !ip.IsValid() {
			return false
		}
	}
	ip = ip.WithZone("").Unmap()
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	if local.addresses == nil {
		local.addresses = make(map[netip.Addr]bool)
		addrs, _ := net.InterfaceAddrs()
		for _, addr := range addrs {
			if prefix, err := netip.ParsePrefix(addr.String()); err == nil {
				local.addresses[prefix.Addr().Unmap()] = true
			}
		}
	}
	return local.addresses[ip]
}

// Some clients accept the inet_aton notation (127.1, hexadecimal, or one
// integer). Treat those spellings as the same address during import.
func legacyIPv4(host string) netip.Addr {
	if len(host) == 0 || host[0] < '0' || host[0] > '9' {
		return netip.Addr{}
	}
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return netip.Addr{}
	}
	var value uint32
	for i, part := range parts {
		bits := 8
		if i == len(parts)-1 {
			bits = 32 - i*8
		}
		octet, err := strconv.ParseUint(part, 0, bits)
		if err != nil || strings.ContainsAny(part, "_+") {
			return netip.Addr{}
		}
		if i == len(parts)-1 {
			value |= uint32(octet)
		} else {
			value |= uint32(octet) << (24 - 8*i)
		}
	}
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], value)
	return netip.AddrFrom4(raw)
}

// BlockedNodes also covers imported dialer dependencies. Retain the original
// records and IDs so that a user can inspect and replace their saved routes.
func (s Store) BlockedNodes() map[string]string {
	local := newLocalServers()
	blocked := map[string]string{}
	dependents := map[string][]string{}
	var queue []string
	for _, src := range s.Sources {
		for _, n := range src.Nodes {
			server, _ := n.Config["server"].(string)
			if local.contains(server) {
				blocked[n.ID] = ErrLocalProxyNode.Error()
				queue = append(queue, n.ID)
			}
			if via, _ := n.Config["dialer-proxy"].(string); via != "" {
				dependents[via] = append(dependents[via], n.ID)
			}
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, child := range dependents[id] {
			if blocked[child] == "" {
				blocked[child] = "原代理链包含本机代理端口。请导入各跳的具体节点后重建代理链"
				queue = append(queue, child)
			}
		}
	}
	return blocked
}

func (s Store) checkRouteNodes(route []string, blocked map[string]string) ([]string, error) {
	for _, id := range route {
		if blocked[id] != "" {
			n, _ := s.Find(id)
			return route, fmt.Errorf("节点 %q 需要迁移：%w", n.Name, ErrLocalProxyNode)
		}
	}
	return route, nil
}
