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
 "strings"
 "time"
 "golang.org/x/crypto/ssh"
 "golang.org/x/crypto/ssh/knownhosts"
)
func die(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
func main() {
 if len(os.Args)!=7 { die(fmt.Errorf("usage: requests ADDRESS KEY KNOWN_HOSTS HOST_ALIAS USER MODE")) }
 mode:=os.Args[6]
 key,err:=os.ReadFile(os.Args[2]); if err!=nil {die(err)}
 signer,err:=ssh.ParsePrivateKey(key); if err!=nil {die(err)}
 verify,err:=knownhosts.New(os.Args[3]); if err!=nil {die(err)}
 cfg:=&ssh.ClientConfig{User:os.Args[5], Auth:[]ssh.AuthMethod{ssh.PublicKeys(signer)}, Timeout:10*time.Second,
  Config:ssh.Config{KeyExchanges:[]string{"curve25519-sha256"}, Ciphers:[]string{"aes128-gcm@openssh.com"}},
  HostKeyAlgorithms:[]string{"ssh-ed25519"}, HostKeyCallback:func(_ string,a net.Addr,k ssh.PublicKey)error{return verify(net.JoinHostPort(os.Args[4],"22"),a,k)}}
 dial:=func()*ssh.Client{c,e:=ssh.Dial("tcp",os.Args[1],cfg);if e!=nil{die(e)};return c}
 // Each rejected request gets a new connection: refusal may close its channel.
 for _,test:=range []struct{name string;data []byte}{
  {"exec-malformed",[]byte{0,0,0,5,'x'}},
  {"exec-oversized",ssh.Marshal(struct{Command string}{strings.Repeat("x",4097)})},
  {"subsystem",ssh.Marshal(struct{Name string}{"sftp"})},
  {"env",ssh.Marshal(struct{Name,Value string}{"LD_PRELOAD","forbidden"})},
  {"auth-agent-req@openssh.com",nil},
  {"x11-req",ssh.Marshal(struct{Single bool;Protocol,Cookie string;Screen uint32}{false,"MIT-MAGIC-COOKIE-1","00000000000000000000000000000000",0})},
 } {
  if mode!=test.name {continue}
  c:=dial();ch,requests,e:=c.OpenChannel("session",nil);if e!=nil{die(e)};go ssh.DiscardRequests(requests)
  request:=test.name;if strings.HasPrefix(request,"exec-"){request="exec"}
  accepted,e:=ch.SendRequest(request,true,test.data)
  if e!=nil||accepted {die(fmt.Errorf("%s: require explicit failure reply, accepted=%v error=%v",test.name,accepted,e))}
  fmt.Printf("REJECT %s\n",test.name);ch.Close();c.Close();return
 }
 if mode=="pty-exec" || mode=="second-exec" || mode=="exec-shell" || mode=="exec-pty" {
  c:=dial();ch,requests,e:=c.OpenChannel("session",nil);if e!=nil{die(e)};go ssh.DiscardRequests(requests)
  request:="exec";payload:=ssh.Marshal(struct{Command string}{"sleep 60"})
  if mode=="pty-exec" {request="pty-req";payload=ssh.Marshal(struct{Term string;Columns,Rows,Width,Height uint32;Modes string}{"xterm",80,24,0,0,"\x00"})}
  accepted,e:=ch.SendRequest(request,true,payload);if e!=nil||!accepted{die(fmt.Errorf("initial %s refused: %v",request,e))}
  request="exec";payload=ssh.Marshal(struct{Command string}{"uname"})
  if mode=="exec-shell" {request="shell";payload=nil}
  if mode=="exec-pty" {request="pty-req";payload=ssh.Marshal(struct{Term string;Columns,Rows,Width,Height uint32;Modes string}{"xterm",80,24,0,0,"\x00"})}
  accepted,e=ch.SendRequest(request,true,payload)
  if e!=nil||accepted{die(fmt.Errorf("%s: require failure reply accepted=%v error=%v",mode,accepted,e))}
  fmt.Printf("REJECT %s\n",mode);ch.Close();c.Close();return
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
for name in ("remote", "gview", "gfm", "sleep", "cat", "echo", "ps", "head", "tail",
             "grep", "wc", "find", "hexdump", "hostname", "date"):
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
PROMPT = rb"(?:^|\n)(?:\x1b\[[0-9;?]*[A-Za-z])*goos> $"
REMOTE_USER = "remote-admin"
AUTHORIZED_KEYS_PATH = "/home/user/remote-keys/authorized_keys"
CORRUPT_HOST_KEY_PATH = "/state/security/ssh/corrupt_host_ed25519_key"
RECOVERED_HOST_KEY_PATH = "/state/security/ssh/recovered_host_ed25519_key"
REKEY_OUTPUT_PATH = "/home/user/remote-rekey.txt"


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


def run(args, timeout=30, env=None, input_data=None):
    return subprocess.run(list(map(str, args)), stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, timeout=timeout, env=env, input=input_data)


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
for name in ("login", "wrong", "second"):
    result = run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", f"goos-{name}-test", "-f", work / name])
    require(f"fixture.{name}", result.returncode == 0, "disposable host-only Ed25519 identity")
shutil.copytree(source, work / "tree")
authorized = work / "tree" / str(Path(AUTHORIZED_KEYS_PATH).parent).lstrip("/")
authorized.mkdir(parents=True, exist_ok=True)
shutil.copyfile(work / "login.pub", authorized / "authorized_keys")
corrupt = work / "tree" / CORRUPT_HOST_KEY_PATH.lstrip("/")
corrupt.parent.mkdir(parents=True, exist_ok=True)
corrupt.write_bytes(b"CORRUPT_ALT_HOST_KEY\n")
(work / "tree" / REKEY_OUTPUT_PATH.lstrip("/")).write_bytes(b"R" * 2048 + b"\n")
persisted_config = work / "tree/state/config"
previous = persisted_config.read_bytes() if persisted_config.exists() else b""
if previous and not previous.endswith(b"\n"):
    previous += b"\n"
persisted_config.write_bytes(previous + (
    "system.remote.enabled bool true\n"
    "system.remote.port uint 0\n"
    f"system.remote.user text {REMOTE_USER}\n"
    f"system.remote.authorized-keys text {AUTHORIZED_KEYS_PATH}\n"
    f"system.remote.host-key text {CORRUPT_HOST_KEY_PATH}\n").encode())
# Wide, recognizable text proves both initial and resized viewer geometry.
(work / "tree/home/user/remote-view.txt").write_text("G" * 150 + "\nREMOTE_VIEW_BODY\n")
(work / "tree/home/user/admin-lines.txt").write_bytes(b"one\ntwo\nthree\n")
(work / "tree/home/user/admin-bytes.bin").write_bytes(b"\x00\x01ABC\xff")
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


def ssh_args(port, name, key="login", extra=(), user=REMOTE_USER):
    return ["ssh", "-F", "/dev/null", "-vv", "-E", str(work / f"{name}.debug.log"),
            "-p", str(port), "-i", str(work / key), "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
            "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ConnectionAttempts=1",
            "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={known}",
            "-o", "GlobalKnownHostsFile=/dev/null", "-o", "HostKeyAlias=goos-remote-test",
            "-o", "KexAlgorithms=curve25519-sha256", "-o", "Ciphers=aes128-gcm@openssh.com",
            "-o", "HostKeyAlgorithms=ssh-ed25519", "-o", "PubkeyAcceptedAlgorithms=ssh-ed25519",
            "-o", "SendEnv=-*", *extra, f"{user}@127.0.0.1"]


def ssh_exec(port, name, command, key="login", input_data=None, expected=0, sessions=0):
    result = run([*ssh_args(port, name, key, extra=("-T",)), command], 30, input_data=input_data)
    (work / f"{name}.stdout.log").write_bytes(result.stdout)
    (work / f"{name}.stderr.log").write_bytes(result.stderr)
    require(name + ".status", result.returncode == expected,
            f"native exit status {expected}, received {result.returncode}; stderr={result.stderr!r}")
    require(name + ".no-terminal", b"goos> " not in result.stdout and b"\r" not in result.stdout,
            "one-shot command has no prompt or PTY newline conversion")
    counts(sessions=sessions)
    return result


def log_records(data):
    return re.findall(rb"^\[[^\r\n]+Z\] (DEBUG|INFO |WARN |ERROR) ([^\[\r\n]+)\[[0-9]+\]: ([^\r\n]*)\r?$",
                      data, re.MULTILINE)


class Terminal:
    def __init__(self, port, name, extra=()):
        self.name, self.rows, self.columns = name, 24, 80
        self.stdout = bytearray()
        self.path = work / f"{name}.stdout.log"
        self.output = self.path.open("wb", buffering=0)
        self.error_path = work / f"{name}.stderr.log"
        self.error = self.error_path.open("wb", buffering=0)
        self.master, slave = pty.openpty()
        tty.setraw(slave)
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", self.rows, self.columns, 0, 0))
        self.process = subprocess.Popen(ssh_args(port, name, extra=("-tt", *extra)), stdin=slave,
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


def rejected_login(name, port, key="wrong", extra=(), user=REMOTE_USER):
    result = run(ssh_args(port, name, key, extra, user=user), 15)
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
    invalid = status(listener="closed", settings="invalid")
    require("bootstrap.invalid-status", invalid.get("reason") is not None,
            "invalid startup retains running admin service and rejection reason")
    require("bootstrap.invalid-no-accepts", not scan(ports[0]) and not scan(ports[1]),
            "invalid startup exposes no SSH host key")
    config("port", "22")
    rejected = console("remote reload")
    require("bootstrap.corrupt-host-key-refused", b"exit status: 2" in rejected,
            "repairing policy does not overwrite corrupt host identity")
    status(listener="closed", settings="invalid")
    require("bootstrap.corrupt-no-accepts", not scan(ports[0]), "corrupt identity keeps listener closed")
    config("host-key", RECOVERED_HOST_KEY_PATH)
    reload_ok()
    initial = status(enabled="true", listener="listening", settings="configured", user=REMOTE_USER,
                     sessions=0, source="system.remote", **{"key-count": 1, "authorized-keys": AUTHORIZED_KEYS_PATH})
    counts()  # A prior key scan can reach the newly opened listener after reload.
    identity = initial.get("fingerprint", "")
    require("bootstrap.recovered-identity", identity.startswith("SHA256:"),
            "explicit alternate identity recovers without restarting service")
    config("host-key", CORRUPT_HOST_KEY_PATH)
    retained = console("remote reload")
    require("bootstrap.corrupt-host-key-retained", b"exit status: 2" in retained and
            status(listener="listening").get("fingerprint") == identity,
            "corrupt original identity still rejects reload; applied identity remains active")
    config("host-key", RECOVERED_HOST_KEY_PATH)
    config("max-sessions", "4")
    config("handshake-timeout-ms", "60000")
    config("enabled", "true")
    reload_ok()
    status(enabled="true", listener="listening", bind="0.0.0.0:22")
    pin_identity(ports[0], identity, initial=True)
    rejected_login("auth.wrong-key", ports[0])
    rejected_login("auth.password-unavailable", ports[0], "login",
                   ("-o", "PreferredAuthentications=password", "-o", "PubkeyAuthentication=no", "-o", "NumberOfPasswordPrompts=0"))

    rejected_login("auth.wrong-user", ports[0], "login", user="user")

    result = ssh_exec(ports[0], "exec.uname", "uname")
    require("exec.uname-output", b"GoOS" in result.stdout, "native uname runs without a PTY")
    result = ssh_exec(ports[0], "exec.sequence", "echo one; echo two")
    require("exec.sequence-output", result.stdout == b"one\ntwo\n", "native sequence stdout is byte-exact")
    result = ssh_exec(ports[0], "exec.stderr", "cat /home/user/remote-missing-file", expected=2)
    require("exec.stderr-separated", not result.stdout and b"no such file or directory" in result.stderr,
            "native error reaches SSH extended stderr with command status")
    result = ssh_exec(ports[0], "exec.config", "config get system.net.mode")
    require("exec.config-output", result.stdout.strip() == b"dhcp", "builtin config keeps its normal authority")
    result = ssh_exec(ports[0], "exec.pipeline", "echo PIPE_OK | grep PIPE_OK")
    require("exec.pipeline-output", result.stdout == b"PIPE_OK\n", "native pipeline executes without terminal grants")
    result = ssh_exec(ports[0], "exec.stdin", "grep needle", input_data=b"other\nneedle\nlast\n")
    require("exec.stdin-output", result.stdout == b"needle\n", "SSH stdin and EOF reach the native stream reader")
    result = ssh_exec(ports[0], "exec.binary-stdin", "wc -c", input_data=b"\x00\xffA\r\nB")
    require("exec.binary-stdin-count", result.stdout.strip() == b"6", "non-PTY input preserves binary bytes")
    ssh_exec(ports[0], "exec.syntax", "echo 'unterminated", expected=2)
    result = ssh_exec(ports[0], "exec.background", "echo MUST_NOT_RUN; sleep 1 &", expected=2)
    require("exec.background-no-side-effects", not result.stdout,
            "whole one-shot command rejects background syntax before executing its sequence")
    result = ssh_exec(ports[0], "exec.literal-ampersand", "echo 'a&b'")
    require("exec.literal-ampersand-output", result.stdout == b"a&b\n", "quoted ampersand is argument data")

    for command, wanted in (("head -n 1 /home/user/admin-lines.txt", b"one\n"),
                            ("tail -n 1 /home/user/admin-lines.txt", b"three\n"),
                            ("grep -i TWO /home/user/admin-lines.txt", b"two\n"),
                            ("wc -l /home/user/admin-lines.txt", b"3")):
        name = command.split()[0]
        result = ssh_exec(ports[0], "admin." + name, command)
        require("admin." + name + ".output", result.stdout.strip() == wanted.strip(), "bounded native file command")
    result = ssh_exec(ports[0], "admin.find", "find /home/user -name admin-lines.txt")
    require("admin.find-output", b"/home/user/admin-lines.txt\n" in result.stdout, "bounded traversal matches exact basename")
    result = ssh_exec(ports[0], "admin.hexdump", "hexdump /home/user/admin-bytes.bin")
    require("admin.hexdump-output", b"00 01 41 42 43 ff" in result.stdout and b"ABC" in result.stdout,
            "fixed binary hex and printable ASCII rows")
    ssh_exec(ports[0], "admin.hostname-set", "hostname remote-test")
    result = ssh_exec(ports[0], "admin.hostname-get", "hostname")
    require("admin.hostname-output", result.stdout.strip() == b"remote-test", "typed hostname durable mutation")
    result = ssh_exec(ports[0], "admin.date", "date")
    require("admin.date-output", re.fullmatch(rb"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?Z\n", result.stdout) is not None,
            "realtime UTC has an unambiguous format")

    second_public = (work / "second.pub").read_text().strip()
    fingerprint_result = run(["ssh-keygen", "-l", "-E", "sha256", "-f", work / "second.pub"])
    require("keys.fixture-fingerprint", fingerprint_result.returncode == 0, "standard Ed25519 public fingerprint")
    second_fingerprint = fingerprint_result.stdout.split()[1].decode()
    ssh_exec(ports[0], "keys.add", f'remote key add "{second_public}"')
    result = ssh_exec(ports[0], "keys.list", "remote keys")
    require("keys.list-added", second_fingerprint.encode() in result.stdout and b"goos-second-test" in result.stdout,
            "stable fingerprint and preserved optional comment")
    result = ssh_exec(ports[0], "keys.authenticate-added", "uname", key="second")
    require("keys.add-effective", b"GoOS" in result.stdout, "successful add reloads policy without reboot")
    ssh_exec(ports[0], "keys.duplicate", f'remote key add "{second_public}"', expected=2)
    before_keys = status()
    ssh_exec(ports[0], "keys.bad-add", 'remote key add "ssh-ed25519 invalid"', expected=2)
    require("keys.bad-add-retains-policy", status().get("generation") == before_keys.get("generation"),
            "bad key leaves previous effective generation active")
    ssh_exec(ports[0], "keys.remove", f"remote key remove {second_fingerprint}")
    rejected_login("keys.removed-authentication", ports[0], key="second")
    config("port", "0")
    before_keys = status()
    ssh_exec(ports[0], "keys.failed-reload", f'remote key add "{second_public}"', expected=2)
    require("keys.failed-reload-retains-policy", status().get("generation") == before_keys.get("generation"),
            "rejected key reload preserves the previous active set")
    config("port", "22")
    reload_ok()
    result = ssh_exec(ports[0], "keys.failed-reload-rollback", "remote keys")
    require("keys.failed-reload-retains-file", second_fingerprint.encode() not in result.stdout,
            "failed reload does not leave the candidate authorized on disk")
    rejected_login("keys.failed-reload-authentication", ports[0], key="second")

    result = ssh_exec(ports[0], "log.newest", "log -n 5")
    require("log.newest-count", len(log_records(result.stdout)) == 5, "newest five retained records")
    result = ssh_exec(ports[0], "log.level", "log -n 20 --level warn")
    records = log_records(result.stdout)
    require("log.level-filter", bool(records) and all(level == b"WARN " for level, _, _ in records),
            "exact structured warn filtering")
    result = ssh_exec(ports[0], "log.combined", "log -n 2 --level info --component remoted")
    records = log_records(result.stdout)
    require("log.combined-filter", len(records) == 2 and all(level == b"INFO " and component == b"remoted"
            for level, component, _ in records), "newest-N after exact level and component filtering")
    result = ssh_exec(ports[0], "log.newest-order",
                      "log --level info --component remoted; echo LOG_SPLIT; log -n 2 --level info --component remoted")
    parts = result.stdout.split(b"LOG_SPLIT\n")
    require("log.snapshot-separator", len(parts) == 2, "two filtered reads within one SSH execution")
    all_records, newest_records = map(log_records, parts)
    require("log.newest-identity-and-order", len(all_records) >= 2 and newest_records == all_records[-2:],
            "newest matching records retain their chronological identity and order without intervening admissions")

    session = Terminal(ports[0], "primary-shell")
    session.wait(PROMPT)
    require("auth.good-key", session.process.poll() is None, "OpenSSH public-key login opens native shell")
    require("terminal.prompt-column-zero", b"\r\ngoos> " in session.stdout,
            "cooked prompt starts at column zero after CRLF")
    debug = (work / "primary-shell.debug.log").read_bytes()
    for name, pattern in (("kex", b"kex: algorithm: curve25519-sha256"),
                          ("host-key", b"kex: host key algorithm: ssh-ed25519"),
                          ("cipher", b"cipher: aes128-gcm@openssh.com")):
        require(f"negotiation.{name}", pattern in debug, "OpenSSH debug records selected minimal algorithm")
    body = session.command("echo REMOTE_STDOUT_OK")
    require("shell.stdout", re.search(rb"(?:^|\n)REMOTE_STDOUT_OK\r?\n", body) is not None,
            "native command output, not command echo")
    require("terminal.stdout-column-zero", b"\r\nREMOTE_STDOUT_OK\r\n" in body and b"\r\ngoos> " in body,
            "cooked command output and following prompt start at column zero")
    error = session.command("cat /home/user/remote-missing-file")
    require("shell.stderr", re.search(rb"(?:^|\n)cat: /home/user/remote-missing-file: no such file or directory\r?\n", error) is not None,
            "native stderr reaches the shared PTY stream, like local terminal jobs")
    require("terminal.stderr-column-zero", b"\r\ncat: /home/user/remote-missing-file: no such file or directory\r\n" in error,
            "cooked diagnostics start at column zero")
    log_output = session.command("log")
    records = list(re.finditer(rb"\[20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z\]", log_output))
    require("terminal.log-column-zero", len(records) > 0 and all(
            record.start() >= 2 and log_output[record.start()-2:record.start()] == b"\r\n"
            for record in records), "system log records start at column zero")
    start = len(session.stdout)
    session.send(b"log -n 0 -f --component remoted\n")
    session.wait(rb"log -n 0 -f --component remoted", start)
    time.sleep(0.5)
    session.pump()
    idle_start = len(session.stdout)
    time.sleep(0.5)
    session.pump()
    require("log.follow-idle", len(session.stdout) == idle_start and session.process.poll() is None,
            "follower stays quiet with no new matching record")
    trigger_time = time.monotonic()
    ssh_exec(ports[0], "log.follow-trigger", "remote reload", sessions=1)
    session.wait(rb"remoted\[[0-9]+\]: remote listener active", idle_start, 10)
    require("log.follow-wake", time.monotonic() - trigger_time < 10,
            "second SSH session wakes the event-driven follower")
    session.pump()
    second_event_start = len(session.stdout)
    ssh_exec(ports[0], "log.follow-second-trigger", "remote reload", sessions=1)
    session.wait(rb"remoted\[[0-9]+\]: remote listener active", second_event_start, 10)
    session.pump()
    require("log.follow-continues", re.search(PROMPT, session.stdout[start:]) is None,
            "follower stays active through a second append instead of returning after its first event")
    cancel_start = len(session.stdout)
    session.send(b"\x03")
    session.wait(PROMPT, cancel_start)
    require("log.follow-interrupt", b"FOLLOW_STOPPED_OK" in session.command("echo FOLLOW_STOPPED_OK"),
            "Ctrl-C stops the builtin and restores the remote prompt")
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
    viewer_pids = {pid for pid, name in inventory().items() if name.rsplit("/", 1)[-1] == "gview"}
    require("terminal.raw-viewer-process", len(viewer_pids) == 1, "one running foreground viewer")
    start = len(session.stdout)
    session.resize(120, 40)
    session.wait(rb"\x1b\[40;1H", start)
    session.wait(rb"(?<!G)G{120}(?!G)", start)
    require("terminal.raw-live-window-change", viewer_pids <= inventory().keys(),
            "same running viewer redraws at 120x40 without input or restart")
    start = len(session.stdout)
    session.resize(100, 30)
    session.wait(rb"\x1b\[30;1H", start)
    session.wait(rb"(?<!G)G{100}(?!G)", start)
    require("terminal.raw-newest-window-change", viewer_pids <= inventory().keys(),
            "same viewer applies the newest sequence at 100x30")
    session.send(b"\x1b[21~")  # F10 quits the raw application.
    session.wait(PROMPT, start)
    require("terminal.raw-exit", b"RAW_EXIT_OK" in session.command("echo RAW_EXIT_OK"),
            "raw viewer returned control to its own shell")
    start = len(session.stdout)
    session.send(b"\x04")
    wait_until(lambda: session.process.poll() is not None, "Ctrl-D disconnects shell")
    counts()
    require("terminal.ctrl-d-cleanup", session.process.returncode == 0, "EOF disconnect cleanly exited and reaped authenticated session")

    local_start = serial_log.stat().st_size
    serial.write(b"log -n 0 -f --component remoted\n")
    serial.flush()
    time.sleep(0.5)
    local_idle = serial_log.stat().st_size
    time.sleep(0.5)
    require("log.local-follow-idle", serial_log.stat().st_size == local_idle,
            "local follower emits nothing while idle")
    result = run([*ssh_args(ports[0], "log.local-trigger", extra=("-T",)), "remote reload"], 30)
    (work / "log.local-trigger.stderr.log").write_bytes(result.stderr)
    require("log.local-trigger", result.returncode == 0, "second SSH session appends a structured remoted event")
    wait_until(lambda: re.search(rb"remoted\[[0-9]+\]: remote listener active", serial_log.read_bytes()[local_idle:]),
               "local follower wakes for SSH-generated event", 10)
    second_event_start = serial_log.stat().st_size
    result = run([*ssh_args(ports[0], "log.local-second-trigger", extra=("-T",)), "remote reload"], 30)
    (work / "log.local-second-trigger.stderr.log").write_bytes(result.stderr)
    require("log.local-second-trigger", result.returncode == 0, "second append for the same local follower")
    wait_until(lambda: re.search(rb"remoted\[[0-9]+\]: remote listener active", serial_log.read_bytes()[second_event_start:]),
               "local follower stays active through its second event", 10)
    require("log.local-follow-continues", re.search(PROMPT, serial_log.read_bytes()[local_start:]) is None,
            "local follower has not returned before cancellation")
    local_cancel_start = serial_log.stat().st_size
    serial.write(b"\x03")
    serial.flush()
    wait_until(lambda: re.search(PROMPT, serial_log.read_bytes()[local_cancel_start:]), "local follow Ctrl-C returns prompt", 10)
    require("log.local-follow-interrupt", b"LOCAL_FOLLOW_STOPPED_OK" in console("echo LOCAL_FOLLOW_STOPPED_OK"),
            "local shell stays usable after cancelling follow")
    counts()

    rekey = Terminal(ports[0], "reload-rekey", extra=("-o", "RekeyLimit=1K"))
    rekey.wait(PROMPT)
    rekey_pid = rekey.process.pid
    config("idle-timeout-sec", "60")
    reload_ok()
    rekey_debug = work / "reload-rekey.debug.log"
    rekey_offset = rekey_debug.stat().st_size
    body = rekey.command(f"cat {REKEY_OUTPUT_PATH}")
    require("reload.rekey-controlled-output", re.search(rb"(?:^|\n)R{2048}\r?\n", body) is not None,
            "bounded post-reload output crosses client rekey threshold")
    wait_until(lambda: b"SSH2_MSG_NEWKEYS received" in rekey_debug.read_bytes()[rekey_offset:],
               "OpenSSH completes post-reload rekey", 30)
    require("reload.rekey-openssh", True, "client records completed post-reload key exchange")
    require("reload.rekey-same-connection", rekey.process.pid == rekey_pid and rekey.process.poll() is None and
            b"REKEY_AFTER_RELOAD_OK" in rekey.command("echo REKEY_AFTER_RELOAD_OK"),
            "fresh command completes after rekey on the original SSH connection")
    rekey.send(b"\x04")
    wait_until(lambda: rekey.process.poll() is not None, "rekey shell EOF disconnect")
    counts()

    for name in ("exec-malformed", "exec-oversized", "pty-exec", "second-exec", "exec-shell", "exec-pty",
                 "subsystem", "env", "auth-agent-req@openssh.com", "x11-req", "second-session", "direct-tcpip", "tcpip-forward"):
        result = run([work / "requests", f"127.0.0.1:{ports[0]}", work / "login", known, "goos-remote-test", REMOTE_USER, name], 20)
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

    victim = Terminal(ports[0], "signer-fault-victim")
    victim.wait(PROMPT)
    survivor = Terminal(ports[0], "signer-fault-survivor", extra=("-o", "RekeyLimit=1K"))
    survivor.wait(PROMPT)
    counts(sessions=2)
    fault = console("fault remote-signer", rb"REMOTE_SIGNER_FAULT_INJECTED")
    require("signer-fault.injected", b"fault: refused" not in fault,
            "PID 1 delivered a signer-only fault to the first authenticated worker")
    wait_until(lambda: victim.process.poll() is not None, "signer fault closes the selected SSH connection")
    require("signer-fault.victim-disconnected", victim.process.returncode != 0,
            "selected SSH connection closed after its signer endpoint died")
    counts(sessions=1)
    debug = work / "signer-fault-survivor.debug.log"
    offset = debug.stat().st_size
    body = survivor.command(f"cat {REKEY_OUTPUT_PATH}")
    require("signer-fault.survivor-output", re.search(rb"(?:^|\n)R{2048}\r?\n", body) is not None,
            "second SSH connection remains usable after the fault")
    wait_until(lambda: b"SSH2_MSG_NEWKEYS received" in debug.read_bytes()[offset:],
               "surviving SSH connection completes rekey after signer fault")
    require("signer-fault.survivor-rekey", survivor.process.poll() is None and
            b"SURVIVOR_OK" in survivor.command("echo SURVIVOR_OK"),
            "surviving worker retains its host identity reference")
    admitted = Terminal(ports[0], "signer-fault-replacement")
    admitted.wait(PROMPT)
    counts(sessions=2)
    require("signer-fault.capacity-reclaimed", b"ADMITTED_OK" in admitted.command("echo ADMITTED_OK"),
            "closed worker releases its admission slot")
    survivor.kill()
    admitted.kill()
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
    serial_output = console("net status")
    require("serial.column-zero", re.search(rb"(?<!\r)\n", serial_output) is None,
            "COM1 writes CRLF for every output line")
    session = Terminal(ports[1], "polish-log")
    session.wait(PROMPT)
    log_output = session.command("log")
    require("terminal.log-lines", b"remote connection admitted worker=" in log_output,
            "SSH log keeps each record intact across stdout reads")
    session.send(b"exit\n")
    wait_until(lambda: session.process.poll() is not None, "exit closes remote shell", 15)
    session.pump()
    require("terminal.exit", session.process.returncode == 0,
            "exit sends successful SSH session termination")
    counts()
    session = Terminal(ports[1], "polish-poweroff")
    session.wait(PROMPT)
    session.send(b"poweroff\n")
    wait_until(lambda: session.process.poll() is not None, "poweroff closes SSH", 15)
    session.pump()
    require("terminal.poweroff-close", session.process.returncode == 0 and
            b"Operation timed out" not in session.error_path.read_bytes() and
            b"Broken pipe" not in session.error_path.read_bytes(),
            "requesting OpenSSH session closes before machine power-off")
    code = guest.wait(timeout=30)
    serial.close()
    guest, serial = None, None
    require("boot1.poweroff", code == 0, "guest powered off after SSH close")
    if os.environ.get("GOOS_REMOTE_SINGLE_BOOT") == "1":
        require("boot-budget", boot_number == 1, "one guest boot")
        print(f"remote: {checks} assertions passed; one boot\nremote report: {work}")
        sys.exit(0)

    boot()
    persisted = status(enabled="true", listener="listening", bind="0.0.0.0:2222", user=REMOTE_USER, sessions=0, pending=0, **{"authorized-keys": AUTHORIZED_KEYS_PATH})
    require("reboot.config-and-identity", persisted.get("fingerprint") == identity,
            "enabled state, alternate port and serial identity survive the same disk's reboot")
    stored = console("config get system.remote.max-sessions", rb"CONFIG_GET_OK")
    require("reboot.config-session-limit", re.search(rb"(?:^|\n)2\r?\n", stored) is not None,
            "configured session limit persisted in the configuration store")
    pin_identity(ports[1], identity)
    result = ssh_exec(ports[1], "reboot.hostname", "hostname")
    require("reboot.hostname-persisted", result.stdout.strip() == b"remote-test", "durable hostname survives disk reboot")
    result = ssh_exec(ports[1], "reboot.keys", "remote keys")
    require("reboot.key-removal-persisted", second_fingerprint.encode() not in result.stdout,
            "removed key remains absent after reboot")
    rejected_login("reboot.removed-key-refused", ports[1], key="second")
    rebooted = Terminal(ports[1], "reboot-shell")
    rebooted.wait(PROMPT)
    require("reboot.login", b"REBOOT_LOGIN_OK" in rebooted.command("echo REBOOT_LOGIN_OK"),
            "OpenSSH strict pinned-host-key login after reboot")
    start = len(rebooted.stdout)
    rebooted.command(f"cat {RECOVERED_HOST_KEY_PATH}")
    rebooted.wait(rb"exit status: [1-9][0-9]*\r?\n", start)
    body = bytes(rebooted.stdout[start:])
    require("login.private-key-denied", b"CAT_PROCESS_OK" not in body and
            re.search(rb"(?:^|\n)cat: " + re.escape(RECOVERED_HOST_KEY_PATH.encode()) + rb": permission denied\r?\n", body) is not None,
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
