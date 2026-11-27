// ESC or a UTF-8 C1 starts a new control or terminates the old one. Parse
// that byte under the old guard: it can dispatch a pending OSC/DCS. The
// following bytes belong to live output and may contain new device queries.
function nextLiveControl(bytes, liveC1Lead) {
  let previous = liveC1Lead ? 0xc2 : 0;
  for (let i = 0; i < bytes.length; i++) {
    const value = bytes[i];
    if (value === 0x1b || (previous === 0xc2 && value >= 0x80 && value < 0xa0))
      return i + 1;
    previous = value;
  }
  return 0;
}

// Each visible pane owns one output stream. Keep its xterm on hide, but only
// park the position already submitted to the parser; queued network frames
// can safely be fetched again. At most one small write finishes after hiding.
export function openTerminalStream({
  url,
  write,
  pendingControl,
  restore,
  control,
  connection,
  ready,
  ended,
}) {
  let link = null,
    visible = false,
    cursor = null,
    writing = false,
    seen = false;
  let backlog = [],
    outstanding = 0,
    drain = 0;
  let historyMayContinue = false,
    liveC1Lead = false;
  const open = () => link?.ws.readyState === WebSocket.OPEN;
  const notifyReady = () => {
    if (
      !link ||
      !visible ||
      !link.initialized ||
      writing ||
      link.remaining > 0 ||
      link.ready
    )
      return;
    link.ready = true;
    if (cursor?.lease && open())
      link.ws.send(JSON.stringify({ type: "caught-up" }));
    ready();
  };
  const pump = () => {
    if (!visible || writing || !link) return;
    while (drain < backlog.length && backlog[drain].done) drain++;
    if (drain > 64 && drain < backlog.length && drain * 2 >= backlog.length) {
      backlog = backlog.slice(drain);
      drain = 0;
    }
    if (drain === backlog.length) {
      backlog = [];
      drain = 0;
      notifyReady();
      return;
    }
    const item = backlog[drain],
      owner = link;
    if (item.restore) {
      historyMayContinue = liveC1Lead = false;
      item.done = true;
      writing = true;
      restore(item.restore, () => {
        writing = false;
        pump();
      });
      return;
    }
    // Publish readiness before the first live response, even when that frame
    // arrived on the wire before xterm finished parsing the history.
    if (!item.history) notifyReady();
    let chunk = item.data.subarray(item.at, item.at + (16 << 10));
    const continuation =
      !item.history && historyMayContinue && pendingControl();
    const boundary = continuation ? nextLiveControl(chunk, liveC1Lead) : 0;
    if (boundary) chunk = chunk.subarray(0, boundary);
    historyMayContinue = item.history || (continuation && !boundary);
    liveC1Lead = continuation && !boundary && chunk.at(-1) === 0xc2;
    item.at += chunk.length;
    outstanding -= chunk.length;
    if (item.at === item.data.length) {
      item.done = true;
      item.data = null;
    }
    if (cursor) cursor.position += chunk.length;
    writing = true;
    write(chunk, item.history || continuation, () => {
      writing = false;
      if (link === owner && item.history) owner.remaining -= chunk.length;
      pump();
    });
  };
  function drop(park) {
    const old = link;
    link = null;
    backlog = [];
    outstanding = 0;
    drain = 0;
    if (old) {
      if (park && cursor?.lease && old.ws.readyState === WebSocket.OPEN) {
        old.ws.send(
          JSON.stringify({ type: "pause", position: cursor.position }),
        );
      }
      old.ws.close();
    }
    connection(false, park);
  }
  function connect() {
    if (!visible || link) return;
    const address = new URL(url());
    address.searchParams.set("replay", "1");
    if (cursor) {
      address.searchParams.set("after", String(cursor.position));
      address.searchParams.set("generation", cursor.generation);
      address.searchParams.set("lease", cursor.lease);
    }
    const ws = new WebSocket(address);
    const current = {
      ws,
      initialized: false,
      ready: false,
      remaining: 0,
      historyNext: false,
      modern: false,
    };
    link = current;
    ws.binaryType = "arraybuffer";
    const initializeLegacy = () => {
      if (current.initialized) return;
      current.initialized = true;
      cursor = null;
      if (seen) backlog.push({ restore: "reset" });
      seen = true;
    };
    ws.onopen = () => {
      if (link !== current) return;
      connection(true, false);
      // Legacy services can have no history and wait for the first keystroke.
      if (!seen) ready();
      else if (!cursor) {
        initializeLegacy();
        pump();
      }
    };
    ws.onmessage = (event) => {
      if (!visible || link !== current) return;
      if (typeof event.data === "string") {
        let state;
        try {
          state = JSON.parse(event.data);
        } catch {
          return;
        }
        if (state.type === "replay") {
          if (
            current.modern ||
            !Number.isSafeInteger(state.start) ||
            !Number.isSafeInteger(state.end) ||
            state.start < 0 ||
            state.end < state.start ||
            typeof state.generation !== "string" ||
            !state.generation
          ) {
            drop(false);
            return;
          }
          current.initialized = current.modern = true;
          current.remaining = state.end - state.start;
          current.received = state.start;
          current.end = state.end;
          if (state.reset || (seen && !cursor))
            backlog.push({ restore: "reset" });
          else if (state.gap) backlog.push({ restore: "gap" });
          seen = true;
          cursor = {
            position: state.start,
            generation: state.generation,
            lease: state.lease || "",
          };
          pump();
        } else if (state.type === "history") {
          initializeLegacy();
          current.historyNext = true;
        } else {
          if (state.type === "terminal-end") current.completed = true;
          control(state);
        }
        return;
      }
      initializeLegacy();
      const bytes = new Uint8Array(event.data);
      const historical = current.modern
        ? current.received < current.end
        : current.historyNext;
      if (current.modern) current.received += bytes.length;
      else if (historical) current.remaining += bytes.length;
      current.historyNext = false;
      if (bytes.length) {
        outstanding += bytes.length;
        // A stalled renderer must not retain an unbounded JavaScript queue.
        if (outstanding > 10 << 20) {
          drop(true);
          return;
        }
        backlog.push({ data: bytes, at: 0, history: historical });
      }
      pump();
    };
    ws.onclose = () => {
      if (link !== current) return;
      if (!current.completed && !ended()) {
        link = null;
        backlog = [];
        outstanding = 0;
        drain = 0;
      }
      connection(false, false);
      pump(); // A completed PTY may close before its final frame is painted.
    };
  }
  return {
    get readyState() {
      return open() && (link.ready || !seen)
        ? WebSocket.OPEN
        : WebSocket.CLOSED;
    },
    send(data) {
      if (open() && (link.ready || !seen)) link.ws.send(data);
    },
    setVisible(value) {
      if (value === visible) return;
      visible = value;
      if (value) connect();
      else if (link) drop(true);
    },
    reconnect() {
      drop(false);
      connect();
    },
    close() {
      visible = false;
      drop(false);
    },
  };
}
