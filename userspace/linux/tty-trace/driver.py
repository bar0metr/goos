"""Runs terminal programs on a real PTY with scripted keystrokes.

This runs inside an x86_64 Linux container. Each program runs under
qemu-user with QEMU_STRACE set, so every guest syscall lands in a per-pid
log under /out/<scenario>/. The driver also records the screen bytes and
snapshots of the slave termios and the foreground process group at the
points where a scenario asks for them.

usage: python3 -I driver.py [scenario ...]
"""

import errno
import fcntl
import json
import os
import re
import select
import shutil
import signal
import struct
import sys
import termios
import time

WORK = "/work"
OUT = "/out"
TMP = "/tmp/t"
DEFAULT_ROWS, DEFAULT_COLS = 24, 80


class Session:
    def __init__(self, name, argv, env=None, stdin=None, stdout=None,
                 trace=True):
        self.name = name
        self.dir = os.path.join(OUT, name)
        os.makedirs(self.dir, exist_ok=True)
        self.buf = b""
        self.mark = 0
        self.events = []
        self.t0 = time.monotonic()
        self.status = None
        full_env = {
            "PATH": WORK + "/bin:/usr/bin:/bin",
            "HOME": TMP,
            "TERM": "xterm",
            "LANG": "C.UTF-8",
        }
        if env:
            full_env.update(env)
        if trace:
            full_env["QEMU_STRACE"] = "1"
            full_env["QEMU_LOG_FILENAME"] = self.dir + "/strace.%d.log"
        self.master, slave = os.openpty()
        self._setwinsize(self.master, DEFAULT_ROWS, DEFAULT_COLS)
        self.slave_name = os.ttyname(slave)
        self.log("spawn", argv=argv, stdin=stdin, stdout=stdout,
                 tty=self.slave_name)
        self.snapshot("initial")
        pid = os.fork()
        if pid == 0:
            try:
                os.close(self.master)
                os.setsid()
                fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
                for fd in (0, 1, 2):
                    os.dup2(slave, fd)
                if stdin is not None:
                    fd = os.open(stdin, os.O_RDONLY)
                    os.dup2(fd, 0)
                if stdout is not None:
                    fd = os.open(stdout, os.O_WRONLY | os.O_CREAT | os.O_TRUNC,
                                 0o644)
                    os.dup2(fd, 1)
                os.closerange(3, 256)
                os.chdir(TMP)
                os.execve(argv[0], argv, full_env)
            finally:
                os._exit(127)
        os.close(slave)
        self.pid = pid

    @staticmethod
    def _setwinsize(fd, rows, cols):
        fcntl.ioctl(fd, termios.TIOCSWINSZ,
                    struct.pack("HHHH", rows, cols, 0, 0))

    def now(self):
        return round(time.monotonic() - self.t0, 3)

    def log(self, kind, **kw):
        kw["t"] = self.now()
        kw["event"] = kind
        self.events.append(kw)
        if getattr(self, "closed", False):
            self.write_events()

    def write_events(self):
        with open(os.path.join(self.dir, "events.json"), "w") as f:
            json.dump(self.events, f, indent=1)

    def pump(self, timeout):
        r, _, _ = select.select([self.master], [], [], timeout)
        if not r:
            return False
        try:
            data = os.read(self.master, 65536)
        except OSError as e:
            if e.errno == errno.EIO:
                return None
            raise
        if not data:
            return None
        self.buf += data
        return True

    def reap(self):
        if self.status is not None:
            return
        try:
            pid, st = os.waitpid(self.pid, os.WNOHANG | os.WUNTRACED
                                 | os.WCONTINUED)
        except ChildProcessError:
            return
        if pid == 0:
            return
        if os.WIFSTOPPED(st):
            self.log("child-stopped", sig=os.WSTOPSIG(st))
        elif os.WIFCONTINUED(st):
            self.log("child-continued")
        else:
            self.status = st
            self.log("child-exited", wait_status=st,
                     code=os.waitstatus_to_exitcode(st))

    def expect(self, pattern, timeout=30.0):
        """Waits for pattern in output written since the last send."""
        rx = re.compile(pattern)
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if rx.search(self.buf, self.mark):
                self.log("matched", pattern=pattern.decode("latin-1"))
                return True
            if self.pump(0.2) is None:
                break
            self.reap()
        ok = bool(rx.search(self.buf, self.mark))
        self.log("matched" if ok else "timeout",
                 pattern=pattern.decode("latin-1"))
        return ok

    def settle(self, quiet=1.5, limit=20.0):
        """Waits until the program has been silent for quiet seconds."""
        end = time.monotonic() + limit
        last = time.monotonic()
        while time.monotonic() < end:
            got = self.pump(0.2)
            self.reap()
            if got:
                last = time.monotonic()
            elif got is None or time.monotonic() - last >= quiet:
                return

    def send(self, data):
        self.mark = len(self.buf)
        self.log("send", data=data.decode("latin-1"))
        os.write(self.master, data)

    def winsize(self, rows, cols):
        # TIOCSWINSZ on the master makes the kernel send SIGWINCH to the
        # foreground process group, as a terminal emulator resize does.
        self.mark = len(self.buf)
        self.log("winsize", rows=rows, cols=cols)
        self._setwinsize(self.master, rows, cols)

    def kill(self, sig, group=False):
        self.mark = len(self.buf)
        self.log("kill", sig=sig, group=group)
        if group:
            os.killpg(self.pid, sig)
        else:
            os.kill(self.pid, sig)

    def fg_pgrp(self):
        try:
            return os.tcgetpgrp(self.master)
        except OSError:
            return None

    def wait_fg_change(self, away_from, timeout=20.0):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            pg = self.fg_pgrp()
            if pg is not None and pg != away_from:
                self.log("fg-pgrp", pgrp=pg)
                return pg
            self.pump(0.2)
            self.reap()
        self.log("timeout", what="fg-pgrp change")
        return None

    def wait_stopped(self, timeout=20.0):
        end = time.monotonic() + timeout
        n = len(self.events)
        while time.monotonic() < end:
            self.reap()
            if any(e["event"] == "child-stopped" for e in self.events[n:]):
                return True
            self.pump(0.2)
        self.log("timeout", what="child stop")
        return False

    def snapshot(self, label):
        try:
            iflag, oflag, cflag, lflag, isp, osp, cc = \
                termios.tcgetattr(self.master)
        except termios.error as e:
            self.log("termios", label=label, error=str(e))
            return
        ws = fcntl.ioctl(self.master, termios.TIOCGWINSZ, b"\0" * 8)
        self.log("termios", label=label,
                 c_iflag=hex(iflag), c_oflag=hex(oflag),
                 c_cflag=hex(cflag), c_lflag=hex(lflag),
                 VMIN=cc[termios.VMIN] if isinstance(cc[termios.VMIN], int)
                 else ord(cc[termios.VMIN]),
                 VTIME=cc[termios.VTIME]
                 if isinstance(cc[termios.VTIME], int)
                 else ord(cc[termios.VTIME]),
                 cc=[c if isinstance(c, int) else ord(c) for c in cc],
                 winsize=list(struct.unpack("HHHH", ws)),
                 fg_pgrp=self.fg_pgrp(),
                 child=getattr(self, "pid", None))

    def finish(self, timeout=30.0):
        end = time.monotonic() + timeout
        while self.status is None and time.monotonic() < end:
            if self.pump(0.2) is None:
                # Slave closed; the child is exiting.
                time.sleep(0.1)
            self.reap()
        if self.status is None:
            self.log("timeout", what="exit; killing")
            try:
                os.kill(self.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            os.waitpid(self.pid, 0)
        while self.pump(0.1):
            pass
        os.close(self.master)
        with open(os.path.join(self.dir, "screen.raw"), "wb") as f:
            f.write(self.buf)
        self.closed = True
        self.write_events()
        timeouts = [e for e in self.events if e["event"] == "timeout"]
        print("%-28s %s" % (self.name,
                            "ok" if not timeouts else
                            "TIMEOUTS: %r" % timeouts), file=sys.stderr)


NANO = WORK + "/bin/nano"
SH = WORK + "/bin/sh"
CTRL_X, CTRL_O, CTRL_Z, CTRL_C = b"\x18", b"\x0f", b"\x1a", b"\x03"
CTRL_T = b"\x14"
NANO_READY = rb"Exit"
PROMPT = rb"PS1> "
NO_TERMINAL = rb"Error opening terminal"


def reset_tmp():
    shutil.rmtree(TMP, ignore_errors=True)
    os.makedirs(TMP)
    with open(TMP + "/existing.txt", "w") as f:
        f.write("first line\nsecond line\n")
    os.chmod(TMP + "/existing.txt", 0o640)


def nano_start_exit():
    s = Session("nano-start-exit", [NANO, "-I"])
    s.expect(NANO_READY)
    s.settle()
    s.snapshot("nano-running")
    s.send(CTRL_X)
    s.finish()


def nano_open_existing():
    s = Session("nano-open-existing", [NANO, "-I", "existing.txt"])
    s.expect(rb"second line")
    s.settle()
    s.send(CTRL_X)
    s.finish()


def nano_edit_save():
    s = Session("nano-edit-save", [NANO, "-I", "existing.txt"])
    s.expect(rb"second line")
    s.settle()
    s.send(b"added text ")
    s.settle()
    s.send(CTRL_O)
    s.expect(rb"Write to File")
    s.settle()
    s.send(b"\r")
    s.expect(rb"Wrote")
    s.settle()
    s.send(CTRL_X)
    s.finish()
    s.log("file", path="existing.txt",
          content=open(TMP + "/existing.txt").read(),
          mode=oct(os.stat(TMP + "/existing.txt").st_mode))


def nano_new_save():
    s = Session("nano-new-save", [NANO, "-I", "new.txt"])
    s.expect(NANO_READY)
    s.settle()
    s.send(b"hello from nano\r")
    s.settle()
    s.send(CTRL_O)
    s.expect(rb"Write to File")
    s.settle()
    s.send(b"\r")
    s.expect(rb"Wrote")
    s.settle()
    s.send(CTRL_X)
    s.finish()
    s.log("file", path="new.txt", content=open(TMP + "/new.txt").read(),
          mode=oct(os.stat(TMP + "/new.txt").st_mode))


def nano_resize():
    s = Session("nano-resize", [NANO, "-I", "existing.txt"])
    s.expect(rb"second line")
    s.settle()
    s.winsize(30, 100)
    s.settle()
    s.snapshot("after-grow")
    s.winsize(20, 60)
    s.settle()
    s.snapshot("after-shrink")
    s.send(CTRL_X)
    s.finish()


def nano_suspend():
    s = Session("nano-suspend", [NANO, "-I", "existing.txt"])
    s.expect(rb"second line")
    s.settle()
    s.snapshot("before-suspend")
    s.send(CTRL_Z)
    s.expect(rb"To suspend")
    s.settle(quiet=1.0)
    s.send(CTRL_T)
    s.settle(quiet=1.0, limit=5)
    s.send(CTRL_Z)
    stopped = s.wait_stopped(timeout=15)
    s.settle(quiet=1.0, limit=5)
    s.snapshot("stopped" if stopped else "ctrl-z-no-stop")
    if stopped:
        s.kill(signal.SIGCONT)
        s.settle()
        s.snapshot("continued")
    s.send(CTRL_X)
    s.finish()


def nano_stdout_redirected():
    s = Session("nano-stdout-file", [NANO, "-I", "existing.txt"],
                stdout=TMP + "/stdout.out")
    s.settle(quiet=3)
    s.send(CTRL_X)
    s.finish(timeout=10)
    s.log("file", path="stdout.out",
          content=open(TMP + "/stdout.out", "rb").read()[:400]
          .decode("latin-1"))


def nano_stdin_redirected():
    with open(TMP + "/piped.txt", "w") as f:
        f.write("text from stdin\n")
    s = Session("nano-stdin-file", [NANO, "-I", "-"],
                stdin=TMP + "/piped.txt")
    s.expect(rb"text from stdin")
    s.settle()
    s.snapshot("nano-running")
    s.send(CTRL_X)
    s.expect(rb"Save modified buffer")
    s.settle()
    s.send(b"n")
    s.finish()


def nano_stdin_devnull():
    s = Session("nano-stdin-devnull", [NANO, "-I", "existing.txt"],
                stdin="/dev/null")
    s.settle(quiet=3)
    s.send(CTRL_X)
    s.finish(timeout=15)


XTERM_KEYS = (b"\x1bOB", b"\x1bOF", b"\x1bOH", b"\x1b[3~")
GOOS_KEYS = (b"\x1b[B", b"\x1b[F", b"\x1b[H", b"\x1b[3~")


def nano_keys(name, term, keys, terminfo=None):
    """Edits with Down, End, Home and Delete, then saves the result.

    The expected file is "first line\\necond lineX\\n" when every key
    sequence matched the terminal description.
    """
    env = {"TERM": term}
    if terminfo:
        env["TERMINFO"] = terminfo
    down, end, home, delete = keys
    s = Session(name, [NANO, "-I", "existing.txt"], env=env)
    s.expect(rb"second line")
    s.settle()
    for key in (down, end, b"X", home, delete):
        s.send(key)
        s.settle(quiet=1.0, limit=5)
    s.send(CTRL_O)
    s.expect(rb"Write to File")
    s.settle()
    s.send(b"\r")
    s.expect(rb"Wrote")
    s.settle()
    s.send(CTRL_X)
    s.finish()
    s.log("file", path="existing.txt",
          content=open(TMP + "/existing.txt").read())


def nano_term(name, term, terminfo=None, hide_db=False,
              ready=rb"second line"):
    """Starts nano with one terminal description and quits.

    With hide_db and no TERMINFO, every database directory on nano's search
    path is moved away, which shows whether ncurses has compiled-in
    fallback entries; the expected result is "Error opening terminal".
    """
    env = {"TERM": term}
    if terminfo:
        env["TERMINFO"] = terminfo
    moved = []
    if hide_db:
        for d in ("/etc/terminfo", "/usr/share/terminfo", "/lib/terminfo",
                  "/usr/lib/terminfo"):
            if os.path.exists(d):
                shutil.move(d, d + ".hidden")
                moved.append(d)
    try:
        s = Session(name, [NANO, "-I", "existing.txt"], env=env)
        s.expect(ready, timeout=20)
        s.settle()
        s.send(CTRL_X)
        s.finish(timeout=15)
    finally:
        for d in moved:
            shutil.move(d + ".hidden", d)


def sh_cmd(s, cmd, quiet=1.0):
    s.send(cmd + b"\r")
    s.expect(PROMPT)
    s.settle(quiet=quiet, limit=10)


def busybox_sh_jobs():
    s = Session("busybox-sh-jobs", [SH, "-i"], env={"PS1": "PS1> "})
    s.expect(PROMPT)
    s.settle()
    s.snapshot("shell-idle")
    shell = s.pid
    sh_cmd(s, b"echo hello")
    sh_cmd(s, b"ls /work/bin")
    # Foreground child, interrupted with ^C.
    s.send(b"sleep 30\r")
    s.wait_fg_change(shell)
    s.snapshot("sleep-foreground")
    s.send(CTRL_C)
    s.expect(PROMPT)
    s.settle(quiet=1.0)
    s.snapshot("after-ctrl-c")
    # Foreground child, stopped with ^Z, resumed with fg, then ^C.
    s.send(b"sleep 30\r")
    s.wait_fg_change(shell)
    s.send(CTRL_Z)
    s.expect(PROMPT)
    s.settle(quiet=1.0)
    s.snapshot("after-ctrl-z")
    sh_cmd(s, b"jobs")
    s.send(b"fg\r")
    s.wait_fg_change(shell)
    s.settle(quiet=1.0, limit=5)
    s.snapshot("after-fg")
    s.send(CTRL_C)
    s.expect(PROMPT)
    s.settle(quiet=1.0)
    # Background job, then a background reader that gets SIGTTIN.
    sh_cmd(s, b"sleep 1 &")
    sh_cmd(s, b"wait")
    sh_cmd(s, b"cat &", quiet=3.0)
    sh_cmd(s, b"jobs")
    sh_cmd(s, b"kill -9 %1")
    sh_cmd(s, b"jobs")
    # Background writer with tostop set gets SIGTTOU.
    sh_cmd(s, b"stty tostop")
    sh_cmd(s, b"echo bgout &", quiet=3.0)
    sh_cmd(s, b"jobs")
    sh_cmd(s, b"kill -9 %1")
    sh_cmd(s, b"stty -tostop")
    # Foreground pipeline and exit status.
    sh_cmd(s, b"echo a b c | cat")
    sh_cmd(s, b"false; echo status=$?")
    s.send(b"exit\r")
    s.finish()


def busybox_sh_nano():
    s = Session("busybox-sh-nano", [SH, "-i"], env={"PS1": "PS1> "})
    s.expect(PROMPT)
    s.settle()
    shell = s.pid
    s.send(b"nano -I existing.txt\r")
    s.wait_fg_change(shell)
    s.expect(rb"second line")
    s.settle()
    s.snapshot("nano-foreground")
    s.send(CTRL_T)
    s.settle(quiet=1.0, limit=5)
    s.send(CTRL_Z)
    s.expect(PROMPT)
    s.settle(quiet=1.0)
    s.snapshot("nano-stopped")
    sh_cmd(s, b"jobs")
    s.send(b"fg\r")
    s.wait_fg_change(shell)
    s.expect(rb"second line")
    s.settle()
    s.snapshot("nano-resumed")
    s.send(CTRL_X)
    s.expect(PROMPT)
    s.settle(quiet=1.0)
    s.snapshot("shell-after-nano")
    s.send(b"exit\r")
    s.finish()


SCENARIOS = {
    "nano-start-exit": nano_start_exit,
    "nano-open-existing": nano_open_existing,
    "nano-edit-save": nano_edit_save,
    "nano-new-save": nano_new_save,
    "nano-resize": nano_resize,
    "nano-suspend": nano_suspend,
    "nano-stdout-file": nano_stdout_redirected,
    "nano-stdin-file": nano_stdin_redirected,
    "nano-stdin-devnull": nano_stdin_devnull,
    "nano-term-linux-nodb": lambda: nano_term(
        "nano-term-linux-nodb", "linux", hide_db=True, ready=NO_TERMINAL),
    "nano-term-xterm-nodb": lambda: nano_term(
        "nano-term-xterm-nodb", "xterm", hide_db=True, ready=NO_TERMINAL),
    "nano-term-vt100-nodb": lambda: nano_term(
        "nano-term-vt100-nodb", "vt100", hide_db=True, ready=NO_TERMINAL),
    "nano-term-goos": lambda: nano_term("nano-term-goos", "goos",
                                        terminfo=WORK + "/terminfo",
                                        hide_db=True),
    "nano-term-db-xterm-256color": lambda: nano_term(
        "nano-term-db-xterm-256color", "xterm-256color",
        terminfo=WORK + "/terminfo", hide_db=True),
    "nano-term-db-vt100": lambda: nano_term(
        "nano-term-db-vt100", "vt100", terminfo=WORK + "/terminfo",
        hide_db=True),
    "nano-term-system-xterm-256color": lambda: nano_term(
        "nano-term-system-xterm-256color", "xterm-256color"),
    "nano-keys-xterm": lambda: nano_keys("nano-keys-xterm", "xterm",
                                         XTERM_KEYS),
    "nano-keys-goos": lambda: nano_keys("nano-keys-goos", "goos", GOOS_KEYS,
                                        terminfo=WORK + "/terminfo"),
    "busybox-sh-jobs": busybox_sh_jobs,
    "busybox-sh-nano": busybox_sh_nano,
}


def main():
    names = sys.argv[1:] or list(SCENARIOS)
    if not os.path.isdir(WORK + "/terminfo"):
        names = [n for n in names
                 if n not in ("nano-term-goos", "nano-keys-goos",
                              "nano-term-db-xterm-256color",
                              "nano-term-db-vt100")]
    for name in names:
        reset_tmp()
        SCENARIOS[name]()


if __name__ == "__main__":
    main()
