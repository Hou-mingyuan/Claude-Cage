import test from "node:test";
import assert from "node:assert/strict";
import { detectNodeRegion, groupNodesByRegion, regionFlag } from "./proxy-region.mjs";

test("detectNodeRegion recognizes flags, Chinese/English cities, and bounded codes", () => {
  assert.equal(detectNodeRegion("🇭🇰 香港 01 | 1x HK"), "HK");
  assert.equal(detectNodeRegion("香港家宽 02 | 1x HK"), "HK");
  assert.equal(detectNodeRegion("日本东京 IEPL 03"), "JP");
  assert.equal(detectNodeRegion("NRT-01 Premium"), "JP");
  assert.equal(detectNodeRegion("Los Angeles Residential 04"), "US");
  assert.equal(detectNodeRegion("SG01 · 1x"), "SG");
  assert.equal(detectNodeRegion("UK-London-02"), "GB");
  assert.equal(detectNodeRegion("法兰克福 01"), "DE");
  assert.equal(detectNodeRegion("台北家宽 01"), "TW");
  assert.equal(detectNodeRegion("首尔高速 02"), "KR");
});

test("detectNodeRegion avoids false positives on generic English words", () => {
  assert.equal(detectNodeRegion("Using fast cloud node"), "ZZ");
  assert.equal(detectNodeRegion("Direct relay in office"), "ZZ");
  assert.equal(detectNodeRegion("Residential proxy 01"), "ZZ");
  assert.equal(detectNodeRegion("Manual SOCKS5"), "ZZ");
});

test("groupNodesByRegion groups by region count descending with unknown last", () => {
  const nodes = [
    { id: "1", name: "🇺🇸 US-01" },
    { id: "2", name: "🇭🇰 香港 01" },
    { id: "3", name: "Manual Relay" },
    { id: "4", name: "🇭🇰 香港 02" },
    { id: "5", name: "🇭🇰 香港 03" },
    { id: "6", name: "🇯🇵 Tokyo 01" },
    { id: "7", name: "🇺🇸 Seattle 02" },
  ];
  const groups = groupNodesByRegion(nodes, "zh-CN");
  assert.deepEqual(
    groups.map((g) => [g.code, g.nodes.length, g.flag]),
    [
      ["HK", 3, regionFlag("HK")],
      ["US", 2, regionFlag("US")],
      ["JP", 1, regionFlag("JP")],
      ["ZZ", 1, ""],
    ],
  );
});
