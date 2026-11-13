import test from "node:test";
import assert from "node:assert/strict";
import { BrowserClipboard } from "./browser-clipboard.mjs";

const settle = () => new Promise(resolve => setImmediate(resolve));
async function fixture(options = {}) {
  const calls = [], copies = [];
  const observer = new BrowserClipboard(async (method, params, session) => {
    calls.push({ method, params, session });
    return { result: { value: true } };
  }, value => copies.push(value), { source: async () => "source", ...options });
  await observer.attach("tab-session", "tab");
  const state = observer.sessions.get("tab-session");
  const context = (id = 3, name = state.world) => observer.event({ sessionId: "tab-session", method: "Runtime.executionContextCreated",
    params: { context: { id, name, auxData: { frameId: "frame", isDefault: name === "" } } } });
  context();
  await observer.setContext("tab", "context-a");
  const gesture = (token = state.token, executionContextId = 3) => observer.event({ sessionId: "tab-session", method: "Runtime.bindingCalled",
    params: { name: state.binding, executionContextId, payload: JSON.stringify({ token, context: "context-a", epoch: 0 }) } });
  let serial = 0;
  const copied = (value = { text: "exact🙂" }, token = observer.clipboardSource.token, sessionId = "source") =>
    { const sequence = ++serial;
      for (const payload of [{ token, serial: sequence, begin: true }, { token, serial: sequence, ...value }])
        observer.event({ sessionId, method: "Runtime.bindingCalled", params: { name: observer.clipboardSource.name, payload: JSON.stringify(payload) } });
    };
  return { observer, state, calls, copies, context, gesture, copied };
}

test("clipboard input code and binding are restricted to an isolated world", async () => {
  const { state, calls } = await fixture();
  const injection = calls.find(call => call.method === "Page.addScriptToEvaluateOnNewDocument");
  assert.equal(injection.params.worldName, state.world);
  assert.equal(calls.find(call => call.method === "Runtime.addBinding" && call.session === "tab-session").params.executionContextName, state.world);
  assert.ok(!injection.params.source.includes("defineProperty"));
  assert.ok(!calls.some(call => call.method === "Browser.grantPermissions"));
});

test("copies require both a private source and trusted input from a live isolated context", async () => {
  const f = await fixture();
  f.context(4, "");
  f.copied();
  await settle();
  assert.deepEqual(f.copies, []);
  f.gesture("wrong");
  f.gesture(f.state.token, 4);
  f.copied();
  await settle();
  assert.deepEqual(f.copies, []);
  f.gesture();
  f.copied({}, "wrong");
  f.copied({ text: "forged" }, f.observer.clipboardSource.token, "tab-session");
  f.copied({ text: "x".repeat(65537) });
  f.copied();
  await settle();
  assert.deepEqual(f.copies, [{ id: "tab", context: "context-a", text: "exact🙂" }]);
  f.observer.event({ sessionId: "tab-session", method: "Page.frameNavigated", params: { frame: { id: "frame" } } });
  f.copied();
  await settle();
  assert.equal(f.copies.length, 1);
});

test("navigation or tab closure during source validation cancels the result", async () => {
  const f = await fixture();
  let release;
  f.observer.cdp = () => new Promise(resolve => { release = resolve; });
  f.gesture();
  f.copied();
  f.observer.detachTab("tab");
  release({ result: { value: true } });
  await settle();
  assert.deepEqual(f.copies, []);
  assert.equal(f.observer.copyContexts.size, 0);
  assert.equal(f.observer.sessions.size, 0);
});

test("native format changes require an enabled receiver", async () => {
  for (const includeChanges of [false, true]) {
    const f = await fixture({ includeChanges });
    f.gesture();
    f.copied({ changed: true });
    await settle();
    assert.deepEqual(f.copies, includeChanges ? [{ id: "tab", context: "context-a", changed: true }] : []);
  }
});

test("new frames inherit the host context and setup failures permit retry", async () => {
  const f = await fixture();
  const before = f.calls.length;
  f.context(9);
  await settle();
  assert.ok(f.calls.slice(before).some(call => call.method === "Runtime.evaluate" && call.params.contextId === 9 && call.params.expression.includes('"context-a"')));
  const cdp = f.observer.cdp;
  f.observer.cdp = async () => { throw new Error("CDP unavailable"); };
  await assert.rejects(f.observer.attach("child", "tab"), /CDP unavailable/);
  assert.equal(f.observer.sessions.has("child"), false);
  f.observer.cdp = cdp;
  await f.observer.attach("child", "tab");
  f.observer.sessions.get("child").contexts.set(10, "child-frame");
  await f.observer.setContext("tab", "context-b");
  assert.ok(f.calls.some(call => call.session === "child" && call.params.contextId === 10 && call.params.expression.includes('"context-b"')));
});

test("unavailable native notifications leave ordinary input usable", async () => {
  const observer = new BrowserClipboard(async () => ({}), () => {});
  await observer.attach("session", "tab");
  await observer.setContext("tab", "context");
  assert.equal(observer.available, false);
  assert.equal(observer.copyContexts.get("tab"), "context");
});
