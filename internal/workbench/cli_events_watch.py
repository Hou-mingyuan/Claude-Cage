# Guest-side watcher for terminal turn completions and approval requests.
# The host prepends cli_channel.py's ROOT definition, so the state directory
# has a single source. Only the connections named by the host are read.
import ctypes
import json
import os
import select
import struct
import sys
import time

IN_MOVED_TO, IN_CREATE, IN_Q_OVERFLOW, IN_IGNORED, IN_ONLYDIR = 0x80, 0x100, 0x4000, 0x8000, 0x1000000


def read_json(path):
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        return {}


def emit(payload):
    sys.stdout.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")) + "\n")
    sys.stdout.flush()


class Inotify:
    """Directory change wakeups through libc; callers fall back to polling."""

    def __init__(self):
        self.libc = ctypes.CDLL(None, use_errno=True)
        self.fd = self.libc.inotify_init1(os.O_NONBLOCK | os.O_CLOEXEC)
        if self.fd < 0:
            raise OSError(ctypes.get_errno(), "inotify unavailable")
        self.watches = {}

    def add(self, path, mask):
        if path in self.watches.values():
            return True
        wd = self.libc.inotify_add_watch(self.fd, os.fsencode(str(path)), mask | IN_ONLYDIR)
        if wd >= 0:
            self.watches[wd] = path
        return wd >= 0

    def wait(self, timeout, relevant):
        if not select.select([self.fd], [], [], timeout)[0]:
            return False
        try:
            data = os.read(self.fd, 65536)
        except BlockingIOError:
            return False
        found, offset = False, 0
        while offset + 16 <= len(data):
            wd, mask, _, size = struct.unpack_from("iIII", data, offset)
            name = data[offset + 16:offset + 16 + size].rstrip(b"\0").decode("utf-8", "ignore")
            offset += 16 + size
            if mask & IN_IGNORED:
                self.watches.pop(wd, None)
            found = found or bool(mask & IN_Q_OVERFLOW) or relevant(self.watches.get(wd), name)
        return found


def watch(request):
    active = [conn for conn in dict.fromkeys(request.get("connections") or ()) if isinstance(conn, str) and conn and "/" not in conn and not conn.startswith(".")]
    heartbeat = max(1.0, float(request.get("heartbeat", 15.0)))
    # Events older than this are a backlog from before the app or stream
    # started; forwarding them would replay stale banners at once.
    max_age = max(1.0, float(request.get("max_age", 60.0)))
    fast, slow = 0.2, 1.5
    file_stats, last_done, seen_approvals = {}, {}, set()
    try:
        notifier = Inotify() if request.get("inotify", True) else None
    except (OSError, AttributeError):
        notifier = None

    def ensure_watches():
        if notifier is None or not ROOT.is_dir():
            return False
        notifier.add(ROOT, IN_CREATE | IN_MOVED_TO)
        for conn in active:
            if (ROOT / conn).is_dir():
                notifier.add(ROOT / conn, IN_MOVED_TO)
        return True

    def relevant(directory, name):
        if directory is None:
            return False
        if directory == ROOT:
            return name in active
        return name == "state.json"

    def scan():
        changed = False
        for conn in active:
            path = ROOT / conn / "state.json"
            try:
                st = path.stat()
            except OSError:
                continue
            sig = (st.st_ino, st.st_size, st.st_mtime_ns)
            if file_stats.get(conn) == sig:
                continue
            file_stats[conn] = sig
            changed = True
            state = read_json(path)
            updated = state.get("updated_at")
            fresh = isinstance(updated, (int, float)) and time.time() - updated <= max_age and not state.get("closed")
            done = state.get("turn_done")
            done = done if isinstance(done, int) and done > 0 else 0
            if done > last_done.get(conn, 0):
                last_done[conn] = done
                if fresh:
                    emit({"id": f"{conn}:turn:{done}", "event": "turn_completed", "connection": conn,
                          "cli_id": state.get("cli_id", ""), "seq": done})
            approvals = state.get("approvals") if isinstance(state.get("approvals"), list) else []
            for item in approvals:
                aid = item.get("id") if isinstance(item, dict) else None
                if isinstance(aid, str) and aid and aid not in seen_approvals:
                    seen_approvals.add(aid)
                    if fresh:
                        emit({"id": f"{conn}:approval:{aid}", "event": "approval_requested", "connection": conn,
                              "cli_id": state.get("cli_id", "")})
        return changed

    watching = ensure_watches()
    scan()
    last_ping = time.monotonic()
    interval = fast
    while True:
        remaining = max(0.0, heartbeat - (time.monotonic() - last_ping))
        if watching:
            # Sleep until a watched state file is replaced or the heartbeat is due.
            if notifier.wait(remaining, relevant):
                ensure_watches()
            watching = ROOT in notifier.watches.values()
        else:
            # No inotify (or no state directory yet): poll only the active
            # connections, quickly after activity and backing off when idle.
            time.sleep(min(interval, remaining) if remaining else 0)
            watching = ensure_watches()
        changed = scan()
        interval = fast if changed else min(slow, interval * 1.5)
        if time.monotonic() - last_ping >= heartbeat:
            emit({"event": "ping"})
            last_ping = time.monotonic()


if __name__ == "__main__":
    try:
        watch(json.loads(sys.argv[1]) if len(sys.argv) > 1 else {})
    except BrokenPipeError:
        pass
