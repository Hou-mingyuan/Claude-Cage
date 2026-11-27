import test from "node:test";
import assert from "node:assert/strict";
import { openTerminalStream } from "./terminal-stream.mjs";

function fixture(t, pendingControl = () => false) {
  const sockets = [],
    written = [],
    pending = [],
    restored = [];
  let readyCount = 0;
  class Socket {
    static OPEN = 1;
    static CLOSED = 3;
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      this.sent = [];
      sockets.push(this);
    }
    open() {
      this.readyState = 1;
      this.onopen?.();
    }
    close() {
      this.readyState = 3;
      this.onclose?.();
    }
    send(value) {
      this.sent.push(JSON.parse(value));
    }
    message(value) {
      this.onmessage?.({
        data: value instanceof Uint8Array ? value : JSON.stringify(value),
      });
    }
  }
  const original = globalThis.WebSocket;
  globalThis.WebSocket = Socket;
  t.after(() => {
    globalThis.WebSocket = original;
  });
  const view = openTerminalStream({
    url: () => "ws://localhost/api/terminals/id/stream?connection=local",
    pendingControl,
    write(data, history, done) {
      written.push({ bytes: Buffer.from(data), history });
      pending.push(done);
    },
    restore(mode, done) {
      restored.push(mode);
      done();
    },
    control() {},
    connection() {},
    ended: () => false,
    ready() {
      readyCount++;
    },
  });
  return {
    view,
    sockets,
    written,
    restored,
    pending,
    readyCount: () => readyCount,
    flush() {
      while (pending.length) pending.shift()();
    },
  };
}

test("a hidden pane opens no socket and resumes only bytes its xterm hasn't consumed", (t) => {
  const f = fixture(t);
  f.view.setVisible(false);
  assert.equal(f.sockets.length, 0);
  f.view.setVisible(true);
  const one = f.sockets[0];
  one.open();
  const data = Buffer.alloc(49000, 65);
  one.message({
    type: "replay",
    start: 0,
    end: data.length,
    generation: "current",
    lease: "one",
  });
  one.message(data);
  const accepted = f.written[0].bytes.length;
  assert.equal(
    f.view.readyState,
    WebSocket.CLOSED,
    "input must wait for history parsing",
  );
  assert.ok(accepted > 0 && accepted < data.length);
  f.view.setVisible(false);
  assert.deepEqual(one.sent.at(-1), { type: "pause", position: accepted });
  one.message(Buffer.from("stale"));
  assert.equal(f.written.length, 1);
  f.view.setVisible(true);
  const two = f.sockets[1];
  assert.equal(two.url.searchParams.get("connection"), "local");
  assert.equal(two.url.searchParams.get("after"), String(accepted));
  assert.equal(two.url.searchParams.get("lease"), "one");
  two.open();
  two.message({
    type: "replay",
    start: accepted,
    end: data.length,
    generation: "current",
    lease: "two",
  });
  two.message(data.subarray(accepted));
  two.message(Buffer.from("live"));
  f.flush();
  assert.deepEqual(
    Buffer.concat(f.written.filter((x) => x.history).map((x) => x.bytes)),
    data,
  );
  assert.equal(
    Buffer.concat(
      f.written.filter((x) => !x.history).map((x) => x.bytes),
    ).toString(),
    "live",
  );
  assert.deepEqual(two.sent, [{ type: "caught-up" }]);
  assert.equal(f.view.readyState, WebSocket.OPEN);
  assert.deepEqual(f.restored, []);
  f.view.close();
});

test("failed authorization cannot reveal a cached screen, and a new unlock resets it", (t) => {
  const f = fixture(t);
  f.view.setVisible(true);
  const one = f.sockets[0];
  one.open();
  one.message({
    type: "replay",
    start: 0,
    end: 0,
    generation: "before-lock",
    lease: "first",
  });
  one.message(Buffer.from("private"));
  f.flush();
  f.view.setVisible(false);
  const shown = f.readyCount();
  f.view.setVisible(true);
  f.sockets[1].close(); // server rejected the HTTP upgrade
  assert.equal(f.readyCount(), shown);
  f.view.reconnect();
  const latest = f.sockets[2];
  latest.open();
  assert.equal(f.readyCount(), shown);
  latest.message({
    type: "replay",
    start: 99,
    end: 103,
    generation: "after-unlock",
    lease: "next",
    reset: true,
  });
  assert.deepEqual(f.restored, ["reset"]);
  assert.equal(f.view.readyState, WebSocket.CLOSED);
  latest.message(Buffer.from("tail"));
  f.flush();
  assert.ok(f.readyCount() > shown);
  assert.equal(f.view.readyState, WebSocket.OPEN);
  f.view.close();
});

test("a partial historical control keeps its guard until new live controls, including a split UTF-8 C1", (t) => {
  const f = fixture(t, () => true);
  f.view.setVisible(true);
  const socket = f.sockets[0];
  socket.open();
  socket.message({ type: "history" });
  socket.message(Buffer.from("\x1b]52;c;c3RhbGU="));
  socket.message(Buffer.from([7, 0xc2]));
  socket.message(Buffer.from([0x9b, 0x63])); // fresh C1 CSI followed by DA
  f.flush();
  assert.deepEqual(
    f.written
      .map(({ bytes, history }) => [Array.from(bytes), history])
      .slice(1),
    [
      [[7, 0xc2], true],
      [[0x9b], true],
      [[0x63], false],
    ],
  );
  f.view.close();
});

test("an overflow marks the gap, while an older server's replay is treated as fresh history", (t) => {
  const f = fixture(t);
  f.view.setVisible(true);
  const one = f.sockets[0];
  one.open();
  one.message({
    type: "replay",
    start: 0,
    end: 0,
    generation: "current",
    lease: "one",
  });
  f.view.setVisible(false);
  f.view.setVisible(true);
  const two = f.sockets[1];
  two.open();
  two.message({
    type: "replay",
    start: 45,
    end: 49,
    generation: "current",
    lease: "two",
    gap: true,
  });
  two.message(Buffer.from("tail"));
  two.message(Buffer.from("now"));
  f.flush();
  assert.deepEqual(f.restored, ["gap"]);
  f.view.setVisible(false);
  f.view.setVisible(true);
  const legacy = f.sockets[2];
  legacy.open();
  legacy.message({ type: "history" });
  legacy.message(Buffer.from("old"));
  legacy.message(Buffer.from("new"));
  f.flush();
  assert.deepEqual(f.restored, ["gap", "reset"]);
  assert.deepEqual(
    f.written.map((x) => [x.bytes.toString(), x.history]),
    [
      ["tail", true],
      ["now", false],
      ["old", true],
      ["new", false],
    ],
  );
  f.view.close();
});
