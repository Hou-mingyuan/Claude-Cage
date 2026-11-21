package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

func TestProxyDelayBatchBoundsConcurrencyAndKeepsSelection(t *testing.T) {
	s := testServer(t)
	p, _ := managedproxy.Load(s.profile)
	if err := p.Import("batch", "", []byte(`proxies:
  - {name: slow, type: http, server: 203.0.113.10, port: 1081}
  - {name: fast, type: http, server: 203.0.113.10, port: 1082}
  - {name: broken, type: http, server: 203.0.113.10, port: 1083}
  - {name: mid, type: http, server: 203.0.113.10, port: 1084}
  - {name: other, type: http, server: 203.0.113.10, port: 1085}
`), "batch", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(s.profile); err != nil {
		t.Fatal(err)
	}
	selected := p.Selected
	ids := map[string]string{}
	var order []string
	for _, n := range p.Sources[1].Nodes {
		ids[n.ID] = n.Name
		order = append(order, n.ID)
	}
	var active, peak atomic.Int32
	var mu sync.Mutex
	measured := map[string]int{}
	old := measureProxyDelay
	t.Cleanup(func() { measureProxyDelay = old })
	measureProxyDelay = func(_ context.Context, _ string, id string) (int, error) {
		now := active.Add(1)
		defer active.Add(-1)
		for {
			if was := peak.Load(); now <= was || peak.CompareAndSwap(was, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		measured[id]++
		mu.Unlock()
		switch ids[id] {
		case "broken":
			return 0, errors.New("测速超时：节点已载入核心")
		case "fast":
			return 40, nil
		case "mid":
			return 90, nil
		default:
			return 300, nil
		}
	}
	body, _ := json.Marshal(proxyInput{NodeIDs: order})
	w := request(s, "POST", "/api/proxy/delay", s.origin, string(body), true)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var reply struct {
		Results []proxyDelayResult
		Best    string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Results) != len(order) || ids[reply.Best] != "fast" {
		t.Fatalf("unexpected batch reply: %s", w.Body.String())
	}
	for i, result := range reply.Results {
		if result.NodeID != order[i] {
			t.Fatal("results lost request order")
		}
		if ids[result.NodeID] == "broken" && (result.Error == "" || result.Delay != 0) {
			t.Fatalf("failed node lost its reason: %+v", result)
		}
	}
	if peak.Load() > proxyDelayWorkers || peak.Load() < 2 {
		t.Fatalf("peak concurrency %d, want 2..%d", peak.Load(), proxyDelayWorkers)
	}
	for _, id := range order {
		if measured[id] != 1 {
			t.Fatalf("node measured %d times", measured[id])
		}
	}
	after, _ := managedproxy.Load(s.profile)
	if after.Selected != selected || len(after.Chain) != 0 {
		t.Fatal("batch measurement changed the selected node")
	}
	for _, bad := range []proxyInput{
		{NodeIDs: []string{order[0], order[0]}},
		{NodeIDs: []string{"missing"}},
		{NodeIDs: order, NodeID: order[0]},
		{NodeIDs: make([]string, proxyDelayMaxIDs+1)},
	} {
		body, _ := json.Marshal(bad)
		if w := request(s, "POST", "/api/proxy/delay", s.origin, string(body), true); w.Code == http.StatusOK {
			t.Fatalf("invalid batch accepted: %+v", bad)
		}
	}
	single, _ := json.Marshal(proxyInput{NodeID: order[1]})
	if w := request(s, "POST", "/api/proxy/delay", s.origin, string(single), true); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"delay":40}` {
		t.Fatalf("single-node reply changed: %d %s", w.Code, w.Body.String())
	}
}
