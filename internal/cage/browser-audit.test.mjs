import vm from "node:vm";
import test from "node:test";
import assert from "node:assert/strict";
import { auditBrowser } from "./browser-audit.mjs";

function browser(failMethod, failStage = "network") {
  const contexts = new Set(), excluded = new Set(), pages = new Map(), calls = [];
  let unsafeDisposal = false, stage = "settings";
  const observation = { timezone: "UTC", language: "ja-JP", platform: "Linux" };
  const cdp = async (method, params, sessionId) => {
    if (method === "Target.createBrowserContext") stage = "network";
    calls.push(method);
    if (method === failMethod && stage === failStage && !params?.expression?.includes("document.readyState"))
      throw new Error("injected failure");
    switch (method) {
      case "Target.createBrowserContext":
        contexts.add("audit-context");
        return { browserContextId: "audit-context" };
      case "Target.createTarget": {
        assert.equal(params.hidden, params.browserContextId ? true : undefined);
        assert.equal(params.background, true);
        assert.ok(excluded.has(params.browserContextId || params.url));
        const targetId = params.browserContextId ? "network-page" : "settings-page";
        pages.set(targetId, params.browserContextId || params.url);
        return { targetId };
      }
      case "Target.attachToTarget":
        return { sessionId: params.targetId + "-session" };
      case "Page.navigate":
        assert.equal(sessionId, "network-page-session", "only isolated pages navigate");
        return {};
      case "Runtime.evaluate":
        if (params.expression.includes("document.readyState")) return { result: { value: true } };
        if (sessionId === "network-page-session") {
          assert.ok(!params.expression.includes("navigator.language"));
          return { result: { value: { network: { status: 200 } } } };
        }
        assert.equal(sessionId, "settings-page-session");
        assert.ok(!params.expression.includes("document.cookie"));
        assert.ok(!params.expression.includes("collectDeviceObservation"), "routine audits must not probe devices");
        return { result: { value: observation } };
      case "Target.closeTarget":
        // Context disposal must wait for the page close to complete.
        await new Promise(resolve => setImmediate(resolve));
        pages.delete(params.targetId);
        return { success: true };
      case "Target.disposeBrowserContext":
        if ([...pages.values()].includes(params.browserContextId)) {
          unsafeDisposal = true;
          throw new Error("live hidden page references this context");
        }
        contexts.delete(params.browserContextId);
        return {};
      default:
        throw new Error("Unexpected command: " + method);
    }
  };
  return { cdp, excluded, contexts, pages, observation, calls, unsafe: () => unsafeDisposal };
}

test("successful audits close hidden pages before disposing network contexts", async () => {
  for (let i = 0; i < 3; i++) {
    const b = browser();
    assert.deepEqual(await auditBrowser(b.cdp, b.excluded, true, "UTC"), {
      ...b.observation, network: { status: 200 },
    });
    assert.equal(b.unsafe(), false);
    assert.equal(b.pages.size, 0);
    assert.equal(b.contexts.size, 0);
    assert.equal(b.excluded.size, 0);
  }
});

for (const method of ["Target.createTarget", "Target.attachToTarget", "Runtime.evaluate"]) {
  for (const stage of ["settings", "network"]) {
    test(method + " failure in " + stage + " releases its audit resources", async () => {
      const b = browser(method, stage);
      await assert.rejects(auditBrowser(b.cdp, b.excluded, true, "UTC"), /injected failure/);
      assert.equal(b.unsafe(), false);
      assert.equal(b.pages.size, 0);
      assert.equal(b.contexts.size, 0);
      assert.equal(b.excluded.size, 0);
    });
  }
}

test("a failed network page close preserves its context and keeps it excluded", async () => {
  const b = browser("Target.closeTarget");
  await auditBrowser(b.cdp, b.excluded, true, "UTC");
  assert.equal(b.unsafe(), false);
  assert.equal(b.pages.size, 1);
  assert.equal(b.contexts.size, 1);
  assert.equal(b.excluded.size, 1);
});

test("settings-only monitoring reads the normal profile without a network context", async () => {
  const b = browser();
  assert.deepEqual(await auditBrowser(b.cdp, b.excluded, false, "UTC"), b.observation);
  assert.equal(b.calls.includes("Target.createBrowserContext"), false);
  assert.equal(b.calls.includes("Page.navigate"), false);
  assert.equal(b.pages.size, 0);
  assert.equal(b.excluded.size, 0);
});

test("a settings page that cannot close stays excluded", async () => {
  const b = browser("Target.closeTarget", "settings");
  await auditBrowser(b.cdp, b.excluded, false, "UTC");
  assert.equal(b.pages.size, 1);
  assert.ok(b.excluded.has(b.pages.get("settings-page")));
});

test("local browser audit uses a syntactically valid async probe in the regular profile", async () => {
  let closed = false;
  const cdp = async (method, input) => {
    if (method === "Target.createTarget") return { targetId: "probe" };
    if (method === "Target.attachToTarget") return { sessionId: "isolated" };
    if (method === "Runtime.evaluate") {
      new vm.Script(input.expression);
      assert.equal(input.awaitPromise, true);
      assert(input.expression.includes("collectDeviceObservation"));
      return { result: { value: { timezone: "UTC", language: "en-US", platform: "Linux x86_64", device: { webgl: { state: "unavailable" } } } } };
    }
    if (method === "Target.closeTarget") { closed = true; return {}; }
    throw Error(`unexpected probe operation: ${method}`);
  };
  const result = await auditBrowser(cdp, new Set(), false, "UTC", true);
  assert.equal(result.device.webgl.state, "unavailable");
  assert.equal(closed, true);
});

test("settings audit falls back to a minimized window when no normal browser window is open", async () => {
  const excluded = new Set(), createParams = [];
  let closed = false;
  const observation = { timezone: "America/New_York", expected_timezone: "America/New_York", language: "en-US", languages: ["en-US"], platform: "Linux x86_64" };
  const cdp = async (method, params) => {
    if (method === "Target.createTarget") {
      assert.ok(excluded.has(params.url), "audit URL must be excluded before target creation");
      createParams.push(params);
      if (params.newWindow === false) {
        throw new Error("Failed to open new tab - no browser is open");
      }
      return { targetId: "minimized-probe" };
    }
    if (method === "Target.attachToTarget") {
      assert.equal(params.targetId, "minimized-probe");
      return { sessionId: "minimized-session" };
    }
    if (method === "Runtime.evaluate") return { result: { value: observation } };
    if (method === "Target.closeTarget") {
      assert.equal(params.targetId, "minimized-probe");
      closed = true;
      return { success: true };
    }
    throw new Error("Unexpected command: " + method);
  };
  const result = await auditBrowser(cdp, excluded, false, "America/New_York");
  assert.deepEqual(result, observation);
  assert.equal(createParams.length, 2);
  assert.equal(createParams[0].newWindow, false);
  assert.equal(createParams[0].background, true);
  assert.equal(createParams[1].newWindow, true);
  assert.equal(createParams[1].background, true);
  assert.equal(createParams[1].windowState, "minimized");
  assert.equal(createParams[1].url, createParams[0].url);
  assert.equal(closed, true);
  assert.equal(excluded.size, 0);
});

test("settings audit cleans up exclusions if minimized window fallback fails", async () => {
  const excluded = new Set();
  let calls = 0;
  const cdp = async (method, params) => {
    assert.equal(method, "Target.createTarget");
    calls++;
    if (params.newWindow === false) throw new Error("Failed to open new tab - no browser is open");
    throw new Error("window creation failed");
  };
  await assert.rejects(auditBrowser(cdp, excluded, false, "UTC"), /window creation failed/);
  assert.equal(calls, 2);
  assert.equal(excluded.size, 0);
});

test("settings audit does not open a new window on unrelated createTarget errors", async () => {
  const excluded = new Set();
  let calls = 0;
  const cdp = async () => {
    calls++;
    throw new Error("Browser command timed out: Target.createTarget");
  };
  await assert.rejects(auditBrowser(cdp, excluded, false, "UTC"), /timed out/);
  assert.equal(calls, 1);
  assert.equal(excluded.size, 0);
});
