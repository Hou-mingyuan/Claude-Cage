// Pure state helpers for unified multi-hop proxy routes.
//
// Mental model:
// - The final hop (and single-node mode) is always a single fixed node:
//   { kind: "node", node_id: string }
// - Every non-final transit hop is a unified group container:
//   { kind: "relay", strategy: "fallback" | "url-test", source_id: string, node_ids: string[] }
//   * 0 nodes -> empty
//   * 1 node  -> single fixed transit node (auto-switch strategy hidden/unused)
//   * >=2 nodes or a whole source -> auto-switching relay group

export const MAX_RELAY_NODES = 300;
export const RELAY_STRATEGY_FALLBACK = "fallback";
export const RELAY_STRATEGY_URL_TEST = "url-test";

export function nodeHop(nodeID = "") {
  return { kind: "node", node_id: nodeID || "" };
}

export function relayHop({
  strategy = RELAY_STRATEGY_FALLBACK,
  source_id = "",
  node_ids = [],
} = {}) {
  const src = source_id || "";
  return {
    kind: "relay",
    strategy: strategy === RELAY_STRATEGY_URL_TEST ? RELAY_STRATEGY_URL_TEST : RELAY_STRATEGY_FALLBACK,
    source_id: src,
    node_ids: src ? [] : [...new Set((node_ids || []).filter(Boolean))],
  };
}

export function isMultiNodeHop(hop) {
  if (!hop || hop.kind !== "relay") return false;
  return Boolean(hop.source_id) || (hop.node_ids?.length || 0) >= 2;
}

export function singleNodeIDOfHop(hop) {
  if (!hop) return "";
  if (hop.kind === "node") return hop.node_id || "";
  if (!hop.source_id && hop.node_ids?.length === 1) return hop.node_ids[0];
  return "";
}

export function savedHopsFromProxy(proxy) {
  if (Array.isArray(proxy?.route_hops) && proxy.route_hops.length > 0) {
    const lastIdx = proxy.route_hops.length - 1;
    return proxy.route_hops.map((hop, idx) => {
      if (idx === lastIdx) {
        return nodeHop(hop?.node_id || "");
      }
      if (hop?.relay) {
        return relayHop({
          strategy: hop.relay.strategy,
          source_id: hop.relay.source_id || "",
          node_ids: hop.relay.node_ids || [],
        });
      }
      return relayHop({ node_ids: hop?.node_id ? [hop.node_id] : [] });
    });
  }
  if (Array.isArray(proxy?.route) && proxy.route.length > 0) {
    const lastIdx = proxy.route.length - 1;
    return proxy.route.map((id, idx) =>
      idx === lastIdx ? nodeHop(id) : relayHop({ node_ids: id ? [id] : [] }),
    );
  }
  if (proxy?.selected) {
    return [nodeHop(proxy.selected)];
  }
  return [nodeHop("")];
}

export function serializeHopsForAPI(hops) {
  const hasAutoGroup = hops.some((hop, i) => i < hops.length - 1 && isMultiNodeHop(hop));
  if (!hasAutoGroup) {
    return {
      node_ids: hops.map((hop) => singleNodeIDOfHop(hop)),
    };
  }
  return {
    hops: hops.map((hop, i) => {
      if (i === hops.length - 1 || !isMultiNodeHop(hop)) {
        return { node_id: singleNodeIDOfHop(hop) };
      }
      return {
        relay: hop.source_id
          ? { strategy: hop.strategy, source_id: hop.source_id }
          : { strategy: hop.strategy, node_ids: [...hop.node_ids] },
      };
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
      if ((x.source_id || "") !== (y.source_id || "")) return false;
      const xIDs = x.node_ids || [];
      const yIDs = y.node_ids || [];
      if (xIDs.length !== yIDs.length) return false;
      for (let j = 0; j < xIDs.length; j++) {
        if (xIDs[j] !== yIDs[j]) return false;
      }
      // Strategy only matters when the hop actually has multiple nodes.
      if (isMultiNodeHop(x) && x.strategy !== y.strategy) return false;
    }
  }
  return true;
}

export function fixedNodeIDsInHops(hops) {
  const set = new Set();
  for (const hop of hops) {
    const id = singleNodeIDOfHop(hop);
    if (id) set.add(id);
  }
  return set;
}

export function hopCandidateIDs(hop, hops, sources, usableIDs) {
  if (!hop) return [];
  if (hop.kind === "node") {
    return hop.node_id && usableIDs.has(hop.node_id) ? [hop.node_id] : [];
  }
  const fixed = fixedNodeIDsInHops(hops);
  if (hop.source_id) {
    const src = (sources || []).find((s) => s.id === hop.source_id);
    if (!src) return [];
    return (src.nodes || [])
      .map((n) => n.id)
      .filter((id) => usableIDs.has(id) && !fixed.has(id));
  }
  if (hop.node_ids.length === 1) {
    const id = hop.node_ids[0];
    return usableIDs.has(id) ? [id] : [];
  }
  return (hop.node_ids || []).filter((id) => usableIDs.has(id) && !fixed.has(id));
}

export function nodesClaimedByOtherHops(hops, targetIndex, sources, usableIDs) {
  const claimed = new Map();
  const targetIsExit = targetIndex === hops.length - 1;
  for (let i = 0; i < hops.length; i++) {
    if (i === targetIndex) continue;
    const hop = hops[i];
    // A source-based relay dynamically excludes the fixed exit, so it should
    // not block selecting a fixed exit node from that source.
    if (targetIsExit && hop.kind === "relay" && hop.source_id) continue;
    if (hop.kind === "node") {
      if (hop.node_id) claimed.set(hop.node_id, i);
      continue;
    }
    for (const id of hopCandidateIDs(hop, hops, sources, usableIDs)) {
      if (!claimed.has(id)) claimed.set(id, i);
    }
  }
  return claimed;
}

export function sourceUsableRelayCount(source, hops, targetIndex, usableIDs, allSources = []) {
  if (!source) return 0;
  const otherFixed = new Set();
  for (let i = 0; i < hops.length; i++) {
    if (i === targetIndex) continue;
    const id = singleNodeIDOfHop(hops[i]);
    if (id) otherFixed.add(id);
  }
  const otherClaimed = nodesClaimedByOtherHops(hops, targetIndex, allSources, usableIDs);
  let count = 0;
  for (const n of source.nodes || []) {
    if (usableIDs.has(n.id) && !otherFixed.has(n.id) && !otherClaimed.has(n.id)) {
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
    if (isFinal) {
      if (hop.kind !== "node" || !hop.node_id || !usableIDs.has(hop.node_id)) {
        issuesByHop[i] = "missing-exit";
      } else if (fixedSeen.has(hop.node_id)) {
        issuesByHop[i] = "duplicate-node";
      } else {
        fixedSeen.add(hop.node_id);
        candidateCounts[i] = 1;
      }
      continue;
    }
    const singleID = singleNodeIDOfHop(hop);
    if (singleID) {
      if (!usableIDs.has(singleID)) {
        issuesByHop[i] = "missing-relay-node";
      } else if (fixedSeen.has(singleID)) {
        issuesByHop[i] = "duplicate-node";
      } else {
        fixedSeen.add(singleID);
        candidateCounts[i] = 1;
      }
    }
  }
  const usedSources = new Set();
  const relayClaimed = new Set();
  for (let i = 0; i < hops.length - 1; i++) {
    const hop = hops[i];
    if (singleNodeIDOfHop(hop)) continue;
    if (!hop.source_id && (hop.node_ids?.length || 0) === 0) {
      issuesByHop[i] = "missing-relay-node";
      continue;
    }
    if (hop.strategy !== RELAY_STRATEGY_FALLBACK && hop.strategy !== RELAY_STRATEGY_URL_TEST) {
      issuesByHop[i] = "invalid-strategy";
      continue;
    }
    if (hop.source_id) {
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
  if (Number.isInteger(requestedHop) && requestedHop >= 0 && requestedHop < hops.length) {
    return requestedHop;
  }
  const { issuesByHop } = validateRouteHops(hops, sources, usableIDs);
  const firstInvalid = issuesByHop.findIndex(Boolean);
  if (firstInvalid >= 0) return firstInvalid;
  return hops.length - 1;
}

export function nextHopPickerAfterExitPick(nextHops, sources, usableIDs) {
  if (!Array.isArray(nextHops) || nextHops.length <= 1) return null;
  const { issuesByHop } = validateRouteHops(nextHops, sources, usableIDs);
  const firstInvalid = issuesByHop.findIndex(Boolean);
  return firstInvalid >= 0 ? firstInvalid : null;
}

// Toggle a node inside a transit hop. If the hop currently holds a whole
// subscription (`source_id`), materialize its usable candidates first so
// unchecking one node cleanly keeps the rest as a custom pool, and checking an
// outside node appends it seamlessly.
export function toggleNodeInRelayHop(hops, hopIndex, nodeID, sources, usableIDs, maxNodes = MAX_RELAY_NODES) {
  const target = hops[hopIndex];
  if (!target || target.kind !== "relay") return hops;
  const baseIDs = target.source_id
    ? hopCandidateIDs(target, hops, sources, usableIDs).slice(0, maxNodes)
    : target.node_ids || [];
  const exists = baseIDs.includes(nodeID);
  if (!exists && baseIDs.length >= maxNodes) return hops;
  const nextIDs = exists ? baseIDs.filter((id) => id !== nodeID) : [...baseIDs, nodeID];
  return hops.map((hop, i) =>
    i === hopIndex ? relayHop({ strategy: hop.strategy, source_id: "", node_ids: nextIDs }) : hop,
  );
}

export function bulkToggleNodesInRelayHop(
  hops,
  hopIndex,
  candidateIDs,
  selectAll,
  sources,
  usableIDs,
  maxNodes = MAX_RELAY_NODES,
) {
  const target = hops[hopIndex];
  if (!target || target.kind !== "relay") return hops;
  const current = target.source_id
    ? hopCandidateIDs(target, hops, sources, usableIDs).slice(0, maxNodes)
    : target.node_ids || [];
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
  return hops.map((hop, i) =>
    i === hopIndex ? relayHop({ strategy: hop.strategy, source_id: "", node_ids: nextIDs }) : hop,
  );
}

export function moveNodeInRelayHop(hops, hopIndex, nodeID, delta) {
  const target = hops[hopIndex];
  if (!target || target.kind !== "relay" || target.source_id) return hops;
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

export function reorderNodeInRelayHop(hops, hopIndex, fromID, toID) {
  const target = hops[hopIndex];
  if (!target || target.kind !== "relay" || target.source_id || !fromID || !toID || fromID === toID) {
    return hops;
  }
  const current = [...(target.node_ids || [])];
  const from = current.indexOf(fromID);
  const to = current.indexOf(toID);
  if (from < 0 || to < 0 || from === to) return hops;
  current.splice(from, 1);
  current.splice(to, 0, fromID);
  return hops.map((hop, i) => (i === hopIndex ? { ...hop, node_ids: current } : hop));
}

export function canSwapTwoHopRoute(hops) {
  if (!Array.isArray(hops) || hops.length !== 2) return false;
  const h0 = hops[0];
  const h1 = hops[1];
  if (h0?.kind !== "relay" || h1?.kind !== "node") return false;
  if (isMultiNodeHop(h0)) return false;
  return Boolean(singleNodeIDOfHop(h0) || h1.node_id);
}

export function swapTwoHopRoute(hops) {
  if (!canSwapTwoHopRoute(hops)) return hops;
  const transitID = singleNodeIDOfHop(hops[0]);
  const exitID = hops[1].node_id;
  return [
    relayHop({ strategy: hops[0].strategy, node_ids: exitID ? [exitID] : [] }),
    nodeHop(transitID),
  ];
}

export function singleHopFromRoute(hops) {
  if (!Array.isArray(hops) || hops.length === 0) return [nodeHop("")];
  const exit = hops.at(-1);
  if (exit?.kind === "node" && exit.node_id) return [nodeHop(exit.node_id)];
  for (const hop of hops) {
    const id = singleNodeIDOfHop(hop) || hop.node_ids?.[0];
    if (id) return [nodeHop(id)];
  }
  return [nodeHop("")];
}

export function sanitizeRelayHopAfterExitChange(hops, newExitID) {
  if (!newExitID) return hops;
  return hops.map((hop, i) => {
    if (i === hops.length - 1 || hop.kind !== "relay" || hop.source_id) return hop;
    if (hop.node_ids.includes(newExitID)) {
      return { ...hop, node_ids: hop.node_ids.filter((id) => id !== newExitID) };
    }
    return hop;
  });
}

export function hasConfiguredRelays(hops) {
  if (!Array.isArray(hops) || hops.length <= 1) return false;
  return hops.slice(0, -1).some((hop) =>
    hop?.kind === "relay" && (Boolean(hop.source_id) || (hop.node_ids?.length || 0) > 0),
  );
}

export function restoreChainRoute(currentExitHop, rememberedRelays, sources, usableIDs) {
  const exit = currentExitHop?.kind === "node" ? nodeHop(currentExitHop.node_id) : nodeHop("");
  if (!Array.isArray(rememberedRelays) || rememberedRelays.length === 0) {
    return [relayHop(), exit];
  }
  const sourceIDs = new Set((sources || []).map((s) => s.id));
  const cleaned = rememberedRelays
    .filter((hop) => hop?.kind === "relay")
    .map((hop) => {
      if (hop.source_id) {
        return sourceIDs.has(hop.source_id)
          ? relayHop({ strategy: hop.strategy, source_id: hop.source_id })
          : relayHop({ strategy: hop.strategy });
      }
      const kept = (hop.node_ids || []).filter((id) => !usableIDs || usableIDs.has(id));
      return relayHop({ strategy: hop.strategy, node_ids: kept });
    });
  if (cleaned.length === 0) return [relayHop(), exit];
  return sanitizeRelayHopAfterExitChange([...cleaned, exit], exit.node_id);
}
