import assert from "node:assert/strict";
import test from "node:test";
import CageWheel, { cageWheelLimit, cageWheelMessage, cageWheelNotched, cageWheelPacket, cageWheelPending } from "../vendor/novnc/core/cage-wheel.js";

function fixture() {
  const sent = [], frames = [];
  const wheel = new CageWheel((...message) => sent.push(message), callback => frames.push(callback), handle => { frames[handle - 1] = null; });
  // One display frame: run what was scheduled for it.
  const frame = () => { for (const callback of frames.splice(0)) callback?.(); };
  return { wheel, sent, frames, frame };
}

test("the first distance is sent at once and later ones once per frame", () => {
  const { wheel, sent, frame } = fixture();
  wheel.add(10, 20, 0, 120);
  assert.deepEqual(sent, [[10, 20, 0, 120]]);
  // A trackpad reports many small distances between two frames.
  for (const distance of [3, 4.5, 2.25, 7]) wheel.add(11, 21, 0, distance);
  assert.equal(sent.length, 1);
  frame();
  assert.deepEqual(sent[1], [11, 21, 0, 17]);
  // The fraction of a pixel is kept, and an idle frame ends the series.
  frame();
  assert.equal(sent.length, 2);
  wheel.add(12, 22, 0, 0.75);
  assert.deepEqual(sent[2], [12, 22, 0, 1]);
});

test("distance beyond one message is carried into the next frames", () => {
  const { wheel, sent, frame } = fixture();
  wheel.add(1, 2, -5000, 3000);
  frame();
  frame();
  frame();
  assert.deepEqual(sent, [[1, 2, -cageWheelLimit, cageWheelLimit], [1, 2, -cageWheelLimit, 952], [1, 2, -904, 0]]);
});

test("less than a pixel waits, and stopping discards what was not sent", () => {
  const { wheel, sent, frame, frames } = fixture();
  wheel.add(1, 2, 0.2, 0.2);
  assert.equal(sent.length, 0);
  assert.equal(frames.length, 0);
  wheel.add(1, 2, 0.4, 0);
  assert.deepEqual(sent, [[1, 2, 1, 0]]);
  wheel.add(1, 2, 0, 300);
  wheel.stop();
  frame();
  assert.equal(sent.length, 1);
  wheel.add(3, 4, 0, -40);
  assert.deepEqual(sent[1], [3, 4, 0, -40]);
});

test("a message carries its position and signed distances", () => {
  const packet = cageWheelPacket(513, 1026, -720, 2048), view = new DataView(packet.buffer);
  assert.equal(packet.length, 14);
  assert.deepEqual([packet[0], packet[1]], [cageWheelMessage, 0]);
  assert.deepEqual([view.getUint16(2), view.getUint16(4), view.getInt32(6), view.getInt32(10)], [513, 1026, -720, 2048]);
});

test("a few sends may be under way, and more distance waits for an answer", async () => {
  const sent = [], frames = [], answers = [];
  const wheel = new CageWheel((...message) => { sent.push(message); return new Promise(resolve => answers.push(resolve)); },
    callback => frames.push(callback), handle => { frames[handle - 1] = null; });
  const frame = () => { for (const callback of frames.splice(0)) callback?.(); };
  // A distant guest answers slower than frames pass: it still gets one per frame.
  wheel.add(1, 2, 0, 10);
  for (const distance of [5, 6]) { wheel.add(1, 2, 0, distance); frame(); }
  assert.deepEqual(sent, [[1, 2, 0, 10], [1, 2, 0, 5], [1, 2, 0, 6]]);
  assert.equal(cageWheelPending, 3);
  // The limit is reached: frames pass and nothing more is sent.
  wheel.add(1, 2, 0, 7);
  frame();
  wheel.add(1, 2, 0, 8);
  frame();
  assert.equal(sent.length, 3);
  answers.shift()(true);
  await Promise.resolve();
  assert.deepEqual(sent[3], [1, 2, 0, 15]);
  // An answer ahead of the frame waits for the frame.
  wheel.add(1, 2, 0, 3);
  answers.shift()(true);
  await Promise.resolve();
  assert.equal(sent.length, 4);
  frame();
  assert.deepEqual(sent[4], [1, 2, 0, 3]);
  // A failed send does not hold the following ones back.
  for (const answer of answers.splice(0)) answer(Promise.reject(new Error("gone")));
  await new Promise(resolve => setTimeout(resolve));
  frame();
  wheel.add(1, 2, 0, 4);
  assert.deepEqual(sent[5], [1, 2, 0, 4]);
});

test("the distance not yet sent can be taken for another delivery", () => {
  const { wheel, sent, frame } = fixture();
  wheel.add(1, 2, 0, 10);
  wheel.add(1, 2, 2.5, 30);
  assert.deepEqual(wheel.take(), { dx: 2.5, dy: 30 });
  frame();
  assert.equal(sent.length, 1);
});

test("a notched wheel is told from a distance", () => {
  // Chromium and WebView2 on Windows, WebKitGTK: one or more whole notches.
  assert.equal(cageWheelNotched({ deltaMode: 0, deltaY: 100, wheelDeltaY: -120 }), true);
  assert.equal(cageWheelNotched({ deltaMode: 0, deltaY: -300, wheelDeltaY: 360 }), true);
  assert.equal(cageWheelNotched({ deltaMode: 0, deltaX: 100, wheelDeltaX: -120, wheelDeltaY: 0 }), true);
  // Line and page units come from a wheel as well.
  assert.equal(cageWheelNotched({ deltaMode: 1, deltaY: 3 }), true);
  // Trackpads, and macOS, which scales a wheel to a distance itself.
  assert.equal(cageWheelNotched({ deltaMode: 0, deltaY: 7, wheelDeltaY: -21 }), false);
  assert.equal(cageWheelNotched({ deltaMode: 0, deltaY: 4, wheelDeltaY: -12 }), false);
  assert.equal(cageWheelNotched({ deltaMode: 0, deltaY: 1.25 }), false);
});
