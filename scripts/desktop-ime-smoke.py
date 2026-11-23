"""Root-only isolated GTK/IBus smoke test; never touches an existing desktop."""
import json
import os
from pathlib import Path
import pwd
import subprocess
import tempfile
import time
import uuid

account = pwd.getpwnam('cage')
unit = 'cage-ime-qa-' + uuid.uuid4().hex[:12]
with tempfile.TemporaryDirectory(prefix=unit, dir='/home/cage/.cache') as directory:
    root = Path(directory)
    os.chown(root, account.pw_uid, account.pw_gid)
    app = root / 'entry.py'
    app.write_text('''import gi,json,os
from pathlib import Path
gi.require_version('Gtk','3.0')
from gi.repository import Gtk,Gdk,GLib
window=Gtk.Window(title='Cage IME QA')
entry=Gtk.Entry()
window.add(entry)
window.set_default_size(400,100)
entry.connect('changed',lambda field: (Path.home()/'result.json').write_text(json.dumps(field.get_text())))
window.show_all()
entry.grab_focus()
Gtk.main()
''')
    try:
        command = 'export HOME="$1" DISPLAY=:196; Xvfb :196 -screen 0 640x480x24 -nolisten tcp -ac & sleep .3; exec dbus-run-session -- /usr/local/bin/cage-input-session python3 "$1/entry.py"'
        subprocess.run(['systemd-run', '--quiet', '--collect', '--unit=' + unit, '--property=User=cage',
                        '--property=PrivateTmp=yes', '/bin/bash', '-c', command, '--', str(root)], check=True)
        ready = root / '.cache/cage-input/ready'
        for _ in range(100):
            if ready.exists():
                break
            time.sleep(.1)
        assert ready.exists(), 'IBus did not become ready'
        pid = subprocess.check_output(['systemctl', 'show', unit, '--property=MainPID', '--value'], text=True).strip()
        subprocess.run(['nsenter', '--target', pid, '--mount', '--net', 'runuser', '-u', 'cage', '--',
                        'env', 'DISPLAY=:196', 'xdotool', 'search', '--sync', '--name', 'Cage IME QA', 'windowfocus', '--sync'], check=True, timeout=5)

        def request(value):
            result = subprocess.check_output(['runuser', '-u', 'cage', '--', 'env', 'HOME=' + str(root),
                                              '/usr/local/bin/cage-native-input', '--request'],
                                             input=json.dumps(value) + '\n', text=True, timeout=5)
            return json.loads(result)

        for _ in range(30):
            state = request({'mode': 'status'})
            if state.get('available'):
                break
            time.sleep(.1)
        assert state.get('available'), state
        assert len(state['rect']) == 4
        session = uuid.uuid4().hex
        assert request({'mode': 'begin', 'context': state['context'], 'session': session, 'sequence': 0})['available']
        assert request({'mode': 'update', 'text': 'nihao', 'selectionStart': 0, 'selectionEnd': 5, 'session': session, 'sequence': 1})['available']
        text = '你好，第二实例🙂'
        assert request({'mode': 'commit', 'text': text, 'session': session, 'sequence': 2})['available']
        for _ in range(30):
            result = root / 'result.json'
            if result.exists() and json.loads(result.read_text()) == text:
                break
            time.sleep(.1)
        assert json.loads(result.read_text()) == text
        print('PASS private Desktop IME caret, preedit and committed Unicode', flush=True)
        session = uuid.uuid4().hex
        for _ in range(30):
            state = request({'mode': 'status'})
            if state.get('available'):
                break
            time.sleep(.1)
        assert state.get('available'), state
        request({'mode': 'begin', 'context': state['context'], 'session': session, 'sequence': 0})
        request({'mode': 'update', 'text': 'cancel', 'selectionStart': 0, 'selectionEnd': 6, 'session': session, 'sequence': 1})
        request({'mode': 'cancel', 'session': session, 'sequence': 2})
        assert json.loads(result.read_text()) == text
        print('PASS cancelled composition does not insert text', flush=True)
    finally:
        subprocess.run(['systemctl', 'stop', unit], check=False)
