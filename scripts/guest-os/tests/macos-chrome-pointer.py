"""Run inside a disposable, signed-in VirtualMac with the window agent installed.

Uses two loopback origins, native window frames and the persistent input socket.
No browser automation or external websites participate in this test.
"""
import base64
import contextlib
import http.server
import json
import queue
import socket
import struct
import subprocess
import tempfile
import threading
import time
from pathlib import Path


@contextlib.contextmanager
def origin(render):
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            data = render(self.path).encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield "http://127.0.0.1:" + str(server.server_port)
    finally:
        server.shutdown()
        server.server_close()


def target(image, directory):
    """Locate the magenta target in actual captured pixels, including Chrome UI."""
    png = directory / "frame.png"
    bmp = directory / "frame.bmp"
    png.write_bytes(base64.b64decode(image))
    subprocess.run(["sips", "-s", "format", "bmp", str(png), "--out", str(bmp)],
                   check=True, stdout=subprocess.DEVNULL)
    data = bmp.read_bytes()
    offset, = struct.unpack_from("<I", data, 10)
    width, height, _, bits = struct.unpack_from("<iiHH", data, 18)
    assert bits in (24, 32)
    stride = (width * bits + 31) // 32 * 4
    pixel = bytes((193, 1, 253))
    points = []
    for row in range(abs(height)):
        start = offset + row * stride
        pos = data.find(pixel, start, start + width * bits // 8)
        while pos >= 0:
            col, rem = divmod(pos - start, bits // 8)
            if not rem:
                points.append((col, row if height < 0 else height - row - 1))
            pos = data.find(pixel, pos + 1, start + width * bits // 8)
    if not points:
        return None
    xs, ys = zip(*points)
    return width, abs(height), (min(xs) + max(xs) + 1)/2, (min(ys) + max(ys) + 1)/2


def main():
    if not Path("/var/db/cage-guest-os").is_file():
        raise RuntimeError("Use a disposable macOS guest")
    events = queue.Queue()

    def child(path):
        if path.startswith("/event/"):
            events.put(path.split("/")[2:])
        return """<!doctype html><style>body { margin:0; } #target {position:absolute;left:37px;top:24px;
        width:17px;height:17px;background:rgb(253,1,193);touch-action:none;}</style><div id="target"></div>
        <script>
        const t=document.getElementById("target");
        let delivery=Promise.resolve(), held=false;
        for(const name of ["pointerdown","pointermove","pointerup","click"]){
          t.addEventListener(name,e=>{
            if(name==="pointerdown"){held=true;t.setPointerCapture(e.pointerId);}
            if(name==="pointermove" && !held)return;
            if(name==="pointerup")held=false;
            const path="/event/"+name+"/"+e.buttons+"/"+e.target.id;
            delivery=delivery.then(()=>fetch(path));
          });
        }</script>"""

    with origin(child) as frame_origin, origin(lambda _: f"""
        <!doctype html><title>Window input test</title><iframe style="margin:32px;border:7px solid grey;
        transform:scale(1.2);transform-origin:top left" src="{frame_origin}"></iframe>""") as page, \
            socket.socket(socket.AF_UNIX) as sock, tempfile.TemporaryDirectory() as directory:
        sock.settimeout(15)
        sock.connect(str(Path.home() / "Library/Caches/dev.cage.GuestClaudeWindow/window.sock"))
        wire = sock.makefile("rwb")

        def request(action, **fields):
            wire.write(json.dumps(dict(action="browser-" + action, surface="browser", keepAlive=True, **fields)).encode() + b"\n")
            wire.flush()
            reply = json.loads(wire.readline())
            if "error" in reply:
                raise RuntimeError(f"{action}: {reply['error']}")
            return reply

        request("launch", url=page)
        for scale in (1, 2):
            deadline = time.monotonic() + 15
            while True:
                frame = request("frame", width=960, height=540, scale=scale)
                match = target(frame["image"], Path(directory))
                if match:
                    break
                if time.monotonic() > deadline:
                    raise RuntimeError("Missing iframe target in the captured window")
                time.sleep(0.2)
            width, height, cx, cy = match
            geometry = {key: frame[key] for key in ("pid", "window", "geometry")}
            def mouse(phase, buttons, dx=0):
                request("pointer", phase=phase, x=(cx+dx)/width, y=cy/height, button=0,
                        buttons=buttons, clicks=1, **geometry)

            def expect(name, buttons):
                received = events.get(timeout=3)
                assert received == [name, str(buttons), "target"], received

            mouse("down", 1)
            expect("pointerdown", 1)
            mouse("up", 0)
            expect("pointerup", 0)
            expect("click", 0)
            mouse("down", 1)
            expect("pointerdown", 1)
            for dx in (12, 24):
                mouse("move", 1, dx)
                expect("pointermove", 1)
            mouse("up", 0, 24)
            expect("pointerup", 0)
            expect("click", 0)
            print(f"Requested {scale}x, captured {width}x{height} for "
                  f"{frame['contentWidth']}x{frame['contentHeight']} points: iframe click, held drag and release passed", flush=True)


if __name__ == "__main__":
    main()
