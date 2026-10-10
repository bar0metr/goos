#!/bin/sh
# Two sequential boots of one disposable installed disk; no native rebuilds.
# Run from the repository root: remote.sh ISO OVMF_CODE SYSTEM_TREE.
set -eu
[ "$#" = 3 ] || { echo 'usage: remote.sh ISO OVMF_CODE SYSTEM_TREE' >&2; exit 2; }
for tool in python3 ssh ssh-keygen ssh-keyscan qemu-system-x86_64 go; do
 command -v "$tool" >/dev/null || { echo "remote: $tool required" >&2; exit 2; }
done
read -r _hz _diag validation _opt _variant <build/config/build
[ "$validation" = 1 ] || { echo 'remote: VALIDATION=1 build required' >&2; exit 2; }
work=$(mktemp -d /Volumes/Data/tmp/goos-remote-test.XXXXXX)
printf 'remote report: %s\n' "$work"
# This host-only probe asks for forbidden SSH messages with want-reply=true.
# OpenSSH remains the interoperability, authentication and terminal client.
cat >"$work/requests.go" <<'GO'
package main
import (
 "fmt"
 "net"
 "os"
 "time"
 "golang.org/x/crypto/ssh"
 "golang.org/x/crypto/ssh/knownhosts"
)
func die(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
func main() {
 if len(os.Args)!=6 { die(fmt.Errorf("usage: requests ADDRESS KEY KNOWN_HOSTS HOST_ALIAS MODE")) }
 mode:=os.Args[5]
 key,err:=os.ReadFile(os.Args[2]); if err!=nil {die(err)}
 signer,err:=ssh.ParsePrivateKey(key); if err!=nil {die(err)}
 verify,err:=knownhosts.New(os.Args[3]); if err!=nil {die(err)}
 cfg:=&ssh.ClientConfig{User:"user", Auth:[]ssh.AuthMethod{ssh.PublicKeys(signer)}, Timeout:10*time.Second,
  Config:ssh.Config{KeyExchanges:[]string{"curve25519-sha256"}, Ciphers:[]string{"aes128-gcm@openssh.com"}},
  HostKeyAlgorithms:[]string{"ssh-ed25519"}, HostKeyCallback:func(_ string,a net.Addr,k ssh.PublicKey)error{return verify(net.JoinHostPort(os.Args[4],"22"),a,k)}}
 dial:=func()*ssh.Client{c,e:=ssh.Dial("tcp",os.Args[1],cfg);if e!=nil{die(e)};return c}
 // Each rejected request gets a new connection: refusal may close its channel.
 for _,test:=range []struct{name string;data []byte}{
  {"exec",ssh.Marshal(struct{Command string}{"echo FORBIDDEN_EXEC"})},
  {"subsystem",ssh.Marshal(struct{Name string}{"sftp"})},
  {"env",ssh.Marshal(struct{Name,Value string}{"LD_PRELOAD","forbidden"})},
  {"auth-agent-req@openssh.com",nil},
  {"x11-req",ssh.Marshal(struct{Single bool;Protocol,Cookie string;Screen uint32}{false,"MIT-MAGIC-COOKIE-1","00000000000000000000000000000000",0})},
 } {
  if mode!=test.name {continue}
  c:=dial();ch,requests,e:=c.OpenChannel("session",nil);if e!=nil{die(e)};go ssh.DiscardRequests(requests)
  accepted,e:=ch.SendRequest(test.name,true,test.data)
  if e!=nil||accepted {die(fmt.Errorf("%s: require explicit failure reply, accepted=%v error=%v",test.name,accepted,e))}
  fmt.Printf("REJECT %s\n",test.name);ch.Close();c.Close();return
 }
 if mode=="second-session" {
 c:=dial()
 ch,requests,err:=c.OpenChannel("session",nil);if err!=nil{die(err)};go ssh.DiscardRequests(requests)
 second,_,err:=c.OpenChannel("session",nil);if err==nil{second.Close();die(fmt.Errorf("second session channel accepted"))}
 if _,ok:=err.(*ssh.OpenChannelError);!ok{die(fmt.Errorf("second session: not channel refusal: %v",err))}
 fmt.Println("REJECT second-session");ch.Close();c.Close();return
 }
 if mode=="direct-tcpip" {
 c:=dial()
 direct,_,err:=c.OpenChannel("direct-tcpip",ssh.Marshal(struct{Host string;Port uint32;Origin string;OriginPort uint32}{"127.0.0.1",22,"127.0.0.1",12345}))
 if err==nil{direct.Close();die(fmt.Errorf("direct-tcpip accepted"))}
 if _,ok:=err.(*ssh.OpenChannelError);!ok{die(fmt.Errorf("direct-tcpip: not channel refusal: %v",err))}
 fmt.Println("REJECT direct-tcpip");c.Close();return
 }
 if mode=="tcpip-forward" {
 c:=dial();accepted,_,err:=c.SendRequest("tcpip-forward",true,ssh.Marshal(struct{Host string;Port uint32}{"127.0.0.1",9000}))
 if err!=nil||accepted{die(fmt.Errorf("tcpip-forward: require explicit failure reply: accepted=%v error=%v",accepted,err))}
 fmt.Println("REJECT tcpip-forward");c.Close();return
 }
 die(fmt.Errorf("unknown probe %s",mode))
}
GO
go build -o "$work/requests" "$work/requests.go"
python3 -I - "$1" "$2" "$3" "$work" <<'PY'
import errno
import fcntl
import os
from pathlib import Path
import pty
import re
import shutil
import signal
import socket
import struct
import subprocess
import sys
import termios
import time
import tty

iso, firmware, source, work = map(lambda p: Path(p).resolve(), sys.argv[1:])
for path in (iso, firmware, source):
    if not path.exists():
        raise SystemExit(f"remote: missing input {path}")
for name in ("remoted", "remoted-worker", "shell"):
    if not (source / "system/services" / name).is_file():
        raise SystemExit(f"remote: missing service {name}")
for name in ("remote", "gview", "sleep", "cat", "echo", "ps"):
    if not (source / "system/bin" / name).is_file():
        raise SystemExit(f"remote: missing command {name}")
report = (work / "checks.tsv").open("w", buffering=1)
report.write("check\tresult\tdetail\n")
checks = 0
children = []
terminals = []
guest = None
serial = None
boot_number = 0
PROMPT = rb"(?:^|\n)goos> $"


def fail(name, detail):
    report.write(f"{name}\tfail\t{detail}\n")
    # Capture retained service logs before finally stops the failed guest.
    if guest is not None and guest.poll() is None and serial is not None:
        for command in ("log", "services", "ps"):
            try:
                start = serial_log.stat().st_size
                serial.write(command.encode() + b"\n")
                serial.flush()
                end = time.monotonic() + 5
                while time.monotonic() < end:
                    if re.search(PROMPT, serial_log.read_bytes()[start:]):
                        break
                    if guest.poll() is not None:
                        break
                    time.sleep(0.05)
            except (OSError, ValueError):
                break
    raise RuntimeError(f"{name}: {detail}; evidence in {work}")


def require(name, condition, detail):
    global checks
    if not condition:
        fail(name, detail)
    report.write(f"{name}\tpass\t{detail}\n")
    checks += 1


def run(args, timeout=30, env=None):
    return subprocess.run(list(map(str, args)), stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, timeout=timeout, env=env)


def wait_until(predicate, detail, timeout=30):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        for terminal in terminals:
            terminal.pump()
        result = predicate()
        if result:
            return result
        if guest is not None and guest.poll() is not None:
            fail("guest-exited", detail)
        time.sleep(0.05)
    fail("timeout", detail)


# Bind two ephemeral localhost ports together before releasing them to QEMU.
# A collision at QEMU startup fails this run rather than selecting a fixed port.
sockets = [socket.socket(), socket.socket()]
for sock in sockets:
    sock.bind(("127.0.0.1", 0))
ports = [sock.getsockname()[1] for sock in sockets]
for sock in sockets:
    sock.close()
for name in ("login", "wrong"):
    result = run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", f"goos-{name}-test", "-f", work / name])
    require(f"fixture.{name}", result.returncode == 0, "disposable host-only Ed25519 identity")
shutil.copytree(source, work / "tree")
authorized = work / "tree/home/user/.ssh"
authorized.mkdir(parents=True, exist_ok=True)
shutil.copyfile(work / "login.pub", authorized / "authorized_keys")
# Wide, recognizable text proves both initial and resized viewer geometry.
(work / "tree/home/user/remote-view.txt").write_text("G" * 110 + "\nREMOTE_VIEW_BODY\n")
require("image.no-private-key", not (work / "tree/state/security/ssh/ssh_host_ed25519_key").exists(),
        "the image does not supply a server identity")
result = run(["go", "run", "./tools/mkfat32", "-new", "-tree", work / "tree", work / "disk.img"], timeout=120)
(work / "mkfat32.log").write_bytes(result.stdout + result.stderr)
require("disk.install", result.returncode == 0, "one disposable FAT32 disk")
(work / "host-tools.log").write_bytes(run(["ssh", "-V"]).stderr)
known = work / "known_hosts"
known.write_text("")


def boot():
    global guest, serial, serial_log, boot_number
    boot_number += 1
    if boot_number > 2:
        fail("boot-budget", "at most two guest boots")
    serial_log = work / f"guest-{boot_number}.log"
    output = serial_log.open("wb")
    net = f"user,id=net0,hostfwd=tcp:127.0.0.1:{ports[0]}-:22,hostfwd=tcp:127.0.0.1:{ports[1]}-:2222"
    args = ["qemu-system-x86_64", "-machine", "q35", "-smp", os.environ.get("SMP", "1"),
            "-accel", "tcg,thread=multi", "-m", os.environ.get("MEMORY", "512M"),
            "-drive", f"if=pflash,format=raw,readonly=on,file={firmware}",
            "-serial", "stdio", "-display", "none", "-no-reboot", "-cdrom", str(iso),
            "-drive", f"if=none,id=goosdisk,format=raw,file={work / 'disk.img'}",
            "-device", "virtio-blk-pci,drive=goosdisk,disable-modern=on", "-netdev", net,
            "-device", "virtio-net-pci,netdev=net0,disable-modern=on,mac=52:54:00:12:34:56,addr=0x4"]
    guest = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=output, stderr=subprocess.STDOUT)
    output.close()
    children.append(guest)
    serial = guest.stdin
    wait_until(lambda: re.search(PROMPT, serial_log.read_bytes()), "serial shell prompt", 120)
    wait_until(lambda: b"NETD_READY 10.0.2." in serial_log.read_bytes(), "native DHCP ready", 30)
    # Allow validation's storage-driver replacement to complete before writes.
    time.sleep(4)
    require(f"boot{boot_number}.ready", True, "installed shell and native network ready")


def console(command, marker=None):
    start = serial_log.stat().st_size
    serial.write(command.encode() + b"\n")
    serial.flush()
    data = wait_until(lambda: (tail if re.search(PROMPT, tail) else None)
                      if (tail := serial_log.read_bytes()[start:]) else None,
                      f"serial command completed: {command}", 60)
    if marker is not None and not re.search(marker, data):
        fail("console-evidence", f"{command}: missing {marker!r}")
    return data


def config(name, value):
    console(f"config set system.remote.{name} {value}", rb"CONFIG_SET_OK")


def status(**expected):
    data = console("remote status", rb"service=running")
    lines = re.findall(rb"(?:^|\n)(service=running[^\r\n]*)", data)
    if len(lines) != 1:
        fail("status-evidence", "exactly one complete remote status line")
    fields = dict(item.split("=", 1) for item in lines[0].decode().split() if "=" in item)
    for key, value in expected.items():
        if fields.get(key) != str(value):
            fail("status-evidence", f"{key}={value}, received {fields}")
    return fields


def counts(sessions=0, pending=0):
    end = time.monotonic() + 30
    while time.monotonic() < end:
        fields = status()
        if fields.get("sessions") == str(sessions) and fields.get("pending") == str(pending):
            return fields
        for terminal in terminals:
            terminal.pump()
        time.sleep(0.25)
    fail("session-cleanup", f"PID1 counts sessions={sessions} pending={pending}")


def inventory():
    data = console("ps", rb"PS_PROCESS_OK")
    return {int(pid): name.decode() for pid, name in
            re.findall(rb"^([0-9]+)[ \t]+native[ \t]+\S+[ \t]+[0-9]+[ \t]+[0-9]+[ \t]+(\S+)\r?$", data, re.MULTILINE)}


def running_sleep():
    return {pid for pid, name in inventory().items() if name.rsplit("/", 1)[-1] == "sleep"}


def reload_ok():
    data = console("remote reload")
    require("reload.accepted", b"exit status:" not in data and b"SHELL_EXEC_OK" in data,
            "successful explicit service reload")


def scan(port):
    result = run(["ssh-keyscan", "-T", "5", "-p", port, "-t", "ed25519", "127.0.0.1"], 10)
    return [line for line in result.stdout.splitlines() if b" ssh-ed25519 " in line]


def pin_identity(port, fingerprint, initial=False):
    lines = scan(port)
    require(f"boot{boot_number}.hostkey-scan", len(lines) == 1, "one Ed25519 host key from native listener")
    public = b" ".join(lines[0].split()[1:3])
    candidate = work / f"scanned-{boot_number}-{port}.pub"
    candidate.write_bytes(public + b"\n")
    result = run(["ssh-keygen", "-l", "-E", "sha256", "-f", candidate])
    require(f"boot{boot_number}.hostkey-fingerprint", result.returncode == 0 and
            fingerprint.encode() in result.stdout.split(), "serial fingerprint matches scanned host key")
    if initial:
        known.write_bytes(b"goos-remote-test " + public + b"\n")
    else:
        require(f"boot{boot_number}.hostkey-persisted", known.read_bytes() == b"goos-remote-test " + public + b"\n",
                "exact pinned key unchanged across reload/reboot")
    counts()


def ssh_args(port, name, key="login", extra=()):
    return ["ssh", "-F", "/dev/null", "-vv", "-E", str(work / f"{name}.debug.log"),
            "-p", str(port), "-i", str(work / key), "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
            "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ConnectionAttempts=1",
            "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={known}",
            "-o", "GlobalKnownHostsFile=/dev/null", "-o", "HostKeyAlias=goos-remote-test",
            "-o", "KexAlgorithms=curve25519-sha256", "-o", "Ciphers=aes128-gcm@openssh.com",
            "-o", "HostKeyAlgorithms=ssh-ed25519", "-o", "PubkeyAcceptedAlgorithms=ssh-ed25519",
            "-o", "SendEnv=-*", *extra, "user@127.0.0.1"]


class Terminal:
    def __init__(self, port, name):
        self.name, self.rows, self.columns = name, 24, 80
        self.stdout = bytearray()
        self.path = work / f"{name}.stdout.log"
        self.output = self.path.open("wb", buffering=0)
        self.error_path = work / f"{name}.stderr.log"
        self.error = self.error_path.open("wb", buffering=0)
        self.master, slave = pty.openpty()
        tty.setraw(slave)
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", self.rows, self.columns, 0, 0))
        self.process = subprocess.Popen(ssh_args(port, name, extra=("-tt",)), stdin=slave,
                                        stdout=slave, stderr=self.error, start_new_session=True,
                                        env={**os.environ, "TERM": "xterm"})
        os.close(slave)
        os.set_blocking(self.master, False)
        children.append(self.process)
        terminals.append(self)
        self.query_tail = b""

    def pump(self):
        while True:
            try:
                data = os.read(self.master, 65536)
            except OSError as error:
                if error.errno in (errno.EAGAIN, errno.EIO):
                    return
                raise
            if not data:
                return
            self.stdout.extend(data)
            self.output.write(data)
            # A minimal terminal emulator answers only actual size-query DSRs.
            queries = self.query_tail + data
            for _ in range(queries.count(b"\x1b[6n")):
                os.write(self.master, f"\x1b[{self.rows};{self.columns}R".encode())
            self.query_tail = queries[-3:]

    def wait(self, pattern, start=0, timeout=30):
        return wait_until(lambda: re.search(pattern, self.stdout[start:]),
                          f"{self.name}: {pattern!r}", timeout)

    def send(self, data):
        os.write(self.master, data)

    def command(self, text):
        self.pump()
        start = len(self.stdout)
        self.send(text.encode() + b"\n")
        self.wait(PROMPT, start)
        return bytes(self.stdout[start:])

    def resize(self, columns, rows):
        self.columns, self.rows = columns, rows
        fcntl.ioctl(self.master, termios.TIOCSWINSZ, struct.pack("HHHH", rows, columns, 0, 0))
        os.kill(self.process.pid, signal.SIGWINCH)

    def kill(self):
        if self.process.poll() is None:
            self.process.kill()
            self.process.wait(timeout=5)
        self.pump()


def rejected_login(name, port, key="wrong", extra=()):
    result = run(ssh_args(port, name, key, extra), 15)
    debug = (work / f"{name}.debug.log").read_bytes()
    (work / f"{name}.stderr.log").write_bytes(result.stderr)
    require(name, result.returncode != 0 and b"Authentications that can continue: publickey" in debug and
            b"Permission denied (publickey)" in debug + result.stderr, "publickey-only authentication refusal, not transport failure")
    counts()


def poweroff():
    global guest, serial
    start = serial_log.stat().st_size
    serial.write(b"poweroff\n")
    serial.flush()
    wait_until(lambda: b"Powering off..." in serial_log.read_bytes()[start:], "orderly guest shutdown")
    code = guest.wait(timeout=30)
    serial.close()
    guest, serial = None, None
    require(f"boot{boot_number}.poweroff", code == 0, "QEMU exited after disk flush")


try:
    boot()
    initial = status(enabled="false", listener="closed", sessions=0, pending=0,
                     source="system.remote", **{"key-count": 1})
    identity = initial.get("fingerprint", "")
    require("default.disabled", identity.startswith("SHA256:") and not scan(ports[0]),
            "disabled startup has a persistent identity but no SSH listener")
    config("max-sessions", "4")
    config("handshake-timeout-ms", "60000")
    config("enabled", "true")
    reload_ok()
    status(enabled="true", listener="listening", bind="0.0.0.0:22")
    pin_identity(ports[0], identity, initial=True)
    rejected_login("auth.wrong-key", ports[0])
    rejected_login("auth.password-unavailable", ports[0], "login",
                   ("-o", "PreferredAuthentications=password", "-o", "PubkeyAuthentication=no", "-o", "NumberOfPasswordPrompts=0"))

    session = Terminal(ports[0], "primary-shell")
    session.wait(PROMPT)
    require("auth.good-key", session.process.poll() is None, "OpenSSH public-key login opens native shell")
    debug = (work / "primary-shell.debug.log").read_bytes()
    for name, pattern in (("kex", b"kex: algorithm: curve25519-sha256"),
                          ("host-key", b"kex: host key algorithm: ssh-ed25519"),
                          ("cipher", b"cipher: aes128-gcm@openssh.com")):
        require(f"negotiation.{name}", pattern in debug, "OpenSSH debug records selected minimal algorithm")
    body = session.command("echo REMOTE_STDOUT_OK")
    require("shell.stdout", re.search(rb"(?:^|\n)REMOTE_STDOUT_OK\r?\n", body) is not None,
            "native command output, not command echo")
    error = session.command("cat /home/user/remote-missing-file")
    require("shell.stderr", re.search(rb"(?:^|\n)cat: /home/user/remote-missing-file: no such file or directory\r?\n", error) is not None,
            "native stderr reaches the shared PTY stream, like local terminal jobs")
    start = len(session.stdout)
    session.send(b"sleep 60\n")
    session.wait(rb"sleep 60", start)
    interrupted = wait_until(running_sleep, "remote foreground sleep process appears")
    session.send(b"\x03")
    session.wait(PROMPT, start)
    wait_until(lambda: not (interrupted & inventory().keys()), "interrupted foreground process reaped")
    require("terminal.ctrl-c", b"SLEEP_PROCESS_OK" not in session.stdout and session.process.poll() is None,
            "observed foreground process interrupted and reaped; authenticated shell remains usable")
    require("terminal.after-interrupt", b"AFTER_INTERRUPT_OK" in session.command("echo AFTER_INTERRUPT_OK"),
            "fresh command completes after interrupt")

    start = len(session.stdout)
    session.send(b"gview /home/user/remote-view.txt\n")
    session.wait(rb"\x1b\[24;1H", start)
    session.wait(rb"(?<!G)G{80}(?!G)", start)
    require("terminal.raw-initial-geometry", True, "viewer status on row24 and 80-column body from actual 80x24 PTY")
    start = len(session.stdout)
    session.resize(100, 30)
    # Geometry changes are stored for the next foreground launch; this gate
    # does not require a new process-wide resize signal for a running app.
    session.send(b"\x1b[21~")  # F10, an unambiguous raw-mode quit event.
    session.wait(PROMPT, start)
    start = len(session.stdout)
    session.send(b"gview /home/user/remote-view.txt\n")
    session.wait(rb"\x1b\[30;1H", start)
    session.wait(rb"(?<!G)G{100}(?!G)", start)
    require("terminal.raw-window-change", True, "next viewer launch uses SSH window-change row30 and 100-column body")
    session.send(b"\x1b[21~")
    session.wait(PROMPT, start)
    require("terminal.raw-exit", b"RAW_EXIT_OK" in session.command("echo RAW_EXIT_OK"),
            "raw viewer returned control to its own shell")
    start = len(session.stdout)
    session.send(b"\x04")
    wait_until(lambda: session.process.poll() is not None, "Ctrl-D disconnects shell")
    counts()
    require("terminal.ctrl-d-cleanup", session.process.returncode == 0, "EOF disconnect cleanly exited and reaped authenticated session")

    for name in ("exec", "subsystem", "env", "auth-agent-req@openssh.com", "x11-req", "second-session", "direct-tcpip", "tcpip-forward"):
        result = run([work / "requests", f"127.0.0.1:{ports[0]}", work / "login", known, "goos-remote-test", name], 20)
        (work / f"forbidden-{name}.log").write_bytes(result.stdout + result.stderr)
        require(f"forbidden.{name}", result.returncode == 0 and f"REJECT {name}\n".encode() in result.stdout,
                "explicit negative SSH reply/channel-open failure")
        counts()  # Reaping is proved before the next independent connection.

    occupied = []
    # Authenticate each connection before the next handshake; all four shells
    # remain open together. Pending-handshake capacity has a separate check.
    for index in range(4):
        client = Terminal(ports[0], f"capacity-{index}")
        client.wait(PROMPT)
        occupied.append(client)
    counts(sessions=4)
    excess = Terminal(ports[0], "capacity-excess")
    wait_until(lambda: excess.process.poll() is not None, "fifth login refused at max-sessions=4", 20)
    require("capacity.four-sessions", excess.process.returncode != 0 and not re.search(PROMPT, excess.stdout),
            "four authenticated shells coexist; fifth cannot start")
    counts(sessions=4)
    # Native admission exposes two pending workers. A QEMU hostfwd TCP accept
    # alone says nothing about guest admission, so require server identification
    # and PID1 pending counts, then prove an authenticated shell still responds.
    holds = []
    try:
        for index in range(2):
            hold = socket.create_connection(("127.0.0.1", ports[0]), timeout=10)
            hold.settimeout(10)
            holds.append(hold)
            banner = bytearray()
            while b"\n" not in banner and len(banner) < 255:
                piece = hold.recv(255 - len(banner))
                if not piece:
                    break
                banner.extend(piece)
            (work / f"pending-{index}.log").write_bytes(banner)
            require(f"capacity.pending-{index}.native-banner", bytes(banner).startswith(b"SSH-2.0-") and b"\n" in banner,
                    "unauthenticated socket reached native SSH worker")
        counts(sessions=4, pending=2)
        require("capacity.pending-responsiveness", b"PENDING_HELD_OK" in occupied[1].command("echo PENDING_HELD_OK"),
                "authenticated shell completes a command with both pending slots occupied")
    finally:
        for hold in holds:
            hold.close()
    counts(sessions=4, pending=0)
    require("capacity.pending-reaped", True, "both unauthenticated workers released after socket disconnect")

    config("max-sessions", "2")
    reload_ok()
    counts(sessions=4)
    for index, client in enumerate(occupied):
        marker = f"EXISTING_SESSION_{index}_OK"
        require(f"reload.existing-session-{index}", marker.encode() in client.command(f"echo {marker}"),
                "lowered limit preserves an already authenticated session")
    excess = Terminal(ports[0], "capacity-lowered-excess")
    wait_until(lambda: excess.process.poll() is not None, "login refused after max-sessions lowered to2", 20)
    require("capacity.lowered-limit-refused", excess.process.returncode != 0 and not re.search(PROMPT, excess.stdout),
            "new login cannot exceed the lower configured limit")
    counts(sessions=4)
    first, second = occupied[:2]
    start = len(first.stdout)
    first.send(b"sleep 60\n")
    first.wait(rb"sleep 60", start)
    abandoned = wait_until(running_sleep, "foreground job running before abrupt disconnect")
    first.kill()  # Abrupt transport loss must reap the shell and foreground job.
    counts(sessions=3)
    wait_until(lambda: not (abandoned & inventory().keys()), "disconnected foreground job reaped")
    for client in occupied[2:]:
        client.kill()
    counts(sessions=1)
    replacement = Terminal(ports[0], "capacity-replacement")
    replacement.wait(PROMPT)
    counts(sessions=2)
    require("disconnect.capacity-reclaimed", b"REPLACEMENT_OK" in replacement.command("echo REPLACEMENT_OK"),
            "disconnected foreground job and session release capacity for a fresh login")
    second.kill()
    replacement.kill()
    counts()

    config("port", "2222")
    reload_ok()
    before = status(listener="listening", bind="0.0.0.0:2222")
    require("reload.old-port-closed", not scan(ports[0]), "port22 closed after port2222 replacement")
    pin_identity(ports[1], identity)
    config("port", "0")
    data = console("remote reload")
    require("reload.invalid-refused", b"exit status: 2" in data, "invalid policy reload has nonzero command exit")
    after = status(listener="listening", bind="0.0.0.0:2222")
    require("reload.rollback", after.get("generation") == before.get("generation") and after.get("fingerprint") == identity,
            "rejected snapshot does not change generation, listener or identity")
    rollback = Terminal(ports[1], "rollback-shell")
    rollback.wait(PROMPT)
    require("reload.rollback-login", b"ROLLBACK_LOGIN_OK" in rollback.command("echo ROLLBACK_LOGIN_OK"),
            "old applied policy still accepts login after failed reload")
    rollback.kill()
    counts()
    config("port", "2222")
    config("enabled", "false")
    reload_ok()
    status(enabled="false", listener="closed")
    require("reload.disabled", not scan(ports[1]), "disabling closes the current listener")
    config("enabled", "true")
    reload_ok()
    status(enabled="true", listener="listening", bind="0.0.0.0:2222")
    poweroff()

    boot()
    persisted = status(enabled="true", listener="listening", bind="0.0.0.0:2222", sessions=0, pending=0)
    require("reboot.config-and-identity", persisted.get("fingerprint") == identity,
            "enabled state, alternate port and serial identity survive the same disk's reboot")
    stored = console("config get system.remote.max-sessions", rb"CONFIG_GET_OK")
    require("reboot.config-session-limit", re.search(rb"(?:^|\n)2\r?\n", stored) is not None,
            "configured session limit persisted in the configuration store")
    pin_identity(ports[1], identity)
    rebooted = Terminal(ports[1], "reboot-shell")
    rebooted.wait(PROMPT)
    require("reboot.login", b"REBOOT_LOGIN_OK" in rebooted.command("echo REBOOT_LOGIN_OK"),
            "OpenSSH strict pinned-host-key login after reboot")
    start = len(rebooted.stdout)
    rebooted.command("cat /state/security/ssh/ssh_host_ed25519_key")
    rebooted.wait(rb"exit status: [1-9][0-9]*\r?\n", start)
    body = bytes(rebooted.stdout[start:])
    require("login.private-key-denied", b"CAT_PROCESS_OK" not in body and
            re.search(rb"(?:^|\n)cat: /state/security/ssh/ssh_host_ed25519_key: permission denied\r?\n", body) is not None,
            "read of the fingerprint-proven persistent private key failed at the login VFS scope")
    rebooted.send(b"\x04")
    wait_until(lambda: rebooted.process.poll() is not None, "reboot shell EOF disconnect")
    counts()
    poweroff()
    require("boot-budget", boot_number == 2, "exactly two sequential boots of the same unmodified disk")
    print(f"remote: {checks} assertions passed; two persistent boots\nremote report: {work}")
finally:
    for child in reversed(children):
        if child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=5)
    for terminal in terminals:
        os.close(terminal.master)
        terminal.output.close()
        terminal.error.close()
    report.close()
PY
