import assert from "node:assert/strict";
import test from "node:test";
import {
  RELAY_STRATEGY_FALLBACK,
  RELAY_STRATEGY_URL_TEST,
  bulkToggleNodesInRelayHop,
  canSwapTwoHopRoute,
  hasConfiguredRelays,
  hopsEqual,
  isMultiNodeHop,
  moveNodeInRelayHop,
  nodeHop,
  nodesClaimedByOtherHops,
  relayHop,
  reorderNodeInRelayHop,
  resolveHopPickerIndex,
  restoreChainRoute,
  sanitizeRelayHopAfterExitChange,
  savedHopsFromProxy,
  serializeHopsForAPI,
  singleHopFromRoute,
  sourceUsableRelayCount,
  swapTwoHopRoute,
  toggleNodeInRelayHop,
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
const usableIDs = new Set(["hk1", "hk2", "jp1", "exit1"]);

test("unified relay hop treats 1 node as fixed and >=2 or source as auto-switch group", () => {
  const fixedHops = savedHopsFromProxy({ route: ["hk1", "exit1"], selected: "exit1" });
  assert.deepEqual(fixedHops, [relayHop({ node_ids: ["hk1"] }), nodeHop("exit1")]);
  assert.equal(isMultiNodeHop(fixedHops[0]), false);
  // 1-node transit serializes to plain node_ids (backward-compatible).
  assert.deepEqual(serializeHopsForAPI(fixedHops), { node_ids: ["hk1", "exit1"] });
  assert.equal(validateRouteHops(fixedHops, sources, usableIDs).valid, true);

  // Adding a 2nd node seamlessly promotes to an auto-switch group.
  const multiHops = toggleNodeInRelayHop(fixedHops, 0, "hk2", sources, usableIDs);
  assert.equal(isMultiNodeHop(multiHops[0]), true);
  assert.deepEqual(serializeHopsForAPI(multiHops), {
    hops: [
      { relay: { strategy: RELAY_STRATEGY_FALLBACK, node_ids: ["hk1", "hk2"] } },
      { node_id: "exit1" },
    ],
  });

  // Removing hk2 seamlessly demotes back to single fixed node.
  const backToSingle = toggleNodeInRelayHop(multiHops, 0, "hk2", sources, usableIDs);
  assert.equal(isMultiNodeHop(backToSingle[0]), false);
  assert.deepEqual(serializeHopsForAPI(backToSingle), { node_ids: ["hk1", "exit1"] });
  assert.equal(hopsEqual(fixedHops, backToSingle), true);
});

test("whole-source relay hop materializes cleanly when user unchecks one node", () => {
  const sourceRoute = [relayHop({ strategy: RELAY_STRATEGY_URL_TEST, source_id: "airport" }), nodeHop("exit1")];
  assert.equal(isMultiNodeHop(sourceRoute[0]), true);
  assert.deepEqual(serializeHopsForAPI(sourceRoute), {
    hops: [
      { relay: { strategy: RELAY_STRATEGY_URL_TEST, source_id: "airport" } },
      { node_id: "exit1" },
    ],
  });

  // Unchecking jp1 converts to custom pool [hk1, hk2] keeping strategy.
  const narrowed = toggleNodeInRelayHop(sourceRoute, 0, "jp1", sources, usableIDs);
  assert.equal(narrowed[0].source_id, "");
  assert.deepEqual(narrowed[0].node_ids, ["hk1", "hk2"]);
  assert.equal(narrowed[0].strategy, RELAY_STRATEGY_URL_TEST);
});

test("priority reorder, bulk toggle, swap, and exit sanitize work cleanly", () => {
  let hops = [relayHop({ node_ids: [] }), nodeHop("exit1")];
  hops = bulkToggleNodesInRelayHop(hops, 0, ["hk1", "hk2", "jp1"], true, sources, usableIDs);
  assert.deepEqual(hops[0].node_ids, ["hk1", "hk2", "jp1"]);

  hops = moveNodeInRelayHop(hops, 0, "jp1", -1);
  assert.deepEqual(hops[0].node_ids, ["hk1", "jp1", "hk2"]);
  hops = reorderNodeInRelayHop(hops, 0, "hk2", "hk1");
  assert.deepEqual(hops[0].node_ids, ["hk2", "hk1", "jp1"]);
  hops = reorderNodeInRelayHop(hops, 0, "hk2", "jp1");
  assert.deepEqual(hops[0].node_ids, ["hk1", "jp1", "hk2"]);
  hops = moveNodeInRelayHop(hops, 0, "hk2", -Infinity);
  assert.deepEqual(hops[0].node_ids, ["hk2", "hk1", "jp1"]);

  assert.equal(canSwapTwoHopRoute(hops), false);
  const singleTransit = [relayHop({ node_ids: ["hk1"] }), nodeHop("exit1")];
  assert.equal(canSwapTwoHopRoute(singleTransit), true);
  assert.deepEqual(swapTwoHopRoute(singleTransit), [relayHop({ node_ids: ["exit1"] }), nodeHop("hk1")]);

  const sanitized = sanitizeRelayHopAfterExitChange([
    relayHop({ node_ids: ["hk1", "jp1", "hk2"] }),
    nodeHop("jp1"),
  ], "jp1");
  assert.deepEqual(sanitized[0].node_ids, ["hk1", "hk2"]);
  assert.deepEqual(singleHopFromRoute(hops), [nodeHop("exit1")]);
});

test("same-source exit is allowed and picker focuses missing hop first", () => {
  const sourceRoute = [relayHop({ source_id: "airport" }), nodeHop("jp1")];
  const val = validateRouteHops(sourceRoute, sources, usableIDs);
  assert.equal(val.valid, true);
  assert.deepEqual(val.candidateCounts, [2, 1]);

  // Source-based relay does not block selecting an exit from that source.
  const exitClaimed = nodesClaimedByOtherHops(sourceRoute, 1, sources, usableIDs);
  assert.equal(exitClaimed.size, 0);

  // Empty transit focuses transit (0); filled transit + empty exit focuses exit (1).
  assert.equal(resolveHopPickerIndex([relayHop(), nodeHop("exit1")], null, sources, usableIDs), 0);
  assert.equal(resolveHopPickerIndex([relayHop({ node_ids: ["hk1"] }), nodeHop("")], null, sources, usableIDs), 1);

  // sourceUsableRelayCount does not subtract the node currently selected in targetIndex itself.
  const singleFromAirport = [relayHop({ node_ids: ["hk1"] }), nodeHop("exit1")];
  assert.equal(sourceUsableRelayCount(sources[0], singleFromAirport, 0, usableIDs, sources), 3);
});

test("switching single mode and back preserves remembered relays without exit conflicts", () => {
  const customChain = [
    relayHop({ strategy: RELAY_STRATEGY_URL_TEST, node_ids: ["hk1", "hk2", "jp1"] }),
    nodeHop("exit1"),
  ];
  assert.equal(hasConfiguredRelays(customChain), true);
  assert.equal(hasConfiguredRelays([relayHop(), nodeHop("exit1")]), false);

  const single = singleHopFromRoute(customChain);
  assert.deepEqual(single, [nodeHop("exit1")]);

  // Switching back preserves relay pool and strategy identically.
  const restoredSameExit = restoreChainRoute(single[0], customChain.slice(0, -1), sources, usableIDs);
  assert.equal(hopsEqual(restoredSameExit, customChain), true);

  // If exit was changed to one of the relay nodes while in single mode, sanitize removes the duplicate.
  const restoredOverlapExit = restoreChainRoute(nodeHop("hk2"), customChain.slice(0, -1), sources, usableIDs);
  assert.deepEqual(restoredOverlapExit, [
    relayHop({ strategy: RELAY_STRATEGY_URL_TEST, node_ids: ["hk1", "jp1"] }),
    nodeHop("hk2"),
  ]);

  // Whole-subscription relay is also preserved across mode switch, and dropped cleanly if source was removed.
  const sourceRelays = [relayHop({ strategy: RELAY_STRATEGY_FALLBACK, source_id: "airport" })];
  assert.deepEqual(
    restoreChainRoute(nodeHop("exit1"), sourceRelays, sources, usableIDs),
    [relayHop({ strategy: RELAY_STRATEGY_FALLBACK, source_id: "airport" }), nodeHop("exit1")],
  );
  assert.deepEqual(
    restoreChainRoute(nodeHop("exit1"), [relayHop({ source_id: "deleted-source" })], sources, usableIDs),
    [relayHop(), nodeHop("exit1")],
  );
});
