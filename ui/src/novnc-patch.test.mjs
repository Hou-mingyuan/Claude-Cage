import test from "node:test";
import assert from "node:assert/strict";
import { cpSync, mkdtempSync, realpathSync, rmSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join, sep } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

// Applies internal/cage/novnc-patch.py to a private copy of the vendored noVNC
// and drives the patched RFB._handleMouse with the events a host WebView sends.
const root = realpathSync(tmpdir());
const work = mkdtempSync(join(root, "novnc-mouse-"));
assert(work.startsWith(root + sep));
test.after(() => rmSync(work, { recursive: true, force: true }));

function patchedNoVNC() {
  const target = join(work, "novnc");
  cpSync(new URL("../vendor/novnc", import.meta.url), target, { recursive: true });
  const python = process.env.PYTHON || (process.platform === "win32" ? "python" : "python3");
  try {
    execFileSync(python, [fileURLToPath(new URL("../../internal/cage/novnc-patch.py", import.meta.url)), target], { stdio: "pipe" });
  } catch (error) {
    if (error.code === "ENOENT") return null;
    throw error;
  }
  return target;
}

let loaded;
async function loadRFB(t) {
  loaded ||= importRFB();
  const RFB = await loaded;
  if (!RFB) t.skip("Python is not available to apply the noVNC patch");
  return RFB;
}

async function importRFB() {
  const target = patchedNoVNC();
  if (!target) return null;
  const noop = () => {};
  // Just enough of a browser for the module graph to load.
  Object.assign(globalThis, { window: globalThis, addEventListener: noop, removeEventListener: noop,
    MutationObserver: class { observe() {} disconnect() {} } });
  globalThis.document = { documentElement: {}, body: { appendChild: noop, removeChild: noop },
    addEventListener: noop, removeEventListener: noop, getElementById: () => null,
    createElement: () => ({ style: {}, getContext: () => ({}), appendChild: noop, remove: noop }) };
  const { log, error } = console;
  Object.assign(console, { log: noop, error: noop });
  try {
    return (await import(pathToFileURL(join(target, "core/rfb.js")).href)).default;
  } finally { Object.assign(console, { log, error }); }
}

function mouse(RFB) {
  const canvas = { setCapture() {}, getBoundingClientRect: () => ({ left: 0, top: 0, right: 800, bottom: 600, width: 800, height: 600 }) };
  const masks = [];
  const rfb = { _canvas: canvas, dragViewport: false, _handleMouseButton: (_x, _y, mask) => masks.push(mask), _handleMouseMove() {} };
  const send = (type, button, buttons) => RFB.prototype._handleMouse.call(rfb, {
    type, button, buttons, clientX: 40, clientY: 30, target: canvas, stopPropagation() {}, preventDefault() {},
  });
  return { masks, send };
}

test("a click is pressed and released even when WebKit reports stale buttons", async (t) => {
  const RFB = await loadRFB(t);
  if (!RFB) return;
  // macOS WebKit reads buttons from [NSEvent pressedMouseButtons] while it
  // handles the event: a quick tap or a synthesized click reads 0 on press.
  let { masks, send } = mouse(RFB);
  send("mousedown", 0, 0);
  send("mouseup", 0, 0);
  assert.deepEqual(masks, [1, 0]);
  // A release handled after the next press began still reads the button held.
  ({ masks, send } = mouse(RFB));
  send("mousedown", 0, 1);
  send("mouseup", 0, 1);
  assert.deepEqual(masks, [1, 0]);
  // Right and middle buttons use the RFB bit order.
  ({ masks, send } = mouse(RFB));
  send("mousedown", 2, 0);
  send("mouseup", 2, 0);
  send("mousedown", 1, 0);
  send("mouseup", 1, 0);
  assert.deepEqual(masks, [4, 0, 2, 0]);
});

test("accurate button state from Chromium and WebView2 is unchanged", async (t) => {
  const RFB = await loadRFB(t);
  if (!RFB) return;
  const { masks, send } = mouse(RFB);
  send("mousedown", 0, 1);
  send("mousedown", 2, 3);
  send("mouseup", 2, 1);
  send("mouseup", 0, 0);
  assert.deepEqual(masks, [1, 5, 1, 0]);
});

function paste(RFB) {
  const sent = [], keys = [], queue = [];
  const rfb = Object.create(RFB.prototype);
  Object.assign(rfb, {
    _rfbConnectionState: "connected", _viewOnly: false, _supportsFence: true,
    _clipboardServerCapabilitiesFormats: { 1: true },
    _clipboardServerCapabilitiesActions: { [1 << 27]: true, [1 << 28]: true },
    _cageModifiers: new Map([["ShiftLeft", 0xffe1]]),
    _cageSendKey: (sym, code, down) => keys.push([sym, down]),
    _sock: { rQwait: () => false, rQskipBytes() {}, rQshift32: () => queue.shift(), rQshift8: () => queue.shift(), rQshiftStr: () => queue.shift() },
  });
  const messages = RFB.messages, saved = {};
  for (const name of ["clientFence", "extendedClipboardNotify", "extendedClipboardProvide"]) {
    saved[name] = messages[name];
    messages[name] = (_sock, ...args) => sent.push([name, ...args]);
  }
  return {
    rfb, sent, keys,
    restore: () => Object.assign(messages, saved),
    // The server acknowledges a client fence (without the request flag).
    fence(payload) { queue.push(1, payload.length, payload); assert.equal(rfb._handleServerFenceMsg(), true); },
    // An X client in the guest reads the clipboard; Xvnc asks for the text.
    request() { queue.push(0xfffffffc, (1 << 25) | 1); assert.equal(rfb._handleServerCutText(), true); },
  };
}

const ctrlV = [[0xffe1, false], [0xffe3, true], [0x76, true], [0x76, false], [0xffe3, false], [0xffe1, true]];

test("a paste needs no clipboard reader in the guest before Ctrl+V", async (t) => {
  const RFB = await loadRFB(t);
  if (!RFB) return;
  const p = paste(RFB);
  t.after(p.restore);
  let done = false;
  const result = p.rfb._cagePasteText("@").then(() => { done = true; });
  assert.deepEqual(p.sent.map(([name, , value]) => [name, value]), [["extendedClipboardNotify", undefined], ["clientFence", "cage-paste-1"]]);
  // Xvnc only asks for the data when an application reads it: paste first.
  p.fence("cage-paste-1");
  assert.deepEqual(p.keys, ctrlV);
  p.request();
  assert.deepEqual(p.sent.slice(2).map(([name, , value]) => [name, value]), [["extendedClipboardProvide", ["@"]], ["clientFence", "cage-paste-1-keys"]]);
  p.fence("cage-paste-1-keys");
  assert.equal(done, false);
  await result;
  assert.equal(p.rfb._cagePasteJob, null);
});

test("an early clipboard reader does not pre-empt the paste keys", async (t) => {
  const RFB = await loadRFB(t);
  if (!RFB) return;
  const p = paste(RFB);
  t.after(p.restore);
  let done = false;
  const result = p.rfb._cagePasteText("_").then(() => { done = true; });
  // A guest clipboard watcher reads as soon as the owner changes.
  p.request();
  assert.deepEqual(p.keys, []);
  assert.deepEqual(p.sent.at(-1).slice(0, 1), ["extendedClipboardProvide"]);
  p.fence("cage-paste-1");
  assert.deepEqual(p.keys, ctrlV);
  // Xvnc (SConnection::requestClipboard, vncSelection's cache) now holds the
  // text and answers the application's read itself: no second request comes.
  // The keys are confirmed at once instead of waiting for one.
  assert.deepEqual(p.sent.at(-1).slice(2), ["cage-paste-1-keys"]);
  p.fence("cage-paste-1-keys");
  assert.equal(done, false);
  await result;
  assert.equal(p.rfb._cagePasteJob, null);
});

test("queued input after an early-read paste is delivered, not dropped", async (t) => {
  const RFB = await loadRFB(t);
  if (!RFB) return;
  const p = paste(RFB);
  t.after(p.restore);
  const order = [];
  const first = p.rfb.cagePasteText("a").then(() => order.push("paste"), e => order.push("fail " + e.message));
  const enter = p.rfb._cageQueueInput(() => order.push("enter"));
  await new Promise(resolve => setImmediate(resolve));
  p.request();
  p.fence("cage-paste-1");
  p.fence("cage-paste-1-keys");
  await first;
  await enter;
  assert.deepEqual(order, ["paste", "enter"]);
});

test("a reader racing the application's read still completes once", async (t) => {
  const RFB = await loadRFB(t);
  if (!RFB) return;
  const p = paste(RFB);
  t.after(p.restore);
  const result = p.rfb._cagePasteText("z");
  p.fence("cage-paste-1");
  // Both reads arrive after Ctrl+V while Xvnc had no data yet.
  p.request();
  p.request();
  assert.deepEqual(p.sent.filter(([name, , value]) => name === "clientFence" && value === "cage-paste-1-keys").length, 1);
  p.fence("cage-paste-1-keys");
  await result;
});
