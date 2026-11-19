package workbench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

// Run the embedded production script with the same node -e entry and input as
// macOSGuestObservation. Replace OS and network calls so no public probe leaves
// the test process and no installed helper or personal settings are accessed.
func TestMacOSBoundaryProbeReturnsOnlyVerifiedLinkOutcome(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required to execute the macOS observation script")
	}
	const bootstrap = `
const Module = require('node:module');
const EventEmitter = require('node:events');
const scenario = JSON.parse(process.argv[2]);
const originalLoad = Module._load;
Module._load = function(name, ...args) {
  if (name === 'node:fs') return { readlinkSync() { throw Error('not available'); } };
  if (name === 'node:net') return { connect() {
    const socket = new EventEmitter();
    socket.destroy = () => {};
    socket.setTimeout = (_delay, callback) => queueMicrotask(callback);
    return socket;
  } };
  if (name === 'node:dgram') return { createSocket: () => ({
    send(_data, _port, _host, callback) { queueMicrotask(() => callback(null)); }, close() {},
  }) };
  if (name === 'node:dns') return { promises: { lookup: async () => { throw Error('no route'); } } };
  if (name === 'node:child_process') return {
    execFile() { throw Error('unexpected scoped probe'); },
    spawnSync() { return { stdout: '', stderr: 'not in table', status: 1 }; },
    execFileSync(command, args) {
      if (command === '/usr/sbin/netstat') return 'Internet6:\n' + scenario.routes;
      if (command === '/sbin/route') return 'interface: ' + scenario.iface;
      if (command === '/usr/local/libexec/cage-macos-ipv6-check') {
        if (args[0] === 'default') return '{"globalSources":0,"boundIndex":0,"result":"no-route"}';
        if (args[0] === 'link') {
          if (args[1] !== scenario.iface) throw Error('wrong interface');
          if (scenario.helperFails) throw Error('interface unavailable');
          return 'sent';
        }
      }
      return '';
    },
  };
  return originalLoad.call(this, name, ...args);
};
`
	for _, tc := range []struct {
		name, iface, routes string
		helperFails         bool
		missingIDs          bool
		want                bool
	}{
		{name: "ethernet", iface: "en0", want: true},
		{name: "ethernet with unsupported IPv6 default", iface: "en0", routes: "default fe80::1%en0 UGcIg en0\n", want: true},
		{name: "Apple virtual ethernet", iface: "vmenet0", want: true},
		{name: "failed helper", iface: "en0", helperFails: true},
		{name: "tunnel", iface: "utun0"},
		{name: "missing check identity", iface: "en0", missingIDs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{"stage": "boundary", "run_id": strings.Repeat("a", 32), "check_id": strings.Repeat("b", 32)}
			if tc.missingIDs {
				delete(input, "check_id")
			}
			data, _ := json.Marshal(input)
			scenario, _ := json.Marshal(map[string]any{"iface": tc.iface, "helperFails": tc.helperFails, "routes": tc.routes})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, node, "-e", bootstrap+macOSSafetyProbe, base64.StdEncoding.EncodeToString(data), string(scenario)).CombinedOutput()
			if err != nil {
				t.Fatalf("observation script failed: %v: %s", err, out)
			}
			var result guestObservation
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatalf("invalid observation: %v: %s", err, out)
			}
			if result.IPv6LinkInterfaceValid != tc.want || result.IPv6LinkSent != tc.want {
				t.Fatalf("link result did not reflect helper success: %s", out)
			}
			if strings.Contains(string(out), "ipv6_link_interface\"") || strings.Contains(string(out), tc.iface) {
				t.Fatalf("raw link interface exposed in observation: %s", out)
			}
			if tc.routes != "" {
				if result.IPv6DefaultRoutes != 1 || len(result.IPv6Scoped) != 1 || result.IPv6Scoped[0].RouteVerified {
					t.Fatalf("unsupported default route must remain present and unverified: %s", out)
				}
				before := cage.GateStatus{Mode: cage.GateChecking, RunID: input["run_id"].(string), CheckID: input["check_id"].(string)}
				after := before
				after.PolicyVerified = true
				after.IPv6LinkProbeCheck = before.CheckID
				server := &Server{macosGuest: &guestos.MacOSProfile{}}
				if server.ipv6PolicyVerified(result, before, after, true) {
					t.Fatal("host accepted an unsupported default route after a successful link probe")
				}
			}
		})
	}
}
