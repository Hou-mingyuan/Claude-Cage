"""Run as root in a disposable Linux test environment with GTK, Xvfb and systemd.

Creates one temporary PrivateTmp desktop service; never uses an existing desktop.
"""
import base64
import json
from pathlib import Path
import queue
import subprocess
import sys
import threading
import time
import uuid

instance = uuid.uuid4().hex[:24]
unit = 'desktop-' + instance
root = Path('/run') / unit
processes = []
events = queue.Queue()
imports = """import gi,json,base64
gi.require_version('Gtk','3.0')
from gi.repository import Gtk,Gdk,GdkPixbuf,GLib
Gtk.init([])
cb=Gtk.Clipboard.get(Gdk.SELECTION_CLIPBOARD)
"""


def expect(predicate):
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        try:
            event = events.get(timeout=.2)
        except queue.Empty:
            continue
        if predicate(event):
            return event
    raise AssertionError('clipboard event timed out')


try:
    subprocess.run(['systemd-run', '--quiet', '--collect', '--unit=' + unit,
                    '--property=User=cage', '--property=PrivateTmp=yes',
                    '--property=RuntimeDirectory=' + unit,
                    'Xvfb', ':197', '-screen', '0', '640x480x24', '-nolisten', 'tcp', '-ac'], check=True)
    for _ in range(30):
        if root.exists():
            break
        time.sleep(.1)
    (root / 'display').write_text('197\n')
    (root / 'Xauthority').touch()
    pid = subprocess.check_output(['systemctl', 'show', unit, '--property=MainPID', '--value'], text=True).strip()
    prefix = ['nsenter', '--target', pid, '--mount', '--net', 'runuser', '-u', 'cage', '--',
              'env', 'DISPLAY=:197', 'XAUTHORITY=' + str(root / 'Xauthority'), 'python3', '-c']
    time.sleep(.3)
    helper = sys.argv[1]
    bridge = subprocess.Popen(['python3', helper, '--desktop', instance], stdin=subprocess.PIPE,
                              stdout=subprocess.PIPE, text=True)
    processes.append(bridge)
    threading.Thread(target=lambda: [events.put(json.loads(line)) for line in bridge.stdout], daemon=True).start()
    expect(lambda e: e.get('hello', {}).get('available'))
    bridge.stdin.write(json.dumps({'text': 'host 中文🙂', 'serial': 1}) + '\n')
    bridge.stdin.flush()
    expect(lambda e: e.get('ack') == 1)
    actual = subprocess.check_output(prefix + [imports + 'print(json.dumps(cb.wait_for_text()))'], text=True, timeout=5)
    assert json.loads(actual) == 'host 中文🙂'
    print('PASS host text -> private desktop', flush=True)
    writer = subprocess.Popen(prefix + [imports + "p=GdkPixbuf.Pixbuf.new(GdkPixbuf.Colorspace.RGB,True,8,3,2)\np.fill(0x12ab34ff)\ncb.set_image(p)\nprint('ready',flush=True)\nGtk.main()"], stdout=subprocess.PIPE, text=True)
    processes.append(writer)
    assert writer.stdout.readline().strip() == 'ready'
    png = expect(lambda e: 'png' in e)['png']
    assert base64.b64decode(png).startswith(b'\x89PNG')
    print('PASS private desktop image -> host', flush=True)
    bridge.stdin.write(json.dumps({'png': png, 'serial': 2}) + '\n')
    bridge.stdin.flush()
    expect(lambda e: e.get('ack') == 2)
    actual = subprocess.check_output(prefix + [imports + 'p=cb.wait_for_image(); print(json.dumps([p.get_width(),p.get_height(),list(p.get_pixels()[:4])]))'], text=True, timeout=5)
    assert json.loads(actual) == [3, 2, [18, 171, 52, 255]]
    print('PASS host image -> private desktop', flush=True)
    writer2 = subprocess.Popen(prefix + [imports + "cb.set_text('guest 中文🙂',-1)\nprint('ready',flush=True)\nGtk.main()"], stdout=subprocess.PIPE, text=True)
    processes.append(writer2)
    assert writer2.stdout.readline().strip() == 'ready'
    expect(lambda e: e.get('text') == 'guest 中文🙂')
    print('PASS private desktop text -> host', flush=True)
finally:
    for process in reversed(processes):
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
    subprocess.run(['systemctl', 'stop', unit], check=False)
