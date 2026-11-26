package managedproxy

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	RelayStrategyFallback = "fallback"
	RelayStrategyURLTest  = "url-test"

	relayGroupPrefix      = "CAGE-RELAY-"
	MaxRelayGroupNodes    = 300
	relayProbeURL         = "https://www.gstatic.com/generate_204"
	relayProbeIntervalSec = 180
	relayProbeTimeoutMs   = 5000
	relayURLTestTolerance = 50
)

// RelayGroup defines an automatic relay pool for a non-exit hop.
// Exactly one of SourceID (dynamic source pool) or NodeIDs (explicit ordered
// node pool) is set.
type RelayGroup struct {
	Strategy string   `json:"strategy"`
	SourceID string   `json:"source_id,omitempty"`
	NodeIDs  []string `json:"node_ids,omitempty"`
}

// RouteHop is one persisted hop in an explicit multi-hop route.
// Exactly one of NodeID or Relay is set. The final hop is always a fixed node
// matching Store.Selected.
type RouteHop struct {
	NodeID string      `json:"node_id,omitempty"`
	Relay  *RelayGroup `json:"relay,omitempty"`
}

// ResolvedHop describes a validated hop ready for Mihomo config generation and
// status reporting.
type ResolvedHop struct {
	NodeID         string      `json:"node_id,omitempty"`
	Relay          *RelayGroup `json:"relay,omitempty"`
	GroupName      string      `json:"group_name,omitempty"`
	Candidates     []string    `json:"-"`
	CandidateCount int         `json:"candidate_count,omitempty"`
	ActiveNodeID   string      `json:"active_node_id,omitempty"`
}

func (h ResolvedHop) targetName() string {
	if h.Relay != nil {
		return h.GroupName
	}
	return h.NodeID
}

func cloneRelayGroup(g *RelayGroup) *RelayGroup {
	if g == nil {
		return nil
	}
	return &RelayGroup{
		Strategy: g.Strategy,
		SourceID: g.SourceID,
		NodeIDs:  append([]string(nil), g.NodeIDs...),
	}
}

func cloneRouteHops(hops []RouteHop) []RouteHop {
	if len(hops) == 0 {
		return nil
	}
	out := make([]RouteHop, len(hops))
	for i, h := range hops {
		out[i] = RouteHop{
			NodeID: h.NodeID,
			Relay:  cloneRelayGroup(h.Relay),
		}
	}
	return out
}

func validRelayStrategy(strategy string) bool {
	return strategy == RelayStrategyFallback || strategy == RelayStrategyURLTest
}

func relayMihomoGroupType(strategy string) string {
	switch strategy {
	case RelayStrategyFallback:
		return "Fallback"
	case RelayStrategyURLTest:
		return "URLTest"
	default:
		return ""
	}
}

func (s Store) findSource(id string) (Source, bool) {
	for _, src := range s.Sources {
		if src.ID == id {
			return src, true
		}
	}
	return Source{}, false
}

// ResolveRoute returns the validated structured hop list from entry to exit.
// Legacy single-node, imported dialer-proxy chains, and fixed Store.Chain routes
// are normalized into fixed ResolvedHop entries.
func (s Store) ResolveRoute() ([]ResolvedHop, error) {
	return s.resolveRoute(s.BlockedNodes(), 1)
}

func (s Store) resolveRoute(blocked map[string]string, minRelayNodes int) ([]ResolvedHop, error) {
	if len(s.RouteHops) == 0 {
		ids, err := s.routeLegacy(blocked)
		if err != nil {
			return nil, err
		}
		out := make([]ResolvedHop, len(ids))
		for i, id := range ids {
			out[i] = ResolvedHop{NodeID: id}
		}
		return out, nil
	}
	if _, ok := s.Find(s.Selected); !ok {
		return nil, errors.New("请先选择有效的最终出口")
	}
	last := s.RouteHops[len(s.RouteHops)-1]
	if last.Relay != nil {
		return nil, errors.New("最终出口必须为单个固定节点，不能使用自动切换组")
	}
	if last.NodeID != s.Selected {
		return nil, errors.New("代理链与最终出口不一致，请重新配置代理")
	}
	fixedNodes := map[string]bool{}
	fixedList := make([]string, 0, len(s.RouteHops))
	for _, hop := range s.RouteHops {
		hasNode := hop.NodeID != ""
		hasRelay := hop.Relay != nil
		if hasNode == hasRelay {
			return nil, errors.New("代理链配置无效，请重新配置代理")
		}
		if hasNode {
			if _, ok := s.Find(hop.NodeID); !ok {
				return nil, errors.New("代理链中的节点已被移除，请重新选择；原配置已保留")
			}
			if fixedNodes[hop.NodeID] {
				return nil, errors.New("代理链不能重复使用同一个节点")
			}
			fixedNodes[hop.NodeID] = true
			fixedList = append(fixedList, hop.NodeID)
		}
	}
	if _, err := s.checkRouteNodes(fixedList, blocked); err != nil {
		return nil, err
	}

	usedSources := map[string]bool{}
	relayClaimed := map[string]bool{}
	resolved := make([]ResolvedHop, len(s.RouteHops))
	for i, hop := range s.RouteHops {
		if hop.Relay == nil {
			resolved[i] = ResolvedHop{NodeID: hop.NodeID}
			continue
		}
		relay := hop.Relay
		if !validRelayStrategy(relay.Strategy) {
			return nil, errors.New("自动切换中转策略无效，请选择故障转移或最低延迟")
		}
		hasSource := strings.TrimSpace(relay.SourceID) != ""
		hasNodes := len(relay.NodeIDs) > 0
		if hasSource == hasNodes {
			return nil, errors.New("自动切换中转组需要指定来源或自选节点列表")
		}
		var candidates []string
		if hasSource {
			if usedSources[relay.SourceID] {
				return nil, errors.New("多跳代理链不能重复使用同一个中转来源")
			}
			usedSources[relay.SourceID] = true
			src, ok := s.findSource(relay.SourceID)
			if !ok {
				return nil, errors.New("自动切换中转组引用的来源已被移除，请重新选择；原配置已保留")
			}
			for _, n := range src.Nodes {
				if blocked[n.ID] != "" || fixedNodes[n.ID] {
					continue
				}
				if relayClaimed[n.ID] {
					return nil, errors.New("多跳代理链的中转节点池存在重叠，请调整各跳范围")
				}
				candidates = append(candidates, n.ID)
			}
			if len(candidates) > MaxRelayGroupNodes {
				return nil, fmt.Errorf("自动切换中转组最多包含 %d 个候选节点；请改用自选节点缩小范围", MaxRelayGroupNodes)
			}
			if len(candidates) < minRelayNodes {
				if minRelayNodes >= 2 {
					return nil, errors.New("自动切换中转组至少需要 2 个可用中转节点")
				}
				return nil, errors.New("自动切换中转组没有可用的中转节点，请重新选择；原配置已保留")
			}
			for _, id := range candidates {
				relayClaimed[id] = true
			}
		} else {
			if len(relay.NodeIDs) > MaxRelayGroupNodes {
				return nil, fmt.Errorf("自动切换中转组最多包含 %d 个候选节点；请改用自选节点缩小范围", MaxRelayGroupNodes)
			}
			seen := map[string]bool{}
			for _, id := range relay.NodeIDs {
				if id == "" {
					return nil, errors.New("自动切换中转组包含无效节点")
				}
				if seen[id] {
					return nil, errors.New("自动切换中转组不能包含重复节点")
				}
				seen[id] = true
				if _, ok := s.Find(id); !ok {
					return nil, errors.New("自动切换中转组中的节点已被移除，请重新选择；原配置已保留")
				}
				if _, err := s.checkRouteNodes([]string{id}, blocked); err != nil {
					return nil, err
				}
				if fixedNodes[id] {
					return nil, errors.New("自动切换中转组不能包含最终出口或其他固定跳节点")
				}
				if relayClaimed[id] {
					return nil, errors.New("多跳代理链的中转节点池存在重叠，请调整各跳范围")
				}
				candidates = append(candidates, id)
			}
			if len(candidates) < minRelayNodes {
				if minRelayNodes >= 2 {
					return nil, errors.New("自动切换中转组至少需要 2 个可用中转节点")
				}
				return nil, errors.New("自动切换中转组没有可用的中转节点，请重新选择；原配置已保留")
			}
			for _, id := range candidates {
				relayClaimed[id] = true
			}
		}
		resolved[i] = ResolvedHop{
			Relay:          cloneRelayGroup(relay),
			GroupName:      fmt.Sprintf("%s%d", relayGroupPrefix, i),
			Candidates:     candidates,
			CandidateCount: len(candidates),
		}
	}
	return resolved, nil
}

// SetRouteHops validates and applies structured hops to the store.
// Single-hop and fixed-only routes collapse into Store.Selected / Store.Chain
// so existing stores and #345 single-exit dialer preservation remain unchanged.
func (s *Store) SetRouteHops(hops []RouteHop) error {
	if len(hops) == 0 {
		return errors.New("请至少选择一个最终出口")
	}
	last := hops[len(hops)-1]
	if last.Relay != nil {
		return errors.New("最终出口必须为单个固定节点，不能使用自动切换组")
	}
	if last.NodeID == "" {
		return errors.New("请先选择有效的最终出口")
	}
	hasRelay := false
	for _, h := range hops {
		if h.Relay != nil {
			hasRelay = true
			break
		}
	}
	if !hasRelay {
		ids := make([]string, len(hops))
		for i, h := range hops {
			if h.NodeID == "" {
				return errors.New("代理链配置无效，请重新配置代理")
			}
			ids[i] = h.NodeID
		}
		s.Selected = ids[len(ids)-1]
		s.RouteHops = nil
		if len(ids) == 1 {
			s.Chain = nil
		} else {
			s.Chain = ids
		}
		_, err := s.Route()
		return err
	}
	s.Selected = last.NodeID
	s.Chain = nil
	s.RouteHops = cloneRouteHops(hops)
	_, err := s.resolveRoute(s.BlockedNodes(), 2)
	return err
}

// UsesSource reports whether the active route (fixed chain or relay group hops)
// depends on any node from sourceID or directly references sourceID.
func (s Store) UsesSource(sourceID string) bool {
	nodeIDs := map[string]bool{}
	if src, ok := s.findSource(sourceID); ok {
		for _, n := range src.Nodes {
			nodeIDs[n.ID] = true
		}
	}
	if len(s.RouteHops) > 0 {
		for _, hop := range s.RouteHops {
			if hop.NodeID != "" && nodeIDs[hop.NodeID] {
				return true
			}
			if hop.Relay != nil {
				if hop.Relay.SourceID == sourceID {
					return true
				}
				for _, id := range hop.Relay.NodeIDs {
					if nodeIDs[id] {
						return true
					}
				}
			}
		}
		return false
	}
	if len(s.Chain) > 1 {
		for _, id := range s.Chain {
			if nodeIDs[id] {
				return true
			}
		}
	}
	return false
}

// HasRelayGroups reports whether the saved route uses any auto-switching relay group.
func (s Store) HasRelayGroups() bool {
	for _, hop := range s.RouteHops {
		if hop.Relay != nil {
			return true
		}
	}
	return false
}

func (s Store) relayProxyGroups(hops []ResolvedHop) []any {
	groups := make([]any, 0, len(hops))
	groups = append(groups, map[string]any{
		"name":    Group,
		"type":    "select",
		"proxies": []string{s.Selected},
	})
	for _, hop := range hops {
		if hop.Relay == nil {
			continue
		}
		pg := map[string]any{
			"name":     hop.GroupName,
			"type":     hop.Relay.Strategy,
			"proxies":  append([]string(nil), hop.Candidates...),
			"url":      relayProbeURL,
			"interval": relayProbeIntervalSec,
			"timeout":  relayProbeTimeoutMs,
			"lazy":     false,
		}
		if hop.Relay.Strategy == RelayStrategyURLTest {
			pg["tolerance"] = relayURLTestTolerance
		}
		groups = append(groups, pg)
	}
	return groups
}

func hopDialers(hops []ResolvedHop, explicit bool) map[string]string {
	dialers := map[string]string{}
	if !explicit {
		return dialers
	}
	for i, hop := range hops {
		upstream := ""
		if i > 0 {
			upstream = hops[i-1].targetName()
		}
		if hop.Relay != nil {
			for _, id := range hop.Candidates {
				dialers[id] = upstream
			}
		} else {
			dialers[hop.NodeID] = upstream
		}
	}
	return dialers
}

func resolvedRouteHasIPv6Server(hops []ResolvedHop, nodes map[string]map[string]any) bool {
	ids := make([]string, 0, len(hops))
	for _, hop := range hops {
		if hop.Relay != nil {
			ids = append(ids, hop.Candidates...)
		} else if hop.NodeID != "" {
			ids = append(ids, hop.NodeID)
		}
	}
	return routeHasIPv6Server(ids, nodes)
}

func (s *Store) refreshRouteHopsForSource(sourceIndex int, updated Source) ([]RouteHop, int, error) {
	oldSource := s.Sources[sourceIndex]
	available := make(map[string]bool, len(updated.Nodes))
	for _, n := range updated.Nodes {
		available[n.ID] = true
	}
	oldIDs := make(map[string]Node, len(oldSource.Nodes))
	for _, n := range oldSource.Nodes {
		oldIDs[n.ID] = n
	}
	// Fixed hops (including the final exit) must never be silently removed.
	for _, hop := range s.RouteHops {
		if hop.NodeID == "" {
			continue
		}
		if old, existed := oldIDs[hop.NodeID]; existed && !available[old.ID] {
			reason := "节点未包含在更新中"
			for _, issue := range updated.Skipped {
				if issue.Name == old.Name {
					reason = issue.Reason
					break
				}
			}
			return nil, 0, fmt.Errorf("订阅更新移除或跳过了当前出口或中转节点 %q：%s；原配置已保留，请先应用其他节点再更新", old.Name, reason)
		}
	}
	nextHops := cloneRouteHops(s.RouteHops)
	pruned := 0
	for i := range nextHops {
		relay := nextHops[i].Relay
		if relay == nil || len(relay.NodeIDs) == 0 {
			continue
		}
		kept := make([]string, 0, len(relay.NodeIDs))
		for _, id := range relay.NodeIDs {
			if _, fromOld := oldIDs[id]; fromOld && !available[id] {
				pruned++
				continue
			}
			kept = append(kept, id)
		}
		if len(kept) == 0 {
			return nil, 0, errors.New("订阅更新移除了当前自动切换中转组中的全部可用节点；原配置已保留，请先调整代理链再更新")
		}
		relay.NodeIDs = kept
	}
	trial := *s
	trial.Sources = append([]Source(nil), s.Sources...)
	trial.Sources[sourceIndex] = updated
	trial.RouteHops = nextHops
	if _, err := trial.resolveRoute(trial.BlockedNodes(), 1); err != nil {
		if errors.Is(err, ErrLocalProxyNode) {
			return nil, 0, err
		}
		return nil, 0, errors.New("订阅更新后当前自动切换中转组无可用节点；原配置已保留，请先调整代理链再更新")
	}
	return nextHops, pruned, nil
}

// RelayCandidatesMatch checks whether a running Mihomo group's member list
// matches the resolved candidate slice in order.
func relayGroupMatches(groupType string, members []string, hop ResolvedHop) bool {
	if hop.Relay == nil {
		return false
	}
	wantType := relayMihomoGroupType(hop.Relay.Strategy)
	if !strings.EqualFold(groupType, wantType) {
		return false
	}
	return slices.Equal(members, hop.Candidates)
}
