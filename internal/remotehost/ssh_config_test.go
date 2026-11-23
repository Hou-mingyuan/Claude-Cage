package remotehost

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestSSHConfigAliasesDefaultsAndLocalAgent(t *testing.T) {
	socket := "/tmp/agent with spaces.sock"
	if runtime.GOOS == "windows" {
		socket = `\\.\pipe\custom-agent`
	}
	config := "\ufeff# test\r\n" + `Host dev prod !blocked *.example
  HostName="%h.example.com" # comment
  User "remote user"
  Port = 2222
  IdentityFile "C:\Users\person\.ssh\DO_NOT_COPY"
  IdentityAgent " ` + socket + ` "
Host DEV
  Port 9999
Host * !prod
  User fallback
  Port 2022
Host prod
  User later
Host ipv6
  HostName ::1
Host *
  User default
`
	p, err := parseSSHConfig(strings.NewReader(config), "local")
	if err != nil || len(p.Hosts) != 3 || len(p.Skipped) != 0 {
		t.Fatalf("preview = %+v, %v", p, err)
	}
	for _, h := range p.Hosts[:2] {
		if h.Port != 2222 || h.User != "remote user" || h.Host != h.Name+".example.com" || h.Auth.Kind != "agent" || h.Auth.IdentityFile != "" || h.Auth.AgentSocket != socket || h.ID != "" || h.HostKey != nil {
			t.Fatalf("imported wrong metadata: %+v", h)
		}
	}
	if h := p.Hosts[2]; h.Host != "::1" || h.User != "fallback" || h.Port != 2022 {
		t.Fatalf("defaults = %+v", h)
	}
	encoded, _ := json.Marshal(p)
	if strings.Contains(string(encoded), "DO_NOT_COPY") || len(p.Revision) != 64 {
		t.Fatalf("preview leaked config or has no revision: %s", encoded)
	}
	again, _ := parseSSHConfig(strings.NewReader(config), "local")
	changed, _ := parseSSHConfig(strings.NewReader(config+"# change\n"), "local")
	changedUser, _ := parseSSHConfig(strings.NewReader(config), "other")
	if p.Revision != again.Revision || p.Revision == changed.Revision || p.Revision == changedUser.Revision {
		t.Fatal("preview revision does not track the file and default username")
	}
}

func TestSSHConfigFirstValueAndNegatedWildcard(t *testing.T) {
	p, err := parseSSHConfig(strings.NewReader(`User global
Host a* !ab
  Port 2200
Host ab ac
  User late
Host ab
  HostName target.example
`), "local")
	if err != nil || len(p.Hosts) != 2 || p.Hosts[0].User != "global" || p.Hosts[0].Port != 22 || p.Hosts[1].Port != 2200 {
		t.Fatalf("first values/negation: %+v %v", p, err)
	}
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{{"A?*", "abc", true}, {"[ab]", "a", false}, {"a*b*c", "axybzbc", true}, {"a*b", "ac", false}} {
		if sshHostMatch(tc.pattern, tc.name) != tc.want {
			t.Errorf("pattern %q name %q", tc.pattern, tc.name)
		}
	}
}

func TestSSHConfigNeverExecutesOrReadsReferencedFiles(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	for _, config := range []string{
		"Match exec \"touch " + marker + "\"\n User bad\nHost a\n User me\n",
		"Host a\n User me\nInclude \"" + marker + "\"\n",
	} {
		_, err := parseSSHConfig(strings.NewReader(config), "local")
		var typed *Error
		if !errors.As(err, &typed) || typed.Code != "ssh_config_unsupported" || strings.Contains(err.Error(), marker) {
			t.Fatalf("unsafe/dynamic config = %v", err)
		}
	}
	p, err := parseSSHConfig(strings.NewReader("Host safe\n IdentityFile \""+marker+"\"\n"), "local")
	if err != nil || len(p.Hosts) != 1 || p.Hosts[0].Auth.IdentityFile != "" {
		t.Fatalf("IdentityFile was required/read: %+v %v", p, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("referenced command/file was touched")
	}
}

func TestSSHConfigSkipsUnsupportedRoutesAndAuthentication(t *testing.T) {
	for _, option := range []string{
		"ProxyCommand arbitrary-command SECRET", "ProxyJump jump", "CanonicalizeHostname yes",
		"IdentitiesOnly yes", "IdentityAgent none", `IdentityAgent ""`, `IdentityAgent \\server\pipe\agent`,
		"IdentityAgent ${SSH_AUTH_SOCK}", "HostName %n.example", "HostName ${TARGET}",
		"Port 0", "Port 65536", "Port nope", "Port 22 23", "CertificateFile key-cert.pub",
		"Hostname a..b", "RemoteCommand command SECRET", "PKCS11Provider library",
		"PubkeyAuthentication NO", "PreferredAuthentications password,keyboard-interactive",
		"AddressFamily inet6", "IdentityAgent /tmp/%h.sock", "IdentityAgent /tmp/${AGENT}/sock",
	} {
		p, err := parseSSHConfig(strings.NewReader("Host bad\n "+option+"\nHost safe\n"), "local")
		if err != nil || len(p.Skipped) != 1 || len(p.Hosts) != 1 || p.Hosts[0].Name != "safe" {
			t.Errorf("%s: %+v %v", option, p, err)
		}
		encoded, _ := json.Marshal(p)
		if strings.Contains(string(encoded), "SECRET") {
			t.Fatal("skipped reason exposed directive values")
		}
	}
}

func TestSSHConfigCommonDefaultsAndUnknownOptionReason(t *testing.T) {
	config := `Host " dev " dev
 HostName " %h.example "
 User " remote "
 Port " 2222 "
 ProxyCommand NONE
 ProxyJump NoNe
 CanonicalizeHostname No
 IdentitiesOnly nO
Host custom
 UnknownFirst SECRET_VALUE
 UnknownSecond SECOND_SECRET
Host *
 ConnectTimeout 10
 PubkeyAuthentication YES
 PreferredAuthentications PUBLICKEY,password
 ControlMaster auto
 ControlPath ~/.ssh/%C
 ControlPersist 5m
 UpdateHostKeys ask
 AddressFamily AnY
 CheckHostIP yes
 IgnoreUnknown UnknownFirst,UnknownSecond
`
	preview, err := parseSSHConfig(strings.NewReader(config), "local")
	if err != nil || len(preview.Hosts) != 1 || len(preview.Skipped) != 1 {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	host := preview.Hosts[0]
	if host.Name != "dev" || host.Host != "dev.example" || host.User != "remote" || host.Port != 2222 || host.Platform != "linux" || host.Executable != "cage" {
		t.Fatalf("preview did not return normalized fields: %+v", host)
	}
	reason := preview.Skipped[0].Reason
	if !strings.Contains(reason, "unknownfirst") || strings.Contains(strings.ToLower(reason), "secret") {
		t.Fatalf("unknown directive reason = %q", reason)
	}
}

func TestSSHConfigMalformedAndBoundedInput(t *testing.T) {
	for _, config := range []string{"Host \"open", "Host", "Host a\nUser\n", "Host a\nUser b\x00", string([]byte{0xff}), strings.Repeat("#", 65537), strings.Repeat("x", (1<<20)+1)} {
		if _, err := parseSSHConfig(strings.NewReader(config), "local"); err == nil {
			t.Errorf("accepted malformed config of length %d", len(config))
		}
	}
	var config strings.Builder
	for i := 0; i < 101; i++ {
		config.WriteString("Host " + newID() + "\n")
	}
	if _, err := parseSSHConfig(strings.NewReader(config.String()), "local"); err == nil {
		t.Fatal("unbounded alias enumeration")
	}
	for _, config := range []string{"Host " + strings.Repeat("* ", 129), "Host " + strings.Repeat("a", 254), strings.Repeat("User me\n", 8193)} {
		if _, err := parseSSHConfig(strings.NewReader(config), "local"); err == nil {
			t.Fatal("accepted excessive patterns/directives")
		}
	}
	p, err := parseSSHConfig(strings.NewReader("# empty\nHost *\n User me\n"), "local")
	if err != nil || len(p.Hosts) != 0 || p.Hosts == nil || p.Skipped == nil {
		t.Fatalf("empty config = %+v %v", p, err)
	}
}

func TestImportSSHHostsIsAtomicIdempotentAndPreservesPins(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	p := HostProfile{Name: "saved", Host: "saved.example", User: "me", Auth: AuthConfig{Kind: "agent"}, HostKey: &HostKey{Algorithm: "ssh-ed25519", Fingerprint: "SHA256:" + strings.Repeat("A", 43)}}
	saved, err := store.Upsert(p)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "remote-hosts.json"))
	candidate := HostProfile{Name: " new ", Host: " new.example ", User: " me ", Auth: AuthConfig{Kind: "agent"}}
	bad := candidate
	bad.Auth = AuthConfig{Kind: "key_file", IdentityFile: filepath.Join(dir, "never-read")}
	rejected := []HostProfile{candidate, bad}
	if _, err := store.ImportSSHHosts(rejected); err == nil {
		t.Fatal("accepted key-file import")
	}
	if rejected[0] != candidate || rejected[1] != bad {
		t.Fatal("rejected import modified input")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "remote-hosts.json"))
	if string(before) != string(after) {
		t.Fatal("invalid batch partially wrote store")
	}
	duplicate := p
	duplicate.ID = ""
	duplicate.HostKey = nil
	input := []HostProfile{duplicate, candidate, candidate}
	added, err := store.ImportSSHHosts(input)
	if err != nil || len(added) != 1 {
		t.Fatalf("import = %+v %v", added, err)
	}
	if !slices.Equal(input, []HostProfile{duplicate, candidate, candidate}) || added[0].Name != "new" || added[0].Host != "new.example" || added[0].User != "me" {
		t.Fatalf("import modified input or did not normalize its result: input=%+v added=%+v", input, added)
	}
	again, err := store.ImportSSHHosts([]HostProfile{candidate})
	if err != nil || len(again) != 0 {
		t.Fatalf("repeat = %+v %v", again, err)
	}
	retained, _ := store.Get(saved.ID)
	if retained.HostKey == nil || *retained.HostKey != *saved.HostKey {
		t.Fatal("existing host pin was changed")
	}
	for i := 0; i < 98; i++ {
		c := candidate
		c.Name = newID()
		c.Host = c.Name + ".example"
		if _, err := store.Upsert(c); err != nil {
			t.Fatal(err)
		}
	}
	before, _ = os.ReadFile(filepath.Join(dir, "remote-hosts.json"))
	candidate.Name = "overflow"
	candidate.Host = "overflow.example"
	if _, err := store.ImportSSHHosts([]HostProfile{candidate}); err == nil {
		t.Fatal("store quota exceeded")
	}
	after, _ = os.ReadFile(filepath.Join(dir, "remote-hosts.json"))
	if string(before) != string(after) {
		t.Fatal("quota failure changed existing store")
	}
}
