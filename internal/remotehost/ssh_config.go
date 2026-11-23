package remotehost

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SSHConfigPreview contains only connection metadata, never config text or keys.
type SSHConfigPreview struct {
	Hosts    []HostProfile   `json:"hosts"`
	Skipped  []SSHConfigSkip `json:"skipped"`
	Revision string          `json:"revision"`
}

type SSHConfigSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type sshDirective struct {
	patterns []string
	key      string
	values   []string
}

const sshConfigMaxBytes = 1 << 20

// ReadSSHConfig reads only the current user's .ssh/config. In particular it
// never opens IdentityFile, runs ssh -G (which may execute Match exec), or
// copies configuration into the guest or the project's shared storage.
func ReadSSHConfig() (SSHConfigPreview, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return SSHConfigPreview{}, failure("ssh_config_read", "无法确定本机用户目录")
	}
	f, err := openSSHConfig(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return SSHConfigPreview{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > sshConfigMaxBytes {
		return SSHConfigPreview{}, failure("ssh_config_invalid", "SSH 配置必须是不超过 1 MiB 的普通文件")
	}
	return parseSSHConfig(f, localSSHUser())
}

func localSSHUser() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("USERNAME")
	}
	if account, err := user.Current(); err == nil && account.Username != "" {
		return account.Username
	}
	return os.Getenv("USER")
}

// sshConfigWords implements the subset of OpenSSH's quoting needed here.
// Backslashes in Windows paths stay literal; only an escaped double quote is
// unescaped. '=' separates the keyword from its arguments, not path values.
// Cage also accepts unquoted inline # comments as an import convenience.
func sshConfigWords(line string) ([]string, bool) {
	var words []string
	var word strings.Builder
	quoted, started := false, false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' && i+1 < len(line) && line[i+1] == '"' {
			word.WriteByte('"')
			started = true
			i++
		} else if c == '"' {
			quoted = !quoted
			started = true
		} else if !quoted && c == '#' {
			break
		} else if !quoted && (c == ' ' || c == '\t' || (c == '=' && len(words) == 0)) {
			flush()
			// A single optional '=' may follow whitespace after the keyword.
			if c != '=' && len(words) == 1 {
				j := i + 1
				for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
					j++
				}
				if j < len(line) && line[j] == '=' {
					i = j
				}
			}
		} else {
			word.WriteByte(c)
			started = true
		}
	}
	flush()
	return words, !quoted
}

// OpenSSH Host patterns use only '*' and '?'; filepath.Match would interpret
// brackets and platform path separators differently. Matching is ASCII-folded.
func sshHostMatch(pattern, name string) bool {
	pattern, name = strings.ToLower(pattern), strings.ToLower(name)
	p, n, star, retry := 0, 0, -1, 0
	for n < len(name) {
		if p < len(pattern) && (pattern[p] == '?' || pattern[p] == name[n]) {
			p++
			n++
			continue
		}
		if p < len(pattern) && pattern[p] == '*' {
			star = p
			p++
			retry = n
			continue
		}
		if star < 0 {
			return false
		}
		retry++
		n = retry
		p = star + 1
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

func sshHostApplies(patterns []string, name string) bool {
	matched := false
	for _, pattern := range patterns {
		negative := strings.HasPrefix(pattern, "!")
		if sshHostMatch(strings.TrimPrefix(pattern, "!"), name) {
			if negative {
				return false
			}
			matched = true
		}
	}
	return matched
}

func parseSSHConfig(reader io.Reader, defaultUser string) (SSHConfigPreview, error) {
	result := SSHConfigPreview{Hosts: []HostProfile{}, Skipped: []SSHConfigSkip{}}
	data, err := io.ReadAll(io.LimitReader(reader, sshConfigMaxBytes+1))
	if err != nil || len(data) > sshConfigMaxBytes || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return result, failure("ssh_config_invalid", "SSH 配置过大或不是有效的 UTF-8 文本")
	}
	defer clear(data)
	// Include the implicit username so a changed default invalidates the preview.
	digest := sha256.New()
	digest.Write(data)
	digest.Write([]byte("\x00" + defaultUser))
	result.Revision = hex.EncodeToString(digest.Sum(nil))
	var directives []sshDirective
	var aliases []string
	seen := map[string]bool{}
	patterns := []string{"*"}
	scanner := bufio.NewScanner(strings.NewReader(strings.TrimPrefix(string(data), "\ufeff")))
	scanner.Buffer(make([]byte, 4096), 65536)
	for scanner.Scan() {
		words, ok := sshConfigWords(strings.TrimSuffix(scanner.Text(), "\r"))
		if !ok {
			return result, failure("ssh_config_invalid", "SSH 配置引号未闭合")
		}
		if len(words) == 0 {
			continue
		}
		key := strings.ToLower(words[0])
		if len(words) < 2 || !validSSHConfigKeyword(key) || hasControl(strings.Join(words, "")) {
			return result, failure("ssh_config_invalid", "SSH 配置指令格式无效")
		}
		if key == "include" || key == "match" {
			// These can change subsequent Host scopes and first-value precedence.
			// Refuse the whole preview rather than silently import a wrong target.
			return result, failure("ssh_config_unsupported", "暂不支持包含 Include 或 Match 的 SSH 配置，请手动添加主机；不会执行配置中的命令")
		}
		if key == "host" {
			if len(words) > 129 {
				return result, failure("ssh_config_limit", "单个 Host 段的匹配规则过多")
			}
			patterns = append([]string(nil), words[1:]...)
			for i := range patterns {
				patterns[i] = strings.TrimSpace(patterns[i])
			}
			for _, alias := range patterns {
				if len(alias) > 253 {
					return result, failure("ssh_config_limit", "SSH 主机别名或匹配规则过长")
				}
				if strings.ContainsAny(alias, "*?!") {
					continue
				}
				lower := strings.ToLower(alias)
				if !seen[lower] {
					seen[lower] = true
					aliases = append(aliases, alias)
				}
			}
			if len(aliases) > 100 {
				return result, failure("ssh_config_limit", "SSH 配置最多导入 100 个主机别名")
			}
			continue
		}
		directives = append(directives, sshDirective{patterns, key, words[1:]})
		if len(directives) > 8192 {
			return result, failure("ssh_config_limit", "SSH 配置指令过多")
		}
	}
	if scanner.Err() != nil {
		return result, failure("ssh_config_invalid", "SSH 配置行过长或无法读取")
	}
	for _, alias := range aliases {
		values := map[string][]string{}
		var keys []string
		for _, d := range directives {
			if values[d.key] == nil && sshHostApplies(d.patterns, alias) {
				values[d.key] = d.values
				keys = append(keys, d.key)
			}
		}
		profile, reason := sshConfigProfile(alias, defaultUser, keys, values)
		if reason != "" {
			result.Skipped = append(result.Skipped, SSHConfigSkip{alias, reason})
			continue
		}
		result.Hosts = append(result.Hosts, profile)
	}
	return result, nil
}

// OpenSSH option names are ASCII words. Validate before using them in a reason.
func validSSHConfigKeyword(key string) bool {
	if len(key) == 0 || len(key) > 64 {
		return false
	}
	for i, c := range key {
		if !(c >= 'a' && c <= 'z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func sshConfigProfile(alias, defaultUser string, keys []string, values map[string][]string) (HostProfile, string) {
	get := func(key, fallback string) string {
		if value := values[key]; len(value) == 1 {
			return value[0]
		}
		return fallback
	}
	// Ignore only options whose absence is explicit in the import dialog. Never
	// silently turn an unsupported proxy/authentication route into a direct one.
	for _, key := range keys {
		args := values[key]
		switch key {
		case "hostname", "user", "port", "identityagent", "identitiesonly":
			if len(args) != 1 {
				return HostProfile{}, "SSH 配置参数数量无效"
			}
		case "identityfile", "addkeystoagent", "forwardagent", "forwardx11", "forwardx11trusted",
			"localforward", "remoteforward", "dynamicforward", "requesttty", "serveraliveinterval",
			"serveralivecountmax", "tcpkeepalive", "loglevel", "compression", "sendenv", "setenv",
			"stricthostkeychecking", "userknownhostsfile", "globalknownhostsfile", "hashknownhosts",
			"connecttimeout", "controlmaster", "controlpath", "controlpersist", "updatehostkeys",
			"checkhostip", "ignoreunknown":
		case "proxycommand", "proxyjump":
			if len(args) != 1 || !strings.EqualFold(args[0], "none") {
				return HostProfile{}, "依赖代理、跳板机或主机名规范化，请手动配置"
			}
		case "canonicalizehostname", "addressfamily", "pubkeyauthentication":
			compatible := "no"
			switch key {
			case "addressfamily":
				compatible = "any"
			case "pubkeyauthentication":
				compatible = "yes"
			}
			if len(args) != 1 || !strings.EqualFold(args[0], compatible) {
				return HostProfile{}, "Cage 无法保留此连接设置：" + key
			}
		case "preferredauthentications":
			if len(args) != 1 || !strings.Contains(","+strings.ToLower(args[0])+",", ",publickey,") {
				return HostProfile{}, "PreferredAuthentications 未包含 publickey，请手动选择认证方式"
			}
		default:
			return HostProfile{}, "包含暂不支持的连接选项：" + key
		}
	}
	if !strings.EqualFold(get("identitiesonly", "no"), "no") {
		return HostProfile{}, "IdentitiesOnly 无法通过导入保留，请手动选择认证方式"
	}
	name := get("hostname", alias)
	// HostName supports %h and %%; reject every other expansion literally.
	var expanded strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] != '%' {
			expanded.WriteByte(name[i])
			continue
		}
		i++
		if i == len(name) {
			return HostProfile{}, "主机名包含不支持的变量"
		}
		switch name[i] {
		case 'h':
			expanded.WriteString(alias)
		case '%':
			expanded.WriteByte('%')
		default:
			return HostProfile{}, "主机名包含不支持的变量"
		}
	}
	port, err := strconv.Atoi(strings.TrimSpace(get("port", "22")))
	if err != nil || port < 1 || port > 65535 {
		return HostProfile{}, "SSH 端口无效"
	}
	socket := strings.TrimSpace(get("identityagent", ""))
	if values["identityagent"] != nil && socket == "" {
		return HostProfile{}, "IdentityAgent 不能为空，请手动选择认证方式"
	}
	if socket == "SSH_AUTH_SOCK" {
		socket = ""
	}
	if strings.Contains(socket, "%") || strings.Contains(socket, "${") {
		return HostProfile{}, "IdentityAgent 不支持变量展开，请填写完整的本机路径"
	}
	if socket != "" && !validAgentSocket(runtime.GOOS, socket) {
		if runtime.GOOS == "windows" {
			return HostProfile{}, "IdentityAgent 需要使用本机 Windows 命名管道"
		}
		return HostProfile{}, "IdentityAgent 需要使用符合系统长度限制的 Unix 套接字绝对路径"
	}
	p := HostProfile{Name: alias, Host: expanded.String(), Port: port, User: get("user", defaultUser), Auth: AuthConfig{Kind: "agent", AgentSocket: socket}}
	normalized, err := normalizeProfile(p)
	if err != nil {
		return HostProfile{}, "主机名、用户名或端口不符合 Cage 连接要求"
	}
	return normalized, ""
}

// ImportSSHHosts atomically appends agent-only profiles. Existing aliases and
// endpoints are preserved, including their host-key pins and active sessions.
func (s *Store) ImportSSHHosts(in []HostProfile) ([]HostProfile, error) {
	if len(in) == 0 || len(in) > 100 {
		return nil, failure("ssh_config_selection", "请选择 1 至 100 个 SSH 主机")
	}
	normalized := make([]HostProfile, 0, len(in))
	for _, p := range in {
		if p.ID != "" || p.Auth.Kind != "agent" || p.Auth.IdentityFile != "" || p.HostKey != nil {
			return nil, failure("ssh_config_selection", "导入只接受未保存的 SSH agent 主机")
		}
		profile, err := normalizeProfile(p)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, profile)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles, err := s.read()
	if err != nil {
		return nil, err
	}
	added := []HostProfile{}
	for _, p := range normalized {
		exists := false
		for _, old := range profiles {
			if strings.EqualFold(old.Name, p.Name) || (strings.EqualFold(old.Host, p.Host) && old.Port == p.Port && old.User == p.User) {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		p.ID = newID()
		profiles = append(profiles, p)
		added = append(added, p)
	}
	if len(profiles) > 100 {
		return nil, failure("profile_limit", "最多保存 100 台远程主机，现有配置未修改")
	}
	if len(added) == 0 {
		return added, nil
	}
	return added, s.write(profiles)
}
