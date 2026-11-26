import assert from "node:assert/strict";
import test from "node:test";
import {
  RELAY_STRATEGY_FALLBACK,
  RELAY_STRATEGY_URL_TEST,
  bulkToggleNodesInCustomRelay,
  canSwapTwoHopRoute,
  hopsEqual,
  moveNodeInCustomRelay,
  nextHopPickerAfterNodePick,
  nodeHop,
  nodesClaimedByOtherHops,
  relayHop,
  resolveHopPickerIndex,
  sanitizeRelayHopAfterExitChange,
  savedHopsFromProxy,
  serializeHopsForAPI,
  singleHopFromRoute,
  switchHopKind,
  toggleNodeInCustomRelay,
  validateRouteHops,
} from "./proxy-route.mjs";

const sources = [
  {
    id: "airport",
    name: "日常机场",
    nodes: [
      { id: "hk1", name: "香港 01" },
      { id: "hk2", name: "香港 02" },
      { id: "jp1", name: "日本 01" },
    ],
  },
  {
    id: "home",
    name: "住宅出口",
    nodes: [{ id: "exit1", name: "美国住宅" }],
  },
];
const allNodes = sources.flatMap((s) => s.nodes.map((n) => ({ ...n, sourceID: s.id, source: s.name })));
const usableIDs = new Set(["hk1", "hk2", "jp1", "exit1"]);

test("savedHopsFromProxy and serializeHopsForAPI handle fixed and relay routes", () => {
  const fixedHops = savedHopsFromProxy({ route: ["hk1", "exit1"], selected: "exit1" });
  assert.deepEqual(fixedHops, [nodeHop("hk1"), nodeHop("exit1")]);
  assert.deepEqual(serializeHopsForAPI(fixedHops), { node_ids: ["hk1", "exit1"] });

  const relayRoute = savedHopsFromProxy({
    selected: "exit1",
    route_hops: [
      {
        relay: { strategy: RELAY_STRATEGY_FALLBACK, source_id: "airport" },
        candidate_count: 3,
        active_node_id: "hk2",
      },
      { node_id: "exit1" },
    ],
  });
  assert.deepEqual(relayRoute, [
    relayHop({ strategy: RELAY_STRATEGY_FALLBACK, mode: "source", source_id: "airport" }),
    nodeHop("exit1"),
  ]);
  assert.deepEqual(serializeHopsForAPI(relayRoute), {
    hops: [
      { relay: { strategy: RELAY_STRATEGY_FALLBACK, source_id: "airport" } },
      { node_id: "exit1" },
    ],
  });
  assert.equal(hopsEqual(relayRoute, [...relayRoute]), true);
});

test("validateRouteHops enforces exit lock, minimum 2 relay nodes, and disjointness", () => {
  const okSource = validateRouteHops(
    [relayHop({ strategy: RELAY_STRATEGY_FALLBACK, mode: "source", source_id: "airport" }), nodeHop("exit1")],
    sources,
    usableIDs,
  );
  assert.equal(okSource.valid, true);
  assert.deepEqual(okSource.candidateCounts, [3, 1]);

  const tooFewCustom = validateRouteHops(
    [relayHop({ strategy: RELAY_STRATEGY_FALLBACK, mode: "nodes", node_ids: ["hk1"] }), nodeHop("exit1")],
    sources,
    usableIDs,
  );
  assert.equal(tooFewCustom.valid, false);
  assert.equal(tooFewCustom.firstInvalidHop, 0);

  const okCustom = validateRouteHops(
    [relayHop({ strategy: RELAY_STRATEGY_URL_TEST, mode: "nodes", node_ids: ["jp1", "hk1"] }), nodeHop("exit1")],
    sources,
    usableIDs,
  );
  assert.equal(okCustom.valid, true);
  assert.deepEqual(okCustom.candidateCounts, [2, 1]);

  // Same-source relay excludes exit automatically, still leaving 2 usable nodes.
  const sameSourceOK = validateRouteHops(
    [relayHop({ strategy: RELAY_STRATEGY_FALLBACK, mode: "source", source_id: "airport" }), nodeHop("jp1")],
    sources,
    usableIDs,
  );
  assert.equal(sameSourceOK.valid, true);
  assert.deepEqual(sameSourceOK.candidateCounts, [2, 1]);

  // Relay as final hop is always rejected.
  const relayExit = validateRouteHops(
    [relayHop({ strategy: RELAY_STRATEGY_FALLBACK, mode: "source", source_id: "airport" })],
    sources,
    usableIDs,
  );
  assert.equal(relayExit.valid, false);
});

test("custom relay pool toggle, bulk toggle, priority reorder, and exit sanitize work smoothly", () => {
  let hops = [relayHop({ strategy: RELAY_STRATEGY_FALLBACK, mode: "nodes", node_ids: [] }), nodeHop("exit1")];
  hops = toggleNodeInCustomRelay(hops, 0, "hk1");
  hops = toggleNodeInCustomRelay(hops, 0, "hk2");
  hops = toggleNodeInCustomRelay(hops, 0, "jp1");
  assert.deepEqual(hops[0].node_ids, ["hk1", "hk2", "jp1"]);

  // Move jp1 up one and to top.
  hops = moveNodeInCustomRelay(hops, 0, "jp1", -1);
  assert.deepEqual(hops[0].node_ids, ["hk1", "jp1", "hk2"]);
  hops = moveNodeInCustomRelay(hops, 0, "hk2", -Infinity);
  assert.deepEqual(hops[0].node_ids, ["hk2", "hk1", "jp1"]);

  // Toggle off hk1.
  hops = toggleNodeInCustomRelay(hops, 0, "hk1");
  assert.deepEqual(hops[0].node_ids, ["hk2", "jp1"]);

  // Bulk select region and bulk deselect region.
  hops = bulkToggleNodesInCustomRelay(hops, 0, ["hk1", "hk2"], true);
  assert.deepEqual(hops[0].node_ids, ["hk2", "jp1", "hk1"]);
  hops = bulkToggleNodesInCustomRelay(hops, 0, ["hk1", "hk2"], false);
  assert.deepEqual(hops[0].node_ids, ["jp1"]);

  // Switching exit to jp1 cleanly strips jp1 from custom relay pool.
  const sanitized = sanitizeRelayHopAfterExitChange([
    relayHop({ strategy: RELAY_STRATEGY_FALLBACK, mode: "nodes", node_ids: ["hk1", "jp1", "hk2"] }),
    nodeHop("jp1"),
  ], "jp1");
  assert.deepEqual(sanitized[0].node_ids, ["hk1", "hk2"]);
});

test("switchHopKind, resolveHopPickerIndex, and swap guard behave predictably", () => {
  const fixed = [nodeHop("hk1"), nodeHop("exit1")];
  assert.equal(canSwapTwoHopRoute(fixed), true);

  const toRelay = switchHopKind(fixed, 0, "relay", { sources, allNodes, usableIDs });
  assert.deepEqual(toRelay[0], relayHop({ strategy: RELAY_STRATEGY_FALLBACK, mode: "source", source_id: "airport" }));
  assert.equal(canSwapTwoHopRoute(toRelay), false);
  // Source-based relay hop is not list-pickable, so default picker settles on exit (1).
  assert.equal(resolveHopPickerIndex(toRelay, null, sources, usableIDs), 1);

  // Switching back to fixed node prefers activeNodeID.
  const backToNode = switchHopKind(toRelay, 0, "node", { sources, allNodes, usableIDs, activeNodeID: "hk2" });
  assert.deepEqual(backToNode[0], nodeHop("hk2"));

  // Custom-pool relay hop IS list-pickable and targeted first when incomplete.
  const incompleteCustom = [relayHop({ mode: "nodes", node_ids: ["hk1"] }), nodeHop("exit1")];
  assert.equal(resolveHopPickerIndex(incompleteCustom, null, sources, usableIDs), 0);
  assert.equal(nextHopPickerAfterNodePick([nodeHop("hk1"), nodeHop("")], 0, sources, usableIDs), 1);

  const claimed = nodesClaimedByOtherHops(incompleteCustom, 1, sources, usableIDs);
  assert.equal(claimed.get("hk1"), 0);
  assert.deepEqual(singleHopFromRoute(toRelay), [nodeHop("exit1")]);
});
