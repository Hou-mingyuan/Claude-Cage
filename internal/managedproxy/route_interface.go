package managedproxy

import (
	"net"
	"net/netip"
	"strings"
)

var (
	tailscaleIPv4Prefix = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleIPv6Prefix = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

type hostInterface struct {
	name     string
	prefixes []netip.Prefix
}

// Package-level hooks for deterministic cross-platform route selection tests.
var (
	listHostInterfaces = systemHostInterfaces
	probeRouteSourceIP = systemRouteSourceIP
)

type nodeInterfaceResolver struct {
	ifaces  []hostInterface
	sources map[netip.Addr]netip.Addr
	probed  map[netip.Addr]bool
}

func resolveNodeInterface(server, dialerProxy, physical string, resolver **nodeInterfaceResolver) string {
	// A chained hop dials through its upstream proxy rather than directly from
	// the host network stack, so host interface routing only applies to direct hops.
	if strings.TrimSpace(dialerProxy) != "" {
		return ""
	}
	dst, ok := parseLiteralServerIP(server)
	if !ok || !needsRouteInterfaceLookup(dst) {
		return ""
	}
	if *resolver == nil {
		ifaces, err := listHostInterfaces()
		if err != nil {
			ifaces = nil
		}
		*resolver = &nodeInterfaceResolver{
			ifaces:  ifaces,
			sources: map[netip.Addr]netip.Addr{},
			probed:  map[netip.Addr]bool{},
		}
	}
	r := *resolver
	if len(r.ifaces) == 0 {
		return ""
	}
	return selectNodeRouteInterface(dst, physical, r.ifaces, func(target netip.Addr) (netip.Addr, bool) {
		if r.probed[target] {
			src := r.sources[target]
			return src, src.IsValid()
		}
		r.probed[target] = true
		src, ok := probeRouteSourceIP(target)
		if ok && src.IsValid() {
			r.sources[target] = src
			return src, true
		}
		return netip.Addr{}, false
	})
}

func parseLiteralServerIP(server string) (netip.Addr, bool) {
	host := strings.TrimSpace(server)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		addr = legacyIPv4(host)
		if !addr.IsValid() {
			return netip.Addr{}, false
		}
	}
	addr = addr.WithZone("").Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsUnspecified() || addr.IsLinkLocalUnicast() || addr.IsMulticast() {
		return netip.Addr{}, false
	}
	return addr, true
}

func isTailscaleAddr(addr netip.Addr) bool {
	return tailscaleIPv4Prefix.Contains(addr) || tailscaleIPv6Prefix.Contains(addr)
}

func needsRouteInterfaceLookup(dst netip.Addr) bool {
	return isTailscaleAddr(dst) || dst.IsPrivate()
}

func selectNodeRouteInterface(dst netip.Addr, physical string, ifaces []hostInterface, routeSource func(netip.Addr) (netip.Addr, bool)) string {
	if !needsRouteInterfaceLookup(dst) {
		return ""
	}
	src, ok := routeSource(dst)
	if !ok || !src.IsValid() || src.IsLoopback() || src.IsUnspecified() {
		return ""
	}
	matched, found := findInterfaceByAddr(ifaces, src)
	if !found || matched.name == "" || matched.name == physical {
		return ""
	}
	// Tailscale/CGNAT peers (100.64.0.0/10 and fd7a:115c:a1e0::/48) typically
	// sit on a /32 or /128 tunnel interface, so validate that both destination
	// and chosen source address belong to the same tailnet range. A host VPN or
	// Clash TUN carrying a non-tailnet address (such as 198.18.0.1) is rejected
	// even if it owns the default route.
	if tailscaleIPv4Prefix.Contains(dst) && tailscaleIPv4Prefix.Contains(src) {
		return matched.name
	}
	if tailscaleIPv6Prefix.Contains(dst) && tailscaleIPv6Prefix.Contains(src) {
		return matched.name
	}
	// For RFC 1918 / ULA private addresses, only honor a non-physical interface
	// when the destination falls inside one of that interface's own directly
	// attached subnets (e.g. secondary LAN NIC or WireGuard/ZeroTier subnet),
	// never a 0.0.0.0/0 default-route capture.
	if dst.IsPrivate() {
		for _, pf := range matched.prefixes {
			if pf.Addr().Is4() == dst.Is4() && pf.Bits() > 0 && pf.Bits() < pf.Addr().BitLen() && pf.Contains(dst) {
				return matched.name
			}
		}
	}
	return ""
}

func findInterfaceByAddr(ifaces []hostInterface, addr netip.Addr) (hostInterface, bool) {
	for _, iface := range ifaces {
		for _, pf := range iface.prefixes {
			if pf.Addr() == addr {
				return iface, true
			}
		}
	}
	return hostInterface{}, false
}

func systemHostInterfaces() ([]hostInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]hostInterface, 0, len(ifaces))
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Name == "" {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		var prefixes []netip.Prefix
		for _, addr := range addrs {
			switch a := addr.(type) {
			case *net.IPNet:
				ip, ok := netip.AddrFromSlice(a.IP)
				if !ok {
					continue
				}
				ones, bits := a.Mask.Size()
				if bits == 32 {
					ip = ip.Unmap()
				}
				if pf := netip.PrefixFrom(ip.WithZone(""), ones); pf.IsValid() {
					prefixes = append(prefixes, pf)
				}
			case *net.IPAddr:
				ip, ok := netip.AddrFromSlice(a.IP)
				if !ok {
					continue
				}
				ip = ip.WithZone("").Unmap()
				if pf := netip.PrefixFrom(ip, ip.BitLen()); pf.IsValid() {
					prefixes = append(prefixes, pf)
				}
			}
		}
		if len(prefixes) > 0 {
			out = append(out, hostInterface{name: iface.Name, prefixes: prefixes})
		}
	}
	return out, nil
}

// systemRouteSourceIP queries the OS routing table (including Windows IP Helper
// routes, macOS scoped routes, and Linux policy routing tables such as
// Tailscale's table 52) without sending any packets on the wire.
func systemRouteSourceIP(dst netip.Addr) (netip.Addr, bool) {
	network := "udp4"
	if dst.Is6() {
		network = "udp6"
	}
	conn, err := net.DialUDP(network, nil, &net.UDPAddr{IP: dst.AsSlice(), Port: 9})
	if err != nil {
		return netip.Addr{}, false
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || local == nil {
		return netip.Addr{}, false
	}
	src, ok := netip.AddrFromSlice(local.IP)
	if !ok {
		return netip.Addr{}, false
	}
	return src.WithZone("").Unmap(), true
}
