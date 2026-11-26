import concurrent.futures
import errno
import hashlib
import json
import os
from pathlib import Path
import pty
import select
import shlex
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import time
import unittest
import uuid
import fcntl


SOURCE = Path(__file__).resolve().parent


def fixture_cli(args):
    config = Path(os.environ["CLAUDE_CONFIG_DIR"]).resolve()
    if args[-2:] == ["auth", "status"]:
        print(json.dumps({"loggedIn": True, "authMethod": "claude.ai", "apiProvider": "firstParty",
                          "email": "fixture@example.invalid", "configDirectory": str(config)}))
        return
    assert not any(key.startswith("CAGE_") for key in os.environ)
    if "--session-id" in args:
        (config / "fixture-history").write_text(args[args.index("--session-id") + 1])
    def window_changed(*_):
        os.write(1, f"window={os.get_terminal_size().columns}\n".encode())
    signal.signal(signal.SIGWINCH, window_changed)
    print("cli-ready=" + str(os.getpid()), flush=True)
    for line in sys.stdin:
        if line.strip() == "/exit":
            print("cli-exiting", flush=True)
            time.sleep(0.15)
            sys.exit(23)
        if line.strip() == "other":
            # An unrelated foreground job owns an unfinished input line.
            owner = os.getpgid(os.getppid())
            ready, go = os.pipe()
            other = os.fork()
            if other == 0:
                os.close(go)
                os.setpgid(0, 0)
                os.read(ready, 1)
                print("other-ready", flush=True)
                print("other-input=" + sys.stdin.readline().strip(), flush=True)
                os.tcsetpgrp(0, owner)
                os._exit(0)
            os.close(ready)
            os.setpgid(other, other)
            os.tcsetpgrp(0, other)
            os.write(go, b"1")
            os.close(go)
            os.kill(os.getpid(), signal.SIGSTOP)
            os.waitpid(other, 0)
            continue
        if line.strip() == "/resume":
            print("history=" + (config / "fixture-history").read_text(), flush=True)
        else:
            print("foreground=" + str(os.tcgetpgrp(0)), flush=True)


class TerminalJobTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        home = Path(self.temp.name).resolve()
        self.workspace = home / "project"
        self.workspace.mkdir()
        (home / ".bashrc").write_text("PS1='test-shell> '\n")
        bin_dir = home / "bin"
        bin_dir.mkdir()
        installed = home / ".local/share/cage/cli-channel-code"
        installed.mkdir(parents=True)
        self.channel = installed / "fixture.py"
        shutil.copyfile(SOURCE / "cli_channel.py", self.channel)
        shutil.copyfile(SOURCE / "terminal_job.py", self.channel.with_suffix(".terminal.py"))
        helper = bin_dir / "cage-claude-account"
        shutil.copyfile(SOURCE.parent / "cage" / "claude-account.py", helper)
        helper.chmod(0o700)
        shutil.copyfile(SOURCE.parent / "cage" / "client_runtime.py", bin_dir / "client_runtime.py")
        binary = bin_dir / "claude"
        binary.write_text("#!/bin/sh\nexec " + shlex.join([sys.executable, "-I", __file__, "--fixture"]) + ' "$@"\n')
        binary.chmod(0o700)
        account = "ab" * 12
        subject = hashlib.sha256(b"fixture@example.invalid\n").hexdigest()
        self.session, connection = str(uuid.uuid4()), str(uuid.uuid4())
        self.folder = home / ".local/share/cage/cli-channels" / connection
        self.message = {"cli_id": self.session, "connection": connection, "workspace": str(self.workspace),
                        "config": str(home / ".local/share/cage/claude-accounts" / account / "config")}
        request = {"id": connection, "session_id": self.session,
                   "prefix": ["env", "CAGE_CLAUDE_SUBJECT=" + subject, str(helper), "run", account]}
        self.env = {"HOME": str(home), "PATH": str(bin_dir) + ":/usr/bin:/bin", "TERM": "xterm",
                    "TMPDIR": str(home), "CAGE_WORKBENCH": "1", "LANG": "C"}
        self.pid, self.master = pty.fork()
        if self.pid == 0:
            os.chdir(self.workspace)
            os.umask(0o027)
            os.execve(sys.executable, [sys.executable, "-I", str(self.channel), "terminal",
                                       json.dumps(request)], self.env)
        self.addCleanup(self.close)
        self.output = b""
        self.wait_for(b"cli-ready=")
        self.child = json.loads((self.folder / "state.json").read_text())["pid"]
        self.wait_for(str(self.child).encode())
        command = subprocess.check_output(["ps", "-ww", "-o", "command=", "-p", str(self.pid)], text=True)
        for private in (str(installed), "CAGE_", account, subject, self.session, connection):
            self.assertNotIn(private, command)

    def close(self):
        if self.master is not None:
            os.close(self.master)
            self.master = None
        deadline = time.monotonic() + 4
        while time.monotonic() < deadline:
            if os.waitpid(self.pid, os.WNOHANG)[0]:
                return
            time.sleep(0.01)
        os.kill(self.pid, signal.SIGKILL)
        os.waitpid(self.pid, 0)
        self.fail("terminal owner did not exit after hangup")

    def wait_for(self, text):
        deadline = time.monotonic() + 7
        while text not in self.output and time.monotonic() < deadline:
            if select.select([self.master], [], [], 0.1)[0]:
                try:
                    self.output += os.read(self.master, 8192)
                except OSError as error:
                    if error.errno == errno.EIO:
                        break
                    raise
        self.assertIn(text, self.output)
        self.output = self.output.split(text, 1)[1]

    def pause(self):
        os.write(self.master, b"\x1a")
        until = time.monotonic() + 3
        while time.monotonic() < until:
            if os.tcgetpgrp(self.master) == self.pid:
                return
            time.sleep(0.01)
        self.fail("Ctrl+Z did not return foreground ownership to the supervisor")

    def resume(self, **override):
        reply = subprocess.run([sys.executable, "-I", str(self.channel), "continue", "-"],
                               input=json.dumps({**self.message, **override}), env=self.env,
                               text=True, capture_output=True, timeout=4, check=True)
        return json.loads(reply.stdout)

    def test_continue_same_group_and_account_after_exit(self):
        for _ in range(2):
            self.pause()
            self.assertEqual(self.resume(config="/different"), {"error": "terminal_conflict"})
            self.assertEqual(os.tcgetpgrp(self.master), self.pid)
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                replies = list(pool.map(lambda _: self.resume(), range(2)))
            self.assertEqual(replies, [{"ok": True}, {"ok": True}])
            os.write(self.master, b"ping\n")
            self.wait_for(b"foreground=" + str(self.child).encode())
        fcntl.ioctl(self.master, termios.TIOCSWINSZ, struct.pack("HHHH", 43, 117, 0, 0))
        self.wait_for(b"window=117")
        os.write(self.master, b"/exit\n")
        self.wait_for(b"cli-exiting")
        os.write(self.master, b"touch stale-input-executed\n")
        self.wait_for(b"test-shell> ")
        os.write(self.master, b"""printf 'status=%d flags=%d mask=%s\\n' "$?" "$(env | grep -c '^CAGE_')" "$(umask)"\n""")
        self.wait_for(b"status=23 flags=0 mask=0027")
        self.assertFalse((self.workspace / "stale-input-executed").exists())
        os.write(self.master, b"claude\n")
        self.wait_for(b"cli-ready=")
        os.write(self.master, b"/resume\n")
        self.wait_for(b"history=" + self.session.encode())
        self.assertEqual(self.resume(), {"error": "terminal_unavailable"})

    def test_close_reaps_suspended_job(self):
        self.pause()
        os.close(self.master)
        self.master = None
        self.assert_reaped()

    def test_stop_reaps_suspended_job(self):
        self.pause()
        os.kill(self.pid, signal.SIGTERM)
        self.assert_reaped()

    def assert_reaped(self):
        until = time.monotonic() + 4
        while time.monotonic() < until:
            if json.loads((self.folder / "state.json").read_text()).get("closed"):
                with self.assertRaises(ProcessLookupError):
                    os.kill(self.child, 0)
                return
            time.sleep(0.01)
        self.fail("suspended CLI survived terminal close")

    def test_other_foreground_job_keeps_its_input(self):
        os.write(self.master, b"other\n")
        self.wait_for(b"other-ready")
        os.write(self.master, b"half")
        self.assertEqual(self.resume(), {"error": "terminal_conflict"})
        os.write(self.master, b"-line\n")
        self.wait_for(b"other-input=half-line")
        self.assertEqual(self.resume(), {"ok": True})
        os.write(self.master, b"ping\n")
        self.wait_for(b"foreground=" + str(self.child).encode())


if __name__ == "__main__":
    if sys.argv[1:2] == ["--fixture"]:
        fixture_cli(sys.argv[2:])
    else:
        unittest.main()
