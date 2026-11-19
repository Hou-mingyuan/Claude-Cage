import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import test from "node:test";
import { BrowserWheel, createActivitySignal, createPointerProbe, wheelInput } from "./browser-wheel.mjs";

const settle = () => new Promise(resolve => setImmediate(resolve));

// A page at twice the density, with the given zoom.
function page({ zoom = 1, density = 2 } = {}) {
  const calls = [], answers = [];
  const cdp = (method, params, session) => {
    calls.push({ method, params, session });
    if (method === "Page.getLayoutMetrics") {
      return Promise.resolve({
        layoutViewport: { clientWidth: 2000 },
        cssLayoutViewport: { clientWidth: 2000 / (density * zoom) },
        cssVisualViewport: { zoom },
      });
    }
    return new Promise((resolve, reject) => answers.push({ resolve, reject }));
  };
  const wheels = () => calls.filter(call => call.method === "Input.dispatchMouseEvent");
  return { cdp, calls, answers, wheels };
}

function fixture(options = {}) {
  const browser = page(options), state = { session: "s1", over: true, now: 1000, probes: 0, acted: 0 };
  const wheel = new BrowserWheel({
    cdp: browser.cdp,
    target: async () => state.session,
    pointer: { overPage: async () => { state.probes++; return state.over; } },
    activity: { acted: () => state.acted++ },
    now: () => state.now,
  });
  return { ...browser, state, wheel };
}

test("a wheel names a position and a distance in whole framebuffer pixels", () => {
  assert.deepEqual(wheelInput({ x: 0, y: 8192, deltaX: -8192, deltaY: 1, extra: true }), { x: 0, y: 8192, deltaX: -8192, deltaY: 1 });
  for (const input of [undefined, {}, { x: 1, y: 1, deltaX: 0, deltaY: 0 }, { x: -1, y: 1, deltaX: 0, deltaY: 1 },
    { x: 1, y: 8193, deltaX: 0, deltaY: 1 }, { x: 1, y: 1, deltaX: 0.5, deltaY: 1 }, { x: 1, y: 1, deltaX: 0, deltaY: 8193 },
    { x: "1", y: 1, deltaX: 0, deltaY: 1 }, { x: 1, y: 1, deltaY: 1 }]) {
    assert.throws(() => wheelInput(input), /Invalid wheel input/);
  }
});

test("the page scrolls by the viewer's distance at its own density", async () => {
  const { wheel, wheels, state } = fixture();
  assert.deepEqual(await wheel.deliver({ x: 800, y: 600, deltaX: 0, deltaY: 120 }), { handled: true });
  assert.deepEqual(wheels(), [{ method: "Input.dispatchMouseEvent", session: "s1",
    params: { type: "mouseWheel", x: 400, y: 300, deltaX: 0, deltaY: 60 } }]);
  assert.equal(state.acted, 1);
});

test("a zoomed page is hit where the pointer is and moves as far as the fingers", async () => {
  const { wheel, wheels } = fixture({ zoom: 1.25 });
  await wheel.deliver({ x: 1000, y: 500, deltaX: -50, deltaY: 100 });
  // Positions are CSS pixels; the distance does not change with the zoom.
  assert.deepEqual(wheels()[0].params, { type: "mouseWheel", x: 400, y: 200, deltaX: -25, deltaY: 50 });
});

test("distances that arrive while the page is busy follow as one", async () => {
  const { wheel, wheels, answers } = fixture();
  await wheel.deliver({ x: 10, y: 10, deltaX: 0, deltaY: 20 });
  await wheel.deliver({ x: 12, y: 14, deltaX: 0, deltaY: 30 });
  await wheel.deliver({ x: 20, y: 24, deltaX: 4, deltaY: 50 });
  assert.equal(wheels().length, 1);
  answers.shift().resolve({});
  await settle();
  assert.deepEqual(wheels()[1].params, { type: "mouseWheel", x: 10, y: 12, deltaX: 2, deltaY: 40 });
  answers.shift().resolve({});
  await settle();
  assert.equal(wheels().length, 2);
});

test("no single page under the pointer declines the distance", async () => {
  const { wheel, wheels, state } = fixture();
  state.session = null;
  assert.deepEqual(await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 9 }), { handled: false });
  state.session = "s1";
  state.over = false;
  assert.deepEqual(await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 9 }), { handled: false });
  assert.equal(wheels().length, 0);
  assert.equal(state.acted, 0);
});

test("the pointer is looked up once for wheels that follow closely", async () => {
  const { wheel, state, answers } = fixture();
  await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 9 });
  state.now += 100;
  await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 9 });
  assert.equal(state.probes, 1);
  answers.shift().resolve({});
  await settle();
  answers.shift().resolve({});
  await settle();
  // A menu that opened meanwhile is noticed with the next look.
  state.now += 400;
  state.over = false;
  assert.deepEqual(await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 9 }), { handled: false });
  assert.equal(state.probes, 2);
});

test("a page that stopped answering collects no distance", async () => {
  const { wheel, wheels, answers, state } = fixture();
  await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 9 });
  state.now += 100;
  await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 500 });
  state.now += 400;
  assert.deepEqual(await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 500 }), { handled: false });
  // Once it answers, the page continues from where the viewer is now.
  answers.shift().resolve({});
  await settle();
  assert.equal(wheels().length, 1);
  assert.deepEqual(await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 8 }), { handled: true });
  assert.equal(wheels()[1].params.deltaY, 4);
});

test("a failed delivery declines for a moment and then resumes", async () => {
  const { wheel, wheels, answers, state } = fixture();
  await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 9 });
  await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 90 });
  answers.shift().reject(new Error("Session closed"));
  await settle();
  assert.equal(wheels().length, 1);
  assert.deepEqual(await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 9 }), { handled: false });
  state.now += 1100;
  assert.deepEqual(await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 10 }), { handled: true });
  assert.equal(wheels()[1].params.deltaY, 5);
});

test("another tab starts from its own distance", async () => {
  const { wheel, wheels, answers, state } = fixture();
  await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 10 });
  await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 300 });
  state.session = "s2";
  await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 20 });
  answers.shift().resolve({});
  await settle();
  assert.deepEqual([wheels()[1].session, wheels()[1].params.deltaY], ["s2", 10]);
});

test("page metrics are read again after a while, without holding the scroll", async () => {
  const { wheel, calls, state, answers } = fixture();
  const metrics = () => calls.filter(call => call.method === "Page.getLayoutMetrics").length;
  const scrolled = async () => {
    assert.deepEqual(await wheel.deliver({ x: 1, y: 1, deltaX: 0, deltaY: 10 }), { handled: true });
    answers.shift().resolve({});
    await settle();
  };
  await scrolled();
  state.now += 200;
  await scrolled();
  assert.equal(metrics(), 1);
  state.now += 600;
  await scrolled();
  assert.equal(metrics(), 2);
  wheel.forget("s1");
  await scrolled();
  assert.equal(metrics(), 3);
});

function probeProcess() {
  const child = new EventEmitter(), lines = [];
  child.stdout = Object.assign(new EventEmitter(), { setEncoding() {} });
  child.stdin = Object.assign(new EventEmitter(), { write: line => lines.push(line.trim()) });
  child.kill = () => { child.killed = true; };
  return { child, lines, say: text => child.stdout.emit("data", text) };
}

test("the pointer rests on the page when its window covers the screen", async () => {
  const xdotool = probeProcess();
  const probe = createPointerProbe({ start: () => xdotool.child });
  const answer = probe.overPage();
  await settle();
  assert.deepEqual(xdotool.lines, ["getmouselocation --shell"]);
  xdotool.say("X=300\nY=600\nSCR");
  xdotool.say("EEN=0\nWINDOW=4194675\n");
  await settle();
  assert.equal(xdotool.lines[1], "getwindowgeometry --shell 4194675");
  xdotool.say("WINDOW=4194675\nX=0\nY=0\nWIDTH=2304\nHEIGHT=1576\nSCREEN=0\n");
  await settle();
  assert.equal(xdotool.lines[2], "getdisplaygeometry --shell");
  xdotool.say("WIDTH=2304\nHEIGHT=1576\n");
  assert.equal(await answer, true);
});

test("a menu or a list under the pointer is not the page", async () => {
  const xdotool = probeProcess();
  const probe = createPointerProbe({ start: () => xdotool.child });
  const answer = probe.overPage();
  await settle();
  xdotool.say("X=300\nY=600\nSCREEN=0\nWINDOW=6291503\n");
  await settle();
  xdotool.say("WINDOW=6291503\nX=750\nY=90\nWIDTH=804\nHEIGHT=102\nSCREEN=0\n");
  await settle();
  xdotool.say("WIDTH=2304\nHEIGHT=1576\n");
  assert.equal(await answer, false);
});

test("a window that closed, or a silent process, answers no and starts anew", async () => {
  const first = probeProcess(), second = probeProcess(), started = [first, second];
  const probe = createPointerProbe({ start: () => started.shift().child, timeout: 30 });
  let answer = probe.overPage();
  await settle();
  first.say("X=1\nY=1\nSCREEN=0\nWINDOW=77\n");
  await settle();
  // xdotool ends on a window that no longer exists.
  first.child.emit("close");
  assert.equal(await answer, false);
  assert.equal(first.child.killed, true);
  answer = probe.overPage();
  await settle();
  assert.deepEqual(second.lines, ["getmouselocation --shell"]);
  assert.equal(await answer, false);
  assert.equal(second.child.killed, true);
});

test("delivered scrolling is reported to the display, a few times a second", () => {
  const state = { now: 0, written: 0, opened: 0 }, sockets = [];
  const connect = () => {
    state.opened++;
    const socket = Object.assign(new EventEmitter(), { write: () => state.written++, destroy() { this.destroyed = true; } });
    sockets.push(socket);
    return socket;
  };
  const signal = createActivitySignal({ path: "/run/a.sock", connect, interval: 250, now: () => state.now });
  signal.acted();
  state.now = 100;
  signal.acted();
  assert.deepEqual([state.opened, state.written], [1, 1]);
  state.now = 260;
  signal.acted();
  assert.deepEqual([state.opened, state.written], [1, 2]);
  // A display that restarted is reached again with the next report.
  sockets[0].emit("error", new Error("EPIPE"));
  state.now = 600;
  signal.acted();
  assert.deepEqual([state.opened, state.written], [2, 3]);
  signal.close();
  assert.equal(sockets[1].destroyed, true);
  // Without a path nothing is opened.
  createActivitySignal({ connect }).acted();
  assert.equal(state.opened, 2);
});
