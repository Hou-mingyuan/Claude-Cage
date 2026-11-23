import test from "node:test";
import assert from "node:assert/strict";
import { desktopInputChannel } from "./desktop-input-channel.mjs";

function fixture() {
  const sockets = [];
  const channel = desktopInputChannel(() => {
    const socket = new EventTarget();
    socket.readyState = 0;
    socket.sent = [];
    socket.send = value => socket.sent.push(new TextDecoder().decode(value));
    socket.close = () => { socket.readyState = 3; socket.dispatchEvent(new Event("close")); };
    sockets.push(socket);
    return socket;
  });
  const open = socket => { socket.readyState = 1; socket.dispatchEvent(new Event("open")); };
  const data = (socket, text) => {
    const event = new Event("message");
    event.data = new TextEncoder().encode(text).buffer;
    socket.dispatchEvent(event);
  };
  return { channel, sockets, open, data };
}

test("input channel keeps requests ordered across split response frames", async () => {
  const f = fixture();
  try {
    const first = f.channel.request({ mode: "status" });
    const second = f.channel.request({ mode: "begin", session: "a".repeat(32) });
    f.open(f.sockets[0]);
    assert.equal(f.sockets[0].sent.length, 2);
    f.data(f.sockets[0], '{"available":');
    f.data(f.sockets[0], 'true}\n{"available":false}\n');
    assert.deepEqual(await first, { available: true });
    assert.deepEqual(await second, { available: false });
  } finally { f.channel.dispose(); }
});

test("closing an instance rejects pending input and never replays it", async () => {
  const f = fixture();
  const pending = f.channel.request({ mode: "commit", text: "你好" });
  f.channel.dispose();
  await assert.rejects(pending);
  f.open(f.sockets[0]);
  assert.deepEqual(f.sockets[0].sent, []);
  await assert.rejects(f.channel.request({ mode: "status" }));
});

test("broken input connection is retried only for a new request", async () => {
  const f = fixture();
  try {
    const old = f.channel.request({ mode: "commit", text: "old" });
    f.sockets[0].close();
    await assert.rejects(old);
    const next = f.channel.request({ mode: "status" });
    f.open(f.sockets[1]);
    assert.equal(f.sockets[1].sent.length, 1);
    assert.equal(JSON.parse(f.sockets[1].sent[0]).mode, "status");
    f.data(f.sockets[1], '{"available":true}\n');
    await next;
  } finally { f.channel.dispose(); }
});
