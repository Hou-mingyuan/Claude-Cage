import assert from "node:assert/strict";
import test from "node:test";
import http from "node:http";
import { browserCallback, nativeBrowserDispatch } from "../macos-browser-window.mjs";

const token = "ab".repeat(12);
const post = (route, input) => ({ method: "POST", path: route, input });
const get = (route, input) => ({ method: "GET", path: route, input });

function fixture() {
  let login = false, key = "first", surface = "browser";
  const calls = [];
  const dispatch = nativeBrowserDispatch({
    window: async (action, args) => {
      calls.push({ action, ...args });
      if (action === "status") return { installed: true, surface, login, profile_pid: 123 };
      if (action === "finish-login") login = false;
      if (action === "launch" && args.login) login = true;
      if (action === "launch" && args.url?.startsWith("http://198.18.0.254/browser-check#")) {
        const options = new URLSearchParams(new URL(args.url).hash.slice(1));
        const report = new URL(options.get("report"));
        report.searchParams.set("result", JSON.stringify({ language: "fr", timezone: "UTC", expected_timezone: options.get("zone"), platform: "MacIntel", network: { status: 200, ip: "192.0.2.1" } }));
        const response = await fetch(report);
        assert.equal(response.status, 200);
      }
      return { surface };
    },
    settingsInputs: async () => key,
  });
  return { calls, dispatch, setKey: value => { key = value; }, setSurface: value => { surface = value; } };
}

test("native login focuses the workbench once for each subscriber without exposing the authorization URL", async () => {
  const f = fixture();
  const url = "https://claude.ai/oauth/authorize?state=private";
  const previous = await f.dispatch(get("/events", { after: 0 }));
  await f.dispatch(post("/login-open", { login: token, url }));
  const update = await f.dispatch(get("/events", { after: previous.sequence }));
  assert.ok(update.state.focus && update.state.tabs[0].login);
  assert.ok(!JSON.stringify(update).includes("private"));
  assert.equal((await f.dispatch(get("/events", { after: update.sequence }))).state.focus, undefined);
  const statusCalls = f.calls.filter(c => c.action === "status").length;
  for (let poll = 0; poll < 10; poll++) {
    assert.equal((await f.dispatch(get("/events", { onlyChanges: true, after: update.sequence }))).state, undefined);
  }
  assert.equal(f.calls.filter(c => c.action === "status").length, statusCalls);
  await assert.rejects(f.dispatch(post("/audit", { timezone: "UTC", network: true })), { code: "login_active" });
  assert.ok((await f.dispatch(get("/events", { after: previous.sequence }))).state.focus);
  await f.dispatch(post("/login-finish", { login: token }));
  assert.equal((await f.dispatch(get("/tabs"))).tabs[0].login, false);
  await assert.rejects(f.dispatch(post("/login-open", { login: token, url: "https://example.org" })));
  await assert.rejects(f.dispatch(post("/new", { incognito: true })));
  f.setSurface(undefined);
  await assert.rejects(f.dispatch(post("/new", { url })));
  assert.equal(f.calls.filter(c => c.action === "launch").length, 1);
});

test("background recheck requires the last page sample and detects changed inputs without opening a tab", async () => {
  const f = fixture();
  const recheck = post("/audit-recheck", { timezone: "UTC", language: "fr" });
  await assert.rejects(f.dispatch(recheck));
  const result = await f.dispatch(post("/audit", { timezone: "UTC", network: true }));
  assert.equal(result.network.status, 200);
  assert.deepEqual(await f.dispatch(recheck), { unchanged: true });
  await assert.rejects(f.dispatch(post("/audit-recheck", { timezone: "UTC", language: "en-US" })));
  f.setKey("different process");
  await assert.rejects(f.dispatch(recheck));
  assert.equal(f.calls.filter(c => c.action === "launch").length, 1);
});

test("loopback callback accepts only the issued authority and path once", async () => {
  let seen = 0;
  const cb = await browserCallback("127.0.0.1", (_, response) => { seen++; response.end("done"); });
  try {
    const url = new URL(cb.url);
    assert.equal((await fetch(url.origin)).status, 404);
    const wrongHost = await new Promise((resolve, reject) => {
      http.get(url, { headers: { Host: "other.localhost" } }, response => {
        response.resume(); resolve(response.statusCode);
      }).on("error", reject);
    });
    assert.equal(wrongHost, 404);
    assert.equal((await fetch(url)).status, 200);
    await cb.result;
    assert.equal(seen, 1);
  } finally { cb.close(); }
});
