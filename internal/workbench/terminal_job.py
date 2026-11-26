"""Own one interactive job and its controlling terminal for its whole lifetime."""
import contextlib
import fcntl
import json
import os
from pathlib import Path
import runpy
import select
import shlex
import signal
import socket
import sys
import tempfile
import termios
import time


class TerminalJob:
    def __init__(self, channel, request):
        self.channel = channel
        self.request = request
        self.connection = channel["identifier"](request["id"])
        self.folder = channel["ROOT"] / self.connection
        self.owner = os.getpgrp()
        self.child = None
        self.result = None
        self.paused = False
        self.closing = 0

    def signal_received(self, signum, _frame):
        if signum in (signal.SIGHUP, signal.SIGTERM):
            self.closing = signum
        elif signum == signal.SIGWINCH and self.child:
            # Normally the kernel delivers WINCH directly to the foreground
            # CLI. Forward resizes directed at the owner while it is paused.
            with contextlib.suppress(ProcessLookupError):
                os.killpg(self.child, signal.SIGWINCH)

    def poll(self):
        if self.child is None:
            return self.result
        child, status = os.waitpid(self.child, os.WNOHANG | os.WUNTRACED | os.WCONTINUED)
        if not child:
            return None
        if os.WIFSTOPPED(status):
            self.paused = True
            if os.tcgetpgrp(0) == child:
                os.tcsetpgrp(0, self.owner)
        elif os.WIFCONTINUED(status):
            self.paused = False
        else:
            self.child = None
            self.result = os.waitstatus_to_exitcode(status)
            return self.result
        return None

    def resume(self, message):
        # A stop or exit can arrive while the controller is sending its frame.
        if self.poll() is not None:
            return {"error": "terminal_unavailable"}
        state = self.channel["read_json"](self.folder / "state.json", {})
        if (message.get("connection") != self.connection or state.get("closed") or
                not all(message.get(key) and message[key] == state.get(key)
                        for key in ("cli_id", "config", "workspace")) or
                state.get("pid") != self.child or os.getpgid(self.child) != self.child or
                os.getsid(self.child) != os.getsid(0)):
            return {"error": "terminal_conflict"}
        foreground = os.tcgetpgrp(0)
        if self.paused and foreground in (self.owner, self.child):
            # UI input is disabled during suspension. Discard in-flight PTY
            # bytes before returning the terminal to this same process group.
            termios.tcflush(0, termios.TCIFLUSH)
            os.tcsetpgrp(0, self.child)
            os.killpg(self.child, signal.SIGCONT)
            self.paused = False
        elif foreground != self.child:
            return {"error": "terminal_conflict"}
        return {"ok": True}

    def reply(self, client):
        client.settimeout(0.5)
        try:
            with client.makefile("rb") as stream:
                message = json.loads(stream.readline(8192))
            result = self.resume(message) if isinstance(message, dict) else {"error": "terminal_conflict"}
            client.sendall(json.dumps(result).encode() + b"\n")
        except (OSError, ValueError):
            pass  # A disconnected controller cannot abandon the terminal job.

    def reap_within_grace(self):
        until = time.monotonic() + 1
        while True:
            if os.waitpid(self.child, os.WNOHANG)[0]:
                self.child = None
                return True
            if time.monotonic() >= until:
                return False
            time.sleep(0.02)

    def shutdown(self):
        if self.child is None:
            return
        try:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(self.child, signal.SIGTERM)
                os.killpg(self.child, signal.SIGCONT)
            if self.reap_within_grace():
                return
            with contextlib.suppress(ProcessLookupError):
                os.killpg(self.child, signal.SIGKILL)
        except PermissionError:
            # Darwin can return EPERM for an exiting group before its leader is waitable.
            if self.reap_within_grace():
                return
            raise
        os.waitpid(self.child, 0)
        self.child = None

    def run(self):
        modes = termios.tcgetattr(0)
        if os.tcgetpgrp(0) != self.owner:
            raise OSError("interactive terminal is busy")
        self.folder.mkdir(mode=0o700, parents=True, exist_ok=True)
        with socket.socket(socket.AF_UNIX) as listener, contextlib.ExitStack() as resources:
            # A killed VM can leave a socket inode. Only its exclusive owner
            # may replace it on a retry of the same account handoff.
            lock = resources.enter_context((self.folder / "job.lock").open("a"))
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            endpoint = self.folder / "job"
            endpoint.unlink(missing_ok=True)
            # Use a short socket name independently of the length of the home path.
            workspace = os.getcwd()
            os.chdir(self.folder)
            try:
                listener.bind("job")
            finally:
                os.chdir(workspace)
            resources.callback(endpoint.unlink, missing_ok=True)
            listener.listen(4)
            wake_read, wake_write = socket.socketpair()
            resources.enter_context(wake_read)
            resources.enter_context(wake_write)
            wake_write.setblocking(False)
            events = select.poll()
            # The PTY may disconnect without delivering SIGHUP to the owner.
            # Darwin needs explicit hangup/error flags. Do not request POLLIN:
            # queued input belongs to the foreground process.
            events.register(0, select.POLLHUP | select.POLLERR)
            events.register(wake_read, select.POLLIN)
            events.register(listener, select.POLLIN)
            resources.callback(signal.set_wakeup_fd, signal.set_wakeup_fd(wake_write.fileno()))
            for sig in (signal.SIGCHLD, signal.SIGHUP, signal.SIGTERM, signal.SIGWINCH):
                resources.callback(signal.signal, sig, signal.signal(sig, self.signal_received))
            for sig in (signal.SIGINT, signal.SIGQUIT, signal.SIGTSTP, signal.SIGTTOU, signal.SIGTTIN):
                resources.callback(signal.signal, sig, signal.signal(sig, signal.SIG_IGN))
            ready, release = os.pipe()
            self.child = os.fork()
            if self.child == 0:
                try:
                    os.close(release)
                    signal.set_wakeup_fd(-1)
                    for sig in (signal.SIGCHLD, signal.SIGHUP, signal.SIGTERM, signal.SIGWINCH,
                                signal.SIGINT, signal.SIGQUIT, signal.SIGTSTP, signal.SIGTTOU, signal.SIGTTIN):
                        signal.signal(sig, signal.SIG_DFL)
                    os.setpgid(0, 0)
                    if not os.read(ready, 1):
                        os._exit(1)
                    os.close(ready)
                    self.channel["launch"](self.request)
                except Exception as error:
                    print(str(error), file=sys.stderr, flush=True)
                os._exit(1)
            os.close(ready)
            launched = self.child
            try:
                try:
                    os.setpgid(self.child, self.child)
                    os.tcsetpgrp(0, self.child)
                    os.write(release, b"1")
                finally:
                    os.close(release)
                while not self.closing:
                    code = self.poll()
                    if code is not None:
                        return code
                    ready_fds = dict(events.poll())
                    if ready_fds.get(0, 0) & (select.POLLHUP | select.POLLERR | select.POLLNVAL):
                        self.closing = signal.SIGHUP
                    elif wake_read.fileno() in ready_fds:
                        wake_read.recv(4096)
                    elif listener.fileno() in ready_fds:
                        with listener.accept()[0] as client:
                            self.reply(client)
                return -self.closing
            finally:
                self.shutdown()
                # A killed CLI need not emit SessionEnd. Never let its PID be
                # mistaken for another process after it has been reaped.
                if (self.folder / "state.json").is_file():
                    with self.channel["state_file"](self.connection) as state:
                        if state.get("pid") == launched:
                            state.update(closed=True, updated_at=time.time())
                try:
                    # An unrelated foreground task can outlive Claude. Let it
                    # finish without changing its modes or eating its input.
                    while os.tcgetpgrp(0) not in (self.owner, launched, 0):
                        if self.closing:
                            break
                        time.sleep(0.1)
                    else:
                        os.tcsetpgrp(0, self.owner)
                        termios.tcflush(0, termios.TCIFLUSH)
                        termios.tcsetattr(0, termios.TCSANOW, modes)
                except (OSError, termios.error):
                    self.closing = signal.SIGHUP


def fallback_shell(channel, request, status):
    runtime = channel["client_runtime"]()
    clean = runtime["environment"](os.environ)
    prefix = request["prefix"]
    # Keep the private account binding local to the command the user invokes.
    # A manually restarted CLI reads its account history without attaching the
    # previous channel. Reopening from the sidebar creates the mobile connection.
    rc = 'if [ -r ~/.bashrc ]; then . ~/.bashrc; fi\n'
    if len(prefix) == 5 and Path(prefix[2]).name == "cage-claude-account" and prefix[3] == "run":
        rc += 'unalias claude 2>/dev/null\nclaude() { command ' + shlex.join(prefix) + ' "$@"; }\n'
    elif request.get("config"):
        rc += "export CLAUDE_CONFIG_DIR=" + shlex.quote(runtime["path"](request["config"])) + "\n"
    rc += "(exit " + str(status) + ")\n"
    with tempfile.TemporaryFile(mode="w+", encoding="utf-8") as stream:
        stream.write("exec " + str(stream.fileno()) + "<&-\n" + rc)
        stream.seek(0)
        os.set_inheritable(stream.fileno(), True)
        os.execve("/bin/bash", ["bash", "--rcfile", "/dev/fd/" + str(stream.fileno()), "-i"], clean)


def main():
    with os.fdopen(int(sys.argv[1]), encoding="utf-8") as stream:
        helper, request, shell_umask = json.load(stream)
    channel = runpy.run_path(helper)
    job = TerminalJob(channel, request)
    code = job.run()
    status = code if code >= 0 else 128 - code
    if not job.closing:
        os.umask(shell_umask)
        fallback_shell(channel, request, status)
    return status


if __name__ == "__main__":
    sys.exit(main())
