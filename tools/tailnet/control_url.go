package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// normalizeControlURL validates and canonicalizes an optional custom control
// server origin. Empty input (or the official controlplane origin) selects the
// default official Tailscale service.
func normalizeControlURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 256 {
		return "", errors.New("控制服务器地址过长")
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("控制服务器地址须为有效的 HTTPS 地址，例如 https://headscale.example.com")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("控制服务器地址只需填写协议、域名和端口，不要包含路径")
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" || strings.ContainsAny(host, " \t\r\n") {
		return "", errors.New("控制服务器地址缺少有效主机名")
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("控制服务器端口须在 1–65535 之间")
		}
		if n == 443 {
			port = ""
		} else {
			port = strconv.Itoa(n)
		}
	}
	if host == "controlplane.tailscale.com" && port == "" {
		return "", nil
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	return "https://" + authority, nil
}

// stateDir isolates each custom control server's tsnet node identity under
// <dir>/servers/<hash>, while keeping the official service directly in <dir>
// so existing installations keep their identity untouched.
func (n *node) stateDir() (string, error) {
	if n.config.ControlURL == "" {
		return n.dir, nil
	}
	sum := sha256.Sum256([]byte(n.config.ControlURL))
	serversRoot := filepath.Join(n.dir, "servers")
	if err := os.MkdirAll(serversRoot, 0700); err != nil {
		return "", err
	}
	if err := protectStateDir(serversRoot); err != nil {
		return "", err
	}
	dir := filepath.Join(serversRoot, hex.EncodeToString(sum[:16]))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := protectStateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}
