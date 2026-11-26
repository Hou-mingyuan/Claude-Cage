// Pure state helpers for multi-hop routes with single-node and auto-switch relay groups.

export const MAX_RELAY_NODES = 300;
export const RELAY_STRATEGY_FALLBACK = "fallback";
export const RELAY_STRATEGY_URL_TEST = "url-test";

export function nodeHop(nodeID = "") {
  return { kind: "node", node_id: nodeID || "" };
}

export function relayHop({
  strategy = RELAY_STRATEGY_FALLBACK,
  mode = "source",
  source_id = "",
  node_ids = [],
} = {}) {
  return {
    kind: "relay",
    strategy: strategy === RELAY_STRATEGY_URL_TEST ? RELAY_STRATEGY_URL_TEST : RELAY_STRATEGY_FALLBACK,
    mode: mode === "nodes" ? "nodes" : "source",
    source_id: mode === "source" ? (source_id || "") : "",
    node_ids: mode === "nodes" ? [...new Set((node_ids || []).filter(Boolean))] : [],
  };
}

export function savedHopsFromProxy(proxy) {
  if (Array.isArray(proxy?.route_hops) && proxy.route_hops.length > 0) {
    return proxy.route_hops.map((hop) => {
      if (hop?.relay) {
        const mode = hop.relay.source_id ? "source" : "nodes";
        return relayHop({
          strategy: hop.relay.strategy,
          mode,
          source_id: hop.relay.source_id || "",
          node_ids: hop.relay.node_ids || [],
        });
      }
      return nodeHop(hop?.node_id || "");
    });
  }
  if (Array.isArray(proxy?.route) && proxy.route.length > 0) {
    return proxy.route.map((id) => nodeHop(id));
  }
  if (proxy?.selected) {
    return [nodeHop(proxy.selected)];
  }
  return [nodeHop("")];
}

export function serializeHopsForAPI(hops) {
  const hasRelay = hops.some((hop) => hop.kind === "relay");
  if (!hasRelay) {
    return { node_ids: hops.map((hop) => hop.node_id) };
  }
  return {
    hops: hops.map((hop) => {
      if (hop.kind === "relay") {
        return {
          relay:
            hop.mode === "source"
              ? { strategy: hop.strategy, source_id: hop.source_id }
              : { strategy: hop.strategy, node_ids: [...hop.node_ids] },
        };
      }
      return { node_id: hop.node_id };
    }),
  };
}

export function hopsEqual(a, b) {
  if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    const x = a[i];
    const y = b[i];
    if (x.kind !== y.kind) return false;
    if (x.kind === "node") {
      if ((x.node_id || "") !== (y.node_id || "")) return false;
    } else {
      if (x.strategy !== y.strategy || x.mode !== y.mode) return false;
      if ((x.source_id || "") !== (y.source_id || "")) return false;
      const xIDs = x.node_ids || [];
      const yIDs = y.node_ids || [];
      if (xIDs.length !== yIDs.length) return false;
      for (let j = 0; j < xIDs.length; j++) {
        if (xIDs[j] !== yIDs[j]) return false;
      }
    }
  }
  return true;
}

export function isListPickableHop(hop) {
  return Boolean(hop && (hop.kind === "node" || (hop.kind === "relay" && hop.mode === "nodes")));
}

export function fixedNodeIDsInHops(hops) {
  const set = new Set();
  for (const hop of hops) {
    if (hop.kind === "node" && hop.node_id) set.add(hop.node_id);
  }
  return set;
}

export function hopCandidateIDs(hop, hops, sources, usableIDs) {
  if (!hop) return [];
  if (hop.kind === "node") {
    return hop.node_id && usableIDs.has(hop.node_id) ? [hop.node_id] : [];
  }
  const fixed = fixedNodeIDsInHops(hops);
  if (hop.mode === "source") {
    const src = (sources || []).find((s) => s.id === hop.source_id);
    if (!src) return [];
    return (src.nodes || [])
      .map((n) => n.id)
      .filter((id) => usableIDs.has(id) && !fixed.has(id));
  }
  return (hop.node_ids || []).filter((id) => usableIDs.has(id) && !fixed.has(id));
}

export function nodesClaimedByOtherHops(hops, targetIndex, sources, usableIDs) {
  const claimed = new Map();
  const targetIsFixed = hops[targetIndex]?.kind === "node";
  for (let i = 0; i < hops.length; i++) {
    if (i === targetIndex) continue;
    const hop = hops[i];
    if (hop.kind === "node") {
      if (hop.node_id) claimed.set(hop.node_id, i);
      continue;
    }
    // A source-based relay hop dynamically excludes fixed hops, so it should
    // not block selecting a fixed exit node from that source.
    if (targetIsFixed && hop.mode === "source") continue;
    for (const id of hopCandidateIDs(hop, hops, sources, usableIDs)) {
      if (!claimed.has(id)) claimed.set(id, i);
    }
  }
  return claimed;
}

export function sourceUsableRelayCount(source, hops, targetIndex, usableIDs, allSources = []) {
  if (!source) return 0;
  const fixed = fixedNodeIDsInHops(hops);
  const otherClaimed = nodesClaimedByOtherHops(hops, targetIndex, allSources, usableIDs);
  let count = 0;
  for (const n of source.nodes || []) {
    if (usableIDs.has(n.id) && !fixed.has(n.id) && !otherClaimed.has(n.id)) {
      count++;
    }
  }
  return count;
}

export function validateRouteHops(hops, sources, usableIDs) {
  const issuesByHop = hops.map(() => "");
  const candidateCounts = hops.map(() => 0);
  if (!Array.isArray(hops) || hops.length === 0) {
    return { valid: false, issuesByHop, firstInvalidHop: 0, candidateCounts };
  }
  const fixedSeen = new Set();
  for (let i = 0; i < hops.length; i++) {
    const hop = hops[i];
    const isFinal = i === hops.length - 1;
    if (isFinal && hop.kind !== "node") {
      issuesByHop[i] = "exit-must-be-fixed";
      continue;
    }
    if (hop.kind === "node") {
      if (!hop.node_id || !usableIDs.has(hop.node_id)) {
        issuesByHop[i] = isFinal ? "missing-exit" : "missing-relay-node";
      } else if (fixedSeen.has(hop.node_id)) {
        issuesByHop[i] = "duplicate-node";
      } else {
        fixedSeen.add(hop.node_id);
        candidateCounts[i] = 1;
      }
    }
  }
  const usedSources = new Set();
  const relayClaimed = new Set();
  for (let i = 0; i < hops.length - 1; i++) {
    const hop = hops[i];
    if (hop.kind !== "relay") continue;
    if (hop.strategy !== RELAY_STRATEGY_FALLBACK && hop.strategy !== RELAY_STRATEGY_URL_TEST) {
      issuesByHop[i] = "invalid-strategy";
      continue;
    }
    if (hop.mode === "source") {
      if (!hop.source_id) {
        issuesByHop[i] = "missing-relay-source";
        continue;
      }
      if (usedSources.has(hop.source_id)) {
        issuesByHop[i] = "duplicate-relay-source";
        continue;
      }
      usedSources.add(hop.source_id);
      const src = (sources || []).find((s) => s.id === hop.source_id);
      if (!src) {
        issuesByHop[i] = "removed-relay-source";
        continue;
      }
      const candidates = hopCandidateIDs(hop, hops, sources, usableIDs);
      candidateCounts[i] = candidates.length;
      if (candidates.some((id) => relayClaimed.has(id))) {
        issuesByHop[i] = "overlapping-relay";
        continue;
      }
      if (candidates.length > MAX_RELAY_NODES) {
        issuesByHop[i] = "too-many-source-nodes";
        continue;
      }
      if (candidates.length < 2) {
        issuesByHop[i] = "too-few-source-nodes";
        continue;
      }
      for (const id of candidates) relayClaimed.add(id);
    } else {
      const rawIDs = hop.node_ids || [];
      const validIDs = rawIDs.filter((id) => usableIDs.has(id) && !fixedSeen.has(id));
      candidateCounts[i] = validIDs.length;
      if (rawIDs.some((id) => fixedSeen.has(id))) {
        issuesByHop[i] = "custom-contains-fixed";
        continue;
      }
      if (rawIDs.some((id) => !usableIDs.has(id))) {
        issuesByHop[i] = "custom-contains-removed";
        continue;
      }
      if (validIDs.some((id) => relayClaimed.has(id))) {
        issuesByHop[i] = "overlapping-relay";
        continue;
      }
      if (validIDs.length > MAX_RELAY_NODES) {
        issuesByHop[i] = "too-many-custom-nodes";
        continue;
      }
      if (validIDs.length < 2) {
        issuesByHop[i] = "too-few-custom-nodes";
        continue;
      }
      for (const id of validIDs) relayClaimed.add(id);
    }
  }
  const firstInvalidHop = issuesByHop.findIndex(Boolean);
  return {
    valid: firstInvalidHop === -1,
    issuesByHop,
    firstInvalidHop,
    candidateCounts,
  };
}

export function resolveHopPickerIndex(hops, requestedHop, sources, usableIDs) {
  if (!Array.isArray(hops) || hops.length <= 1) return 0;
  if (
    Number.isInteger(requestedHop) &&
    requestedHop >= 0 &&
    requestedHop < hops.length &&
    isListPickableHop(hops[requestedHop])
  ) {
    return requestedHop;
  }
  const { issuesByHop } = validateRouteHops(hops, sources, usableIDs);
  const firstInvalidPickable = hops.findIndex(
    (hop, i) => isListPickableHop(hop) && Boolean(issuesByHop[i]),
  );
  if (firstInvalidPickable >= 0) return firstInvalidPickable;
  return hops.length - 1;
}

export function nextHopPickerAfterNodePick(nextHops, pickedIndex, sources, usableIDs) {
  if (!Array.isArray(nextHops) || nextHops.length <= 1) return null;
  const { issuesByHop } = validateRouteHops(nextHops, sources, usableIDs);
  for (let i = pickedIndex + 1; i < nextHops.length; i++) {
    if (isListPickableHop(nextHops[i]) && issuesByHop[i]) return i;
  }
  for (let i = 0; i < pickedIndex; i++) {
    if (isListPickableHop(nextHops[i]) && issuesByHop[i]) return i;
  }
  return null;
}

export function toggleNodeInCustomRelay(hops, hopIndex, nodeID, maxNodes = MAX_RELAY_NODES) {
  const target = hops[hopIndex];
  if (!target || target.kind !== "relay" || target.mode !== "nodes") return hops;
  const current = target.node_ids || [];
  const exists = current.includes(nodeID);
  if (!exists && current.length >= maxNodes) return hops;
  const nextIDs = exists ? current.filter((id) => id !== nodeID) : [...current, nodeID];
  return hops.map((hop, i) => (i === hopIndex ? { ...hop, node_ids: nextIDs } : hop));
}

export function bulkToggleNodesInCustomRelay(
  hops,
  hopIndex,
  candidateIDs,
  selectAll,
  maxNodes = MAX_RELAY_NODES,
) {
  const target = hops[hopIndex];
  if (!target || target.kind !== "relay" || target.mode !== "nodes") return hops;
  const current = target.node_ids || [];
  let nextIDs;
  if (selectAll) {
    const seen = new Set(current);
    nextIDs = [...current];
    for (const id of candidateIDs) {
      if (nextIDs.length >= maxNodes) break;
      if (!seen.has(id)) {
        seen.add(id);
        nextIDs.push(id);
      }
    }
  } else {
    const removeSet = new Set(candidateIDs);
    nextIDs = current.filter((id) => !removeSet.has(id));
  }
  return hops.map((hop, i) => (i === hopIndex ? { ...hop, node_ids: nextIDs } : hop));
}

export function moveNodeInCustomRelay(hops, hopIndex, nodeID, delta) {
  const target = hops[hopIndex];
  if (!target || target.kind !== "relay" || target.mode !== "nodes") return hops;
  const current = [...(target.node_ids || [])];
  const from = current.indexOf(nodeID);
  if (from < 0) return hops;
  let to;
  if (delta === -Infinity) to = 0;
  else if (delta === Infinity) to = current.length - 1;
  else to = Math.max(0, Math.min(current.length - 1, from + delta));
  if (to === from) return hops;
  current.splice(from, 1);
  current.splice(to, 0, nodeID);
  return hops.map((hop, i) => (i === hopIndex ? { ...hop, node_ids: current } : hop));
}

export function switchHopKind(hops, index, nextKind, { sources = [], allNodes = [], usableIDs = new Set(), activeNodeID = "" } = {}) {
  if (index < 0 || index >= hops.length - 1) return hops;
  const current = hops[index];
  if (!current || current.kind === nextKind) return hops;
  if (nextKind === "node") {
    const claimed = nodesClaimedByOtherHops(hops, index, sources, usableIDs);
    const candidates = hopCandidateIDs(current, hops, sources, usableIDs).filter((id) => !claimed.has(id));
    const chosen =
      activeNodeID && usableIDs.has(activeNodeID) && !claimed.has(activeNodeID)
        ? activeNodeID
        : candidates[0] || "";
    return hops.map((hop, i) => (i === index ? nodeHop(chosen) : hop));
  }
  // Switching fixed node -> auto-switch relay group.
  const usedSources = new Set(
    hops
      .filter((h, i) => i !== index && h.kind === "relay" && h.mode === "source" && h.source_id)
      .map((h) => h.source_id),
  );
  const currentNode = allNodes.find((n) => n.id === current.node_id);
  const eligibleSource = (src) => {
    if (!src || usedSources.has(src.id)) return false;
    const count = sourceUsableRelayCount(src, hops, index, usableIDs, sources);
    return count >= 2 && count <= MAX_RELAY_NODES;
  };
  const preferredSource =
    (currentNode && eligibleSource(sources.find((s) => s.id === currentNode.sourceID))
      ? sources.find((s) => s.id === currentNode.sourceID)
      : sources.find(eligibleSource)) || null;
  const nextHop = preferredSource
    ? relayHop({ strategy: RELAY_STRATEGY_FALLBACK, mode: "source", source_id: preferredSource.id })
    : relayHop({
        strategy: RELAY_STRATEGY_FALLBACK,
        mode: "nodes",
        node_ids: current.node_id && usableIDs.has(current.node_id) ? [current.node_id] : [],
      });
  return hops.map((hop, i) => (i === index ? nextHop : hop));
}

export function canSwapTwoHopRoute(hops) {
  return (
    Array.isArray(hops) &&
    hops.length === 2 &&
    hops[0]?.kind === "node" &&
    hops[1]?.kind === "node" &&
    Boolean(hops[0].node_id || hops[1].node_id)
  );
}

export function singleHopFromRoute(hops) {
  if (!Array.isArray(hops) || hops.length === 0) return [nodeHop("")];
  const exit = hops.at(-1);
  if (exit?.kind === "node" && exit.node_id) return [nodeHop(exit.node_id)];
  const fallback = hops.find((h) => h.kind === "node" && h.node_id);
  return [nodeHop(fallback?.node_id || "")];
}

export function sanitizeRelayHopAfterExitChange(hops, newExitID) {
  if (!newExitID) return hops;
  return hops.map((hop, i) => {
    if (i === hops.length - 1) return hop;
    if (hop.kind === "relay" && hop.mode === "nodes" && hop.node_ids.includes(newExitID)) {
      return { ...hop, node_ids: hop.node_ids.filter((id) => id !== newExitID) };
    }
    return hop;
  });
}
