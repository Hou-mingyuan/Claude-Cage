import { spawn } from "node:child_process";
import { createConnection } from "node:net";

// A wheel step sent through the display scrolls a fixed distance and starts
// an animation. A trackpad reports many small distances: the page then trails
// the fingers, overshoots, and keeps moving after they stop. The viewer sends
// the scrolled distance instead, in framebuffer pixels, and the page scrolls
// by exactly that much within the same frame.
//
// Only a single page under the pointer can take a distance. Everything else
// declines it, and the viewer sends its usual wheel steps to the display.

const distanceLimit = 8192;
const metricsLifetime = 500;
const pointerLifetime = 300;
const stalledAfter = 400;
const declineAfterFailure = 1000;

const whole = (value, low, high) => Number.isInteger(value) && value >= low && value <= high;

export function wheelInput(input) {
  const { x, y, deltaX, deltaY } = input || {};
  if (!whole(x, 0, distanceLimit) || !whole(y, 0, distanceLimit) ||
      !whole(deltaX, -distanceLimit, distanceLimit) || !whole(deltaY, -distanceLimit, distanceLimit) ||
      (!deltaX && !deltaY)) throw new Error("Invalid wheel input");
  return { x, y, deltaX, deltaY };
}

// Menus and select lists are windows of their own, smaller than the screen.
// The page window always covers it. One long-lived process answers where the
// pointer rests; it is started by the first wheel and ends with the agent.
export function createPointerProbe({
  start = () => spawn("/usr/bin/xdotool", ["-"], { stdio: ["pipe", "pipe", "ignore"] }), timeout = 400,
} = {}) {
  let current;
  const open = () => {
    const probe = { child: start(), jobs: [], data: "", closed: false };
    const close = () => {
      if (probe.closed) return;
      probe.closed = true;
      if (current === probe) current = undefined;
      for (const job of probe.jobs.splice(0)) { clearTimeout(job.timer); job.resolve(null); }
      probe.child.kill();
    };
    probe.close = close;
    probe.child.stdout.setEncoding("utf8");
    probe.child.stdout.on("data", (chunk) => {
      probe.data += chunk;
      if (probe.data.length > 4096) return close();
      const job = probe.jobs[0];
      if (!job) { probe.data = ""; return; }
      const fields = Object.fromEntries([...probe.data.matchAll(/^([A-Z]+)=(-?\d+)$/gm)].map(match => [match[1], +match[2]]));
      if (!job.done(fields)) return;
      probe.data = "";
      probe.jobs.shift();
      clearTimeout(job.timer);
      job.resolve(fields);
    });
    probe.child.on("error", close);
    probe.child.on("close", close);
    probe.child.stdin.on("error", close);
    return probe;
  };
  // Answers arrive in request order; one that never comes ends the process.
  const ask = (line, done) => new Promise((resolve) => {
    const probe = current && !current.closed ? current : current = open();
    probe.jobs.push({ done, resolve, timer: setTimeout(probe.close, timeout) });
    probe.child.stdin.write(line + "\n");
  });
  return {
    // True when the window under the pointer covers the whole screen.
    async overPage() {
      // One question at a time: each answer is read up to its last line, and
      // a window that closed meanwhile ends the process without an answer.
      const pointer = await ask("getmouselocation --shell", fields => "WINDOW" in fields);
      if (!pointer?.WINDOW) return false;
      const window = await ask(`getwindowgeometry --shell ${pointer.WINDOW}`, fields => "SCREEN" in fields);
      if (!window) return false;
      const screen = await ask("getdisplaygeometry --shell", fields => "HEIGHT" in fields);
      return Boolean(screen && window.X === 0 && window.Y === 0 &&
        window.WIDTH === screen.WIDTH && window.HEIGHT === screen.HEIGHT);
    },
    close: () => current?.close(),
  };
}

// The display lowers its refresh rate when nobody operates it, and learns of
// that from the display's own input. A distance never passes through there.
export function createActivitySignal({ path, connect = () => createConnection(path), interval = 250, now = Date.now } = {}) {
  let socket, sent = -Infinity;
  return {
    acted() {
      if (!path || now() - sent < interval) return;
      sent = now();
      if (!socket) {
        const opened = socket = connect();
        const drop = () => { if (socket === opened) socket = undefined; opened.destroy(); };
        opened.on("error", drop);
        opened.on("close", drop);
      }
      socket.write("\n");
    },
    close: () => socket?.destroy(),
  };
}

export class BrowserWheel {
  // target() resolves to the session of the one page that fills the window,
  // or to nothing while there is no such page.
  constructor({ cdp, target, pointer, activity, now = Date.now }) {
    this.cdp = cdp;
    this.target = target;
    this.pointer = pointer;
    this.activity = activity;
    this.now = now;
    this.metrics = new Map();
    this.overPage = { at: -Infinity, value: false };
    this.session = undefined;
    this.pending = null;
    this.sending = null;
    this.declineUntil = -Infinity;
  }

  forget(session) { this.metrics.delete(session); }

  async scale(session) {
    const known = this.metrics.get(session), now = this.now();
    if (known && (now - known.at < metricsLifetime || known.loading)) return known.value;
    const load = this.cdp("Page.getLayoutMetrics", {}, session).then((result) => {
      const density = result.layoutViewport.clientWidth / result.cssLayoutViewport.clientWidth;
      const zoom = result.cssVisualViewport.zoom || 1;
      if (!(density > 0.1 && density < 32 && zoom > 0.1 && zoom < 16)) throw new Error("Unusable page metrics");
      // Positions are CSS pixels. Distances are independent of the page zoom.
      const value = { position: density, distance: density / zoom };
      this.metrics.set(session, { at: this.now(), value });
      return value;
    }).catch((error) => { this.metrics.delete(session); throw error; });
    if (!known) return load;
    // A zoom change shows up one wheel later; the scroll does not wait for it.
    known.loading = true;
    load.catch(() => {});
    return known.value;
  }

  async pointerOverPage() {
    const now = this.now();
    if (now - this.overPage.at < pointerLifetime) return this.overPage.value;
    // Wheels that arrive together share one answer.
    this.probing ||= this.pointer.overPage().catch(() => false).then((value) => {
      this.overPage = { at: this.now(), value };
      this.probing = null;
      return value;
    });
    return this.probing;
  }

  async deliver(input) {
    const wheel = wheelInput(input);
    if (this.now() < this.declineUntil) return { handled: false };
    // A page that stopped answering, behind a dialog for instance, must not
    // collect a distance to jump by later.
    if (this.sending && this.now() - this.sending.since > stalledAfter) {
      this.pending = null;
      return { handled: false };
    }
    const session = await this.target();
    if (!session || !await this.pointerOverPage()) return { handled: false };
    let scale;
    try { scale = await this.scale(session); }
    catch { return this.failed(); }
    if (session !== this.session) { this.session = session; this.pending = null; }
    const pending = this.pending ||= { x: 0, y: 0, deltaX: 0, deltaY: 0 };
    pending.x = wheel.x / scale.position;
    pending.y = wheel.y / scale.position;
    pending.deltaX += wheel.deltaX / scale.distance;
    pending.deltaY += wheel.deltaY / scale.distance;
    this.activity?.acted();
    this.flush();
    return { handled: true };
  }

  failed() {
    this.pending = null;
    this.declineUntil = this.now() + declineAfterFailure;
    return { handled: false };
  }

  // The page takes one distance per frame. What arrives meanwhile is added
  // up and follows as soon as the page has answered.
  flush() {
    if (this.sending || !this.pending) return;
    const { x, y, deltaX, deltaY } = this.pending, session = this.session;
    this.pending = null;
    const sending = this.sending = { since: this.now() };
    this.cdp("Input.dispatchMouseEvent", { type: "mouseWheel", x, y, deltaX, deltaY }, session).then(
      () => { if (this.sending === sending) { this.sending = null; this.flush(); } },
      () => { if (this.sending === sending) { this.sending = null; this.failed(); } },
    );
  }
}
