import assert from "node:assert/strict";
import { test } from "node:test";
import { checkedBrowserProfile, browserProfileFlags } from "../browser-protocol.mjs";
import { nativeBrowserDispatch } from "../macos-browser-window.mjs";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";

const options = { browser_width: 1280, browser_height: 800, webgl: "disabled", canvas: "blocked" };
test("creation preferences accept only bounded typed fields", () => {
  assert.deepEqual(checkedBrowserProfile(options), options);
  for (const change of [{ browser_width: 0 }, { browser_height: "800" }, { webgl: "spoof" }, { canvas: "random" }, { args: ["--no-sandbox"] }, { proxy: "http://elsewhere" }])
    assert.throws(() => checkedBrowserProfile({ ...options, ...change }), { code: "invalid_configuration" });
  assert.deepEqual(browserProfileFlags(options), ["--window-size=1280,800", "--disable-3d-apis", "--disable-webgl", "--disable-reading-from-canvas"]);
  assert.deepEqual(browserProfileFlags({ ...options, webgl: "native", canvas: "native" }), ["--window-size=1280,800"]);
});
test("real graphics APIs are selected only for compatible guest platforms", () => {
  const native = { ...options, webgl: "native", canvas: "native" };
  assert.deepEqual(browserProfileFlags({ ...native, graphics_backend: "metal" }, "darwin"),
    ["--window-size=1280,800", "--use-gl=angle", "--use-angle=metal", "--enable-zero-copy"]);
  assert.deepEqual(browserProfileFlags({ ...native, graphics_backend: "d3d11" }, "win32"),
    ["--window-size=1280,800", "--use-gl=angle", "--use-angle=d3d11", "--enable-zero-copy"]);
  assert.deepEqual(browserProfileFlags({ ...native, graphics_backend: "vulkan" }, "linux"),
    ["--window-size=1280,800", "--use-gl=angle", "--use-angle=vulkan", "--enable-zero-copy"]);
  assert.deepEqual(browserProfileFlags({ ...native, graphics_backend: "opengl" }, "linux"),
    ["--window-size=1280,800", "--use-gl=angle", "--use-angle=gl", "--enable-zero-copy"]);
  assert.deepEqual(browserProfileFlags({ ...native, graphics_backend: "software" }, "linux"),
    ["--window-size=1280,800", "--use-gl=angle", "--use-angle=swiftshader"]);
  assert.deepEqual(browserProfileFlags({ ...native, graphics_backend: "auto" }, "linux"),
    ["--window-size=1280,800"]);
  assert.throws(() => browserProfileFlags({ ...native, graphics_backend: "metal" }, "win32"), { code: "invalid_configuration" });
  assert.throws(() => checkedBrowserProfile({ ...options, graphics_backend: "metal" }), { code: "invalid_configuration" });
  assert.deepEqual(checkedBrowserProfile({ ...native, graphics_backend: "auto" }), native);
});
test("macOS creation settings are stored before opening a browser window", async () => {
  let saved;
  const dispatch = nativeBrowserDispatch({ profile: value => { saved = checkedBrowserProfile(value); }, window: () => { throw Error("window must stay closed"); } });
  assert.deepEqual(await dispatch({ method: "POST", path: "/profile", input: options }), { ok: true });
  assert.deepEqual(saved, options);
});
test("Windows saves creation choices before browser startup and reuses them on reconnect", async () => {
  const { createWindowsBrowserService } = await import("../windows-browser.mjs");
  const directory = await mkdtemp(path.join(tmpdir(), "browser-profile-test-"));
  let launches = 0;
  const create = () => createWindowsBrowserService({ directory, testing: true, chromium: { launchPersistentContext() { launches++; throw Error("unexpected browser startup"); } } });
  let service = create();
  try {
    const request = value => service.request({ method: "POST", path: "/profile", input: value });
    assert.deepEqual(await request(options), { ok: true });
    assert.deepEqual(JSON.parse(await readFile(path.join(directory, "guest-profile.json"), "utf8")), options);
    assert.deepEqual(await request(options), { ok: true });
    await assert.rejects(request({ ...options, webgl: "native" }), { code: "invalid_configuration" });
    await service.close(); service = create();
    assert.deepEqual(await request(options), { ok: true });
    assert.equal(launches, 0);
  } finally { await service.close(); await rm(directory, { recursive: true, force: true }); }
});

test("browser startup uses the persisted backend and respects Chromium rasterization policy", async t => {
  const { createWindowsBrowserService } = await import("../windows-browser.mjs");
  const hardware = { darwin: "metal", win32: "d3d11", linux: "vulkan" }[process.platform];
  for (const backend of [hardware, "software", "auto"]) {
    await t.test(backend, async () => {
      const directory = await mkdtemp(path.join(tmpdir(), "browser-graphics-test-"));
      let launched;
      const create = () => createWindowsBrowserService({ directory, testing: true, executablePath: "fixture-browser", modules: {}, chromium: {
        launchPersistentContext(_directory, options) {
          launched = options;
          throw Object.assign(new Error("captured launch options"), { code: "launch_captured" });
        },
      } });
      let service = create();
      try {
        await service.request({ method: "POST", path: "/profile", input: { ...options, webgl: "native", graphics_backend: backend } });
        await service.close();
        service = create();
        await service.request({ operation: "configure", input: { proxy: "http://127.0.0.1:18501" } });
        await assert.rejects(service.request({ method: "GET", path: "/tabs", input: {} }), { code: "launch_captured" });
        assert.equal(launched.args.includes("--enable-zero-copy"), backend === hardware);
        assert.ok(!launched.args.includes("--enable-gpu-rasterization"));
        assert.ok(!launched.args.includes("--enable-accelerated-2d-canvas"));
        assert.ok(!launched.args.includes("--ignore-gpu-blocklist"));
        assert.equal(launched.chromiumSandbox, true);
        assert.ok(launched.args.includes("--window-size=1280,800"));
        assert.equal(launched.args.includes("--use-angle=swiftshader"), backend === "software");
      } finally { await service.close(); await rm(directory, { recursive: true, force: true }); }
    });
  }
});
