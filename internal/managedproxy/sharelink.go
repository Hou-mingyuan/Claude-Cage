package managedproxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// shareLinkNodes converts a share-link subscription into Mihomo proxy maps.
// The body is either newline-separated links or the same text in Base64, as
// served by v2rayN-style subscriptions. It returns ok=false when the content is
// not a share-link list, so the YAML error stays the reported one.
//
// Each entry that cannot be mapped without guessing is returned with a fixed
// reason. Reasons and names never contain link text, credentials or servers.
func shareLinkNodes(data []byte) (nodes []map[string]any, reasons []string, ok bool) {
	text, ok := shareLinkText(data)
	if !ok {
		return nil, nil, false
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := parseShareLink(line)
		if err != nil {
			// Keep the entry so the import report counts it; only the protocol
			// and a name taken from the fragment are shown.
			p = map[string]any{"name": shareLinkName(line), "type": shareLinkScheme(line)}
			reasons = append(reasons, err.Error())
		} else {
			reasons = append(reasons, "")
		}
		nodes = append(nodes, p)
	}
	return nodes, reasons, len(nodes) > 0
}

// shareLinkText returns the decoded link list when every non-empty line looks
// like scheme://. A single Base64 blob is decoded first.
func shareLinkText(data []byte) (string, bool) {
	text := strings.TrimPrefix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\ufeff")
	if looksLikeLinks(text) {
		return text, true
	}
	compact := strings.Join(strings.Fields(text), "")
	if compact == "" || len(compact) > MaxImport*2 {
		return "", false
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := enc.DecodeString(compact)
		if err != nil || !utf8.Valid(decoded) {
			continue
		}
		if s := strings.ReplaceAll(string(decoded), "\r\n", "\n"); looksLikeLinks(s) {
			return s, true
		}
	}
	return "", false
}

func looksLikeLinks(text string) bool {
	found := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		scheme, rest, ok := strings.Cut(line, "://")
		if !ok || rest == "" || scheme == "" || strings.ContainsAny(scheme, " \t:/") {
			return false
		}
		found = true
	}
	return found
}

func shareLinkScheme(line string) string {
	scheme, _, _ := strings.Cut(line, "://")
	scheme = strings.ToLower(scheme)
	if len(scheme) > 16 {
		return ""
	}
	return scheme
}

// shareLinkName is the URL fragment, the only part of a link meant for display.
func shareLinkName(line string) string {
	before, fragment, ok := strings.Cut(line, "#")
	if !ok {
		// Shadowrocket links carry the display name in remarks instead.
		_, rawQuery, _ := strings.Cut(before, "?")
		name, _ := rawQueryValue(rawQuery, "remarks")
		return strings.TrimSpace(name)
	}
	if name, err := url.PathUnescape(fragment); err == nil {
		fragment = name
	}
	return strings.TrimSpace(fragment)
}

var errShareLinkUnsupported = errors.New("分享链接的协议暂不支持；请改用 Clash/Mihomo YAML")

// The bundled core's masque type needs an ECDSA client private key and the
// server public key, which MASQUE share links do not carry.
var errShareLinkMASQUE = errors.New("MASQUE 分享链接暂不支持：随附 Mihomo 核心的 masque 节点需要 ECDSA 客户端私钥和服务器公钥，这类链接只提供用户认证和 HTTP/3 传输参数，无法转换。Cage 目前也不能导入 MASQUE 节点，请向服务商获取其他协议的节点")

func parseShareLink(line string) (map[string]any, error) {
	switch shareLinkScheme(line) {
	case "trojan":
		return parseTrojanLink(line)
	case "vless":
		return parseVLESSLink(line)
	case "vmess":
		return parseVMessLink(line)
	case "ss":
		return parseShadowsocksLink(line)
	case "hysteria2", "hy2":
		return parseHysteria2Link(line)
	case "tuic":
		return parseTUICLink(line)
	case "anytls":
		return parseAnyTLSLink(line)
	case "masque":
		return nil, errShareLinkMASQUE
	default:
		return nil, errShareLinkUnsupported
	}
}

var errShareLinkInvalid = errors.New("分享链接格式无效，缺少服务器、端口或凭据")

// linkBase parses the common user@host:port#name shape.
func linkBase(line, typ string) (*url.URL, map[string]any, error) {
	u, err := url.Parse(line)
	if err != nil || u.Hostname() == "" || u.User == nil {
		return nil, nil, errShareLinkInvalid
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, nil, errShareLinkInvalid
	}
	name := strings.TrimSpace(u.Fragment)
	if name == "" {
		name = typ + " " + net.JoinHostPort(u.Hostname(), u.Port())
	}
	return u, map[string]any{"name": name, "type": typ, "server": u.Hostname(), "port": port}, nil
}

func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func setString(p map[string]any, key, value string) {
	if value != "" {
		p[key] = value
	}
}

func commaList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// applyTransport maps the v2rayN transport fields shared by trojan and vless.
func applyTransport(p map[string]any, network, path, host, service, header string) error {
	if header != "" && header != "none" {
		return errors.New("分享链接的伪装头类型暂不支持；请改用 Clash/Mihomo YAML")
	}
	switch network {
	case "", "tcp":
	case "ws":
		p["network"] = "ws"
		opts := map[string]any{}
		setString(opts, "path", path)
		if host != "" {
			opts["headers"] = map[string]any{"Host": host}
		}
		p["ws-opts"] = opts
	case "grpc":
		p["network"] = "grpc"
		opts := map[string]any{}
		setString(opts, "grpc-service-name", service)
		p["grpc-opts"] = opts
	case "h2", "http":
		p["network"] = "h2"
		opts := map[string]any{}
		setString(opts, "path", path)
		if hosts := commaList(host); len(hosts) > 0 {
			opts["host"] = hosts
		}
		p["h2-opts"] = opts
	default:
		return errShareLinkTransport
	}
	return nil
}

var errShareLinkTransport = errors.New("分享链接的传输方式暂不支持；请改用 Clash/Mihomo YAML")

func parseTrojanLink(line string) (map[string]any, error) {
	u, p, err := linkBase(line, "trojan")
	if err != nil {
		return nil, err
	}
	password := u.User.Username()
	if password == "" {
		return nil, errShareLinkInvalid
	}
	q := u.Query()
	p["password"] = password
	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("peer")
	}
	setString(p, "sni", sni)
	if truthy(q.Get("allowInsecure")) || truthy(q.Get("insecure")) {
		p["skip-cert-verify"] = true
	}
	if alpn := commaList(q.Get("alpn")); len(alpn) > 0 {
		p["alpn"] = alpn
	}
	setString(p, "client-fingerprint", q.Get("fp"))
	if security := q.Get("security"); security != "" && security != "tls" {
		return nil, errors.New("Trojan 分享链接的安全层暂不支持；请改用 Clash/Mihomo YAML")
	}
	if err := applyTransport(p, q.Get("type"), q.Get("path"), q.Get("host"), q.Get("serviceName"), q.Get("headerType")); err != nil {
		return nil, err
	}
	return p, nil
}

var errVLESSEncryption = errors.New("VLESS 分享链接的 encryption 暂不支持；请改用 Clash/Mihomo YAML")

func parseVLESSLink(line string) (map[string]any, error) {
	_, rest, _ := strings.Cut(line, "://")
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	authority, _, _ := strings.Cut(rest, "/")
	if !strings.Contains(authority, "@") {
		return parseShadowrocketVLESS(line)
	}
	u, p, err := linkBase(line, "vless")
	if err != nil {
		return nil, err
	}
	uuid := u.User.Username()
	if uuid == "" {
		return nil, errShareLinkInvalid
	}
	q := u.Query()
	if enc := q.Get("encryption"); enc != "" && enc != "none" {
		return nil, errVLESSEncryption
	}
	p["uuid"] = uuid
	setString(p, "flow", q.Get("flow"))
	switch q.Get("security") {
	case "", "none":
	case "tls":
		p["tls"] = true
	case "reality":
		p["tls"] = true
		if q.Get("pbk") == "" {
			return nil, errors.New("VLESS Reality 分享链接缺少公钥")
		}
		opts := map[string]any{"public-key": q.Get("pbk")}
		setString(opts, "short-id", q.Get("sid"))
		p["reality-opts"] = opts
	default:
		return nil, errors.New("VLESS 分享链接的安全层暂不支持；请改用 Clash/Mihomo YAML")
	}
	setString(p, "servername", q.Get("sni"))
	setString(p, "client-fingerprint", q.Get("fp"))
	if alpn := commaList(q.Get("alpn")); len(alpn) > 0 {
		p["alpn"] = alpn
	}
	if truthy(q.Get("allowInsecure")) || truthy(q.Get("insecure")) {
		p["skip-cert-verify"] = true
	}
	if err := applyTransport(p, q.Get("type"), q.Get("path"), q.Get("host"), q.Get("serviceName"), q.Get("headerType")); err != nil {
		return nil, err
	}
	return p, nil
}

var errVMessFormat = errors.New("VMess 分享链接不是 v2rayN 格式；请改用 Clash/Mihomo YAML")

// parseVMessLink reads the v2rayN form vmess://Base64(JSON), then
// Shadowrocket's vmess://Base64(cipher:uuid@server:port)?params.
func parseVMessLink(line string) (map[string]any, error) {
	encoded := strings.TrimSpace(line[len("vmess://"):])
	var raw []byte
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := enc.DecodeString(encoded); err == nil {
			raw = decoded
			break
		}
	}
	if raw == nil {
		return parseShadowrocketVMess(line)
	}
	var v struct {
		PS, Add, ID, Scy, Net, Type, Host, Path, TLS, SNI, ALPN, FP string
		Port, AID                                                   json.RawMessage
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&v) != nil {
		return parseShadowrocketVMess(line)
	}
	port, portErr := jsonInt(v.Port)
	aid, aidErr := jsonInt(v.AID)
	if v.Add == "" || v.ID == "" || portErr != nil || port < 1 || port > 65535 || (len(v.AID) > 0 && aidErr != nil) {
		return nil, errShareLinkInvalid
	}
	name := strings.TrimSpace(v.PS)
	if name == "" {
		name = "vmess " + net.JoinHostPort(v.Add, strconv.Itoa(port))
	}
	cipher := v.Scy
	if cipher == "" {
		cipher = "auto"
	}
	p := map[string]any{"name": name, "type": "vmess", "server": v.Add, "port": port, "uuid": v.ID, "alterId": aid, "cipher": cipher}
	switch v.TLS {
	case "", "none":
	case "tls":
		p["tls"] = true
		setString(p, "servername", v.SNI)
		setString(p, "client-fingerprint", v.FP)
		if alpn := commaList(v.ALPN); len(alpn) > 0 {
			p["alpn"] = alpn
		}
	default:
		return nil, errors.New("VMess 分享链接的安全层暂不支持；请改用 Clash/Mihomo YAML")
	}
	service, header := "", v.Type
	if v.Net == "grpc" {
		service = v.Path
		// For gRPC, v2rayN's type is the transport mode, not a disguise
		// header: "gun" (the default) or "multi". The core speaks gun, which
		// multi-mode servers also accept.
		if header == "gun" || header == "multi" {
			header = ""
		}
	}
	if err := applyTransport(p, v.Net, v.Path, v.Host, service, header); err != nil {
		return nil, err
	}
	return p, nil
}

var errShadowrocketParam = errors.New("Shadowrocket 分享链接含有暂不支持的参数；请改用 Clash/Mihomo YAML")

// shadowrocketParams are the Shadowrocket parameters with a Mihomo
// equivalent. Any other parameter is rejected instead of dropped, because it
// may change how the node connects.
var shadowrocketParams = map[string]bool{
	"remarks": true, "obfs": true, "obfsParam": true, "path": true, "tls": true, "peer": true,
	"allowInsecure": true, "alpn": true, "fp": true, "udp": true, "tfo": true, "mux": true, "xtls": true,
}

// shadowrocketLink is the scheme://Base64(method:id@server:port)?params form
// Shadowrocket exports for VLESS and VMess. The name is in remarks, not in
// the fragment.
type shadowrocketLink struct {
	method, id string
	p          map[string]any
	q          url.Values
}

// parseShadowrocketLink returns ok=false when the encoded part does not decode
// to method:id@server:port, so the caller can report its own format error.
func parseShadowrocketLink(line, typ string, extra ...string) (*shadowrocketLink, bool, error) {
	_, rest, _ := strings.Cut(line, "://")
	rest, fragment, _ := strings.Cut(rest, "#")
	encoded, rawQuery, _ := strings.Cut(rest, "?")
	decoded, ok := decodeBase64(strings.TrimSuffix(encoded, "/"))
	if !ok {
		return nil, false, nil
	}
	at := strings.LastIndexByte(decoded, '@')
	if at < 0 {
		return nil, false, nil
	}
	method, id, ok := strings.Cut(decoded[:at], ":")
	if !ok {
		return nil, false, nil
	}
	host, portText, err := net.SplitHostPort(decoded[at+1:])
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || port < 1 || port > 65535 || method == "" || id == "" || host == "" ||
		strings.ContainsAny(id, ":@/?# \t\r\n") || strings.ContainsAny(host, "@/?# \t\r\n") {
		return nil, true, errShareLinkInvalid
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, true, errShareLinkInvalid
	}
	for k, v := range q {
		if len(v) != 1 || !(shadowrocketParams[k] || slices.Contains(extra, k)) {
			return nil, true, errShadowrocketParam
		}
	}
	name := ""
	if n, err := url.PathUnescape(fragment); err == nil {
		name = strings.TrimSpace(n)
	}
	if name == "" {
		name = strings.TrimSpace(q.Get("remarks"))
	}
	if name == "" {
		name = typ + " " + net.JoinHostPort(host, strconv.Itoa(port))
	}
	p := map[string]any{"name": name, "type": typ, "server": host, "port": port}
	return &shadowrocketLink{method: method, id: id, p: p, q: q}, true, nil
}

func shadowrocketBool(v string) (bool, bool) {
	switch strings.ToLower(v) {
	case "", "0", "false":
		return false, true
	case "1", "true":
		return true, true
	}
	return false, false
}

// apply maps the TLS, UDP and transport parameters. allowHTTP enables obfs=http,
// HTTP/1.1 header camouflage over TCP, which Cage maps only for VMess.
func (l *shadowrocketLink) apply(allowHTTP bool) error {
	flags := map[string]bool{}
	for _, k := range []string{"tls", "allowInsecure", "udp", "tfo", "mux", "xtls"} {
		v, ok := shadowrocketBool(l.q.Get(k))
		if !ok {
			return errShadowrocketParam
		}
		flags[k] = v
	}
	// Shadowrocket's mux and XTLS modes have no exact Mihomo equivalent.
	if flags["mux"] || flags["xtls"] {
		return errShadowrocketParam
	}
	p, q := l.p, l.q
	if flags["tls"] {
		p["tls"] = true
		setString(p, "servername", q.Get("peer"))
		if flags["allowInsecure"] {
			p["skip-cert-verify"] = true
		}
		if alpn := commaList(q.Get("alpn")); len(alpn) > 0 {
			p["alpn"] = alpn
		}
		setString(p, "client-fingerprint", q.Get("fp"))
	}
	// udp is only validated: import turns UDP off for every node.
	if flags["tfo"] {
		p["tfo"] = true
	}
	path, host := q.Get("path"), q.Get("obfsParam")
	// Newer Shadowrocket versions may write obfsParam as a JSON header map;
	// only a plain host name is mapped.
	if strings.ContainsAny(host, "{}\"' \t") {
		return errShadowrocketParam
	}
	switch q.Get("obfs") {
	case "", "none", "tcp":
		return nil
	case "websocket", "ws":
		return applyTransport(p, "ws", path, host, "", "")
	case "grpc":
		return applyTransport(p, "grpc", "", "", path, "")
	case "h2":
		return applyTransport(p, "h2", path, host, "", "")
	case "http":
		if !allowHTTP {
			break
		}
		if path == "" {
			path = "/"
		}
		opts := map[string]any{"path": []string{path}}
		if hosts := commaList(host); len(hosts) > 0 {
			opts["headers"] = map[string]any{"Host": hosts}
		}
		p["network"], p["http-opts"] = "http", opts
		return nil
	}
	return errShareLinkTransport
}

func parseShadowrocketVLESS(line string) (map[string]any, error) {
	l, ok, err := parseShadowrocketLink(line, "vless")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errShareLinkInvalid
	}
	if l.method != "auto" && l.method != "none" {
		return nil, errVLESSEncryption
	}
	l.p["uuid"] = l.id
	if err := l.apply(false); err != nil {
		return nil, err
	}
	return l.p, nil
}

var vmessCiphers = map[string]bool{"auto": true, "none": true, "zero": true, "aes-128-gcm": true, "chacha20-poly1305": true}

func parseShadowrocketVMess(line string) (map[string]any, error) {
	l, ok, err := parseShadowrocketLink(line, "vmess", "alterId")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errVMessFormat
	}
	if !vmessCiphers[l.method] {
		return nil, errShadowrocketParam
	}
	aid := 0
	if v := l.q.Get("alterId"); v != "" {
		if aid, err = strconv.Atoi(v); err != nil || aid < 0 || aid > 65535 {
			return nil, errShareLinkInvalid
		}
	}
	l.p["uuid"], l.p["alterId"], l.p["cipher"] = l.id, aid, l.method
	if err := l.apply(true); err != nil {
		return nil, err
	}
	return l.p, nil
}

func jsonInt(raw json.RawMessage) (int, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

// parseShadowsocksLink reads SIP002 (ss://userinfo@host:port) and the legacy
// ss://Base64(method:password@host:port) form.
func parseShadowsocksLink(line string) (map[string]any, error) {
	body := line[len("ss://"):]
	fragment := ""
	if i := strings.IndexByte(body, '#'); i >= 0 {
		body, fragment = body[:i], body[i:]
	}
	if !strings.Contains(body, "@") {
		// Legacy form: the whole authority is Base64.
		query := ""
		if i := strings.IndexAny(body, "/?"); i >= 0 {
			body, query = body[:i], body[i:]
		}
		decoded, ok := decodeBase64(body)
		if !ok {
			return nil, errShareLinkInvalid
		}
		at := strings.LastIndexByte(decoded, '@')
		if at < 0 {
			return nil, errShareLinkInvalid
		}
		body = url.PathEscape(decoded[:at]) + "@" + decoded[at+1:] + query
	}
	u, p, err := linkBase("ss://"+body+fragment, "ss")
	if err != nil {
		return nil, err
	}
	method, password := "", ""
	if pass, hasPassword := u.User.Password(); hasPassword {
		method, password = u.User.Username(), pass
	} else if decoded, ok := decodeBase64(u.User.Username()); ok {
		method, password, _ = strings.Cut(decoded, ":")
	} else {
		method, password, _ = strings.Cut(u.User.Username(), ":")
	}
	if method == "" || password == "" {
		return nil, errShareLinkInvalid
	}
	p["cipher"], p["password"] = method, password
	plugin, err := rawQueryValue(u.RawQuery, "plugin")
	if err != nil {
		return nil, errors.New("Shadowsocks 分享链接的插件参数无效")
	}
	if plugin != "" {
		fields := strings.Split(plugin, ";")
		opts := map[string]string{}
		for _, field := range fields[1:] {
			k, v, _ := strings.Cut(field, "=")
			opts[k] = v
		}
		switch fields[0] {
		case "obfs-local", "simple-obfs":
			mode := opts["obfs"]
			if mode != "http" && mode != "tls" {
				return nil, errors.New("Shadowsocks 分享链接的 obfs 模式无效")
			}
			pluginOpts := map[string]any{"mode": mode}
			setString(pluginOpts, "host", opts["obfs-host"])
			p["plugin"], p["plugin-opts"] = "obfs", pluginOpts
		default:
			return nil, errors.New("Shadowsocks 分享链接的插件暂不支持；请改用 Clash/Mihomo YAML")
		}
	}
	return p, nil
}

// rawQueryValue returns the first value of key in a raw query. Unlike
// url.ParseQuery it keeps pairs whose value has bare semicolons, the way
// Shadowrocket and other clients export SIP002 plugin options
// (plugin=obfs-local;obfs=http), which ParseQuery drops silently.
func rawQueryValue(rawQuery, key string) (string, error) {
	for _, pair := range strings.Split(rawQuery, "&") {
		k, v, _ := strings.Cut(pair, "=")
		if k, err := url.QueryUnescape(k); err != nil || k != key {
			continue
		}
		return url.QueryUnescape(v)
	}
	return "", nil
}

func decodeBase64(s string) (string, bool) {
	if unescaped, err := url.PathUnescape(s); err == nil {
		s = unescaped
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := enc.DecodeString(s); err == nil && utf8.Valid(decoded) {
			return string(decoded), true
		}
	}
	return "", false
}

func parseHysteria2Link(line string) (map[string]any, error) {
	if strings.HasPrefix(strings.ToLower(line), "hy2://") {
		line = "hysteria2://" + line[len("hy2://"):]
	}
	line, ports, err := hysteria2LinkPorts(line)
	if err != nil {
		return nil, err
	}
	u, p, err := linkBase(line, "hysteria2")
	if err != nil {
		return nil, err
	}
	if ports != "" {
		p["ports"] = ports
	}
	password := u.User.Username()
	if pass, ok := u.User.Password(); ok {
		// Some clients write user:pass; Hysteria 2 treats the whole value as auth.
		password += ":" + pass
	}
	if password == "" {
		return nil, errShareLinkInvalid
	}
	q := u.Query()
	p["password"] = password
	setString(p, "sni", q.Get("sni"))
	if truthy(q.Get("insecure")) {
		p["skip-cert-verify"] = true
	}
	switch obfs := q.Get("obfs"); obfs {
	case "":
	case "salamander":
		p["obfs"] = obfs
		setString(p, "obfs-password", q.Get("obfs-password"))
	default:
		return nil, errors.New("Hysteria2 分享链接的混淆方式暂不支持")
	}
	setString(p, "mport", q.Get("mport"))
	setString(p, "pinSHA256", q.Get("pinSHA256"))
	return p, nil
}

// hysteria2LinkPorts handles the authority forms of the Hysteria 2 URI scheme
// that net/url rejects: an omitted port (443) and a multi-port list such as
// host:443,5000-6000. The link is returned with a single port (the first one)
// for linkBase, and the list, normalized, for the core's ports field.
func hysteria2LinkPorts(line string) (string, string, error) {
	scheme, rest, ok := strings.Cut(line, "://")
	if !ok {
		return line, "", errShareLinkInvalid
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority, tail := rest[:end], rest[end:]
	at := strings.LastIndexByte(authority, '@')
	userinfo, hostport := authority[:at+1], authority[at+1:]
	host, spec := hostport, ""
	if strings.HasPrefix(hostport, "[") {
		closing := strings.IndexByte(hostport, ']')
		if closing < 0 {
			return line, "", errShareLinkInvalid
		}
		host, spec = hostport[:closing+1], hostport[closing+1:]
		if spec != "" && !strings.HasPrefix(spec, ":") {
			return line, "", errShareLinkInvalid
		}
		spec = strings.TrimPrefix(spec, ":")
	} else if i := strings.IndexByte(hostport, ':'); i >= 0 {
		host, spec = hostport[:i], hostport[i+1:]
	}
	if spec == "" {
		return scheme + "://" + userinfo + host + ":443" + tail, "", nil
	}
	if !strings.ContainsAny(spec, ",-") {
		return line, "", nil
	}
	ports, err := hysteria2Ports(spec)
	if err != nil {
		return line, "", errShareLinkInvalid
	}
	first, _, _ := strings.Cut(ports, ",")
	first, _, _ = strings.Cut(first, "-")
	return scheme + "://" + userinfo + host + ":" + first + tail, ports, nil
}

func parseTUICLink(line string) (map[string]any, error) {
	u, p, err := linkBase(line, "tuic")
	if err != nil {
		return nil, err
	}
	password, ok := u.User.Password()
	if u.User.Username() == "" || !ok || password == "" {
		return nil, errors.New("TUIC 分享链接需要 uuid:password；TUIC v4 token 链接暂不支持")
	}
	q := u.Query()
	p["uuid"], p["password"] = u.User.Username(), password
	setString(p, "congestion-controller", q.Get("congestion_control"))
	setString(p, "udp-relay-mode", q.Get("udp_relay_mode"))
	setString(p, "sni", q.Get("sni"))
	if alpn := commaList(q.Get("alpn")); len(alpn) > 0 {
		p["alpn"] = alpn
	}
	if truthy(q.Get("allow_insecure")) || truthy(q.Get("insecure")) {
		p["skip-cert-verify"] = true
	}
	return p, nil
}

func parseAnyTLSLink(line string) (map[string]any, error) {
	u, p, err := linkBase(line, "anytls")
	if err != nil {
		return nil, err
	}
	password := u.User.Username()
	if password == "" {
		return nil, errShareLinkInvalid
	}
	q := u.Query()
	p["password"] = password
	setString(p, "sni", q.Get("sni"))
	setString(p, "client-fingerprint", q.Get("fp"))
	if alpn := commaList(q.Get("alpn")); len(alpn) > 0 {
		p["alpn"] = alpn
	}
	if truthy(q.Get("insecure")) || truthy(q.Get("allowInsecure")) {
		p["skip-cert-verify"] = true
	}
	return p, nil
}
