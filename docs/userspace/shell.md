# Native shell

`fetch [OPTIONS] URL` transfers an HTTP/1.1 response. It accepts `http://` and
`https://` hostnames and IPv4 addresses, with an optional port, path and query.
The default ports are 80 and 443. `-I`
requests headers only; `-i` includes response headers; `-d DATA` sends a POST;
`-H 'Name: value'` adds a request header; `-L` follows up to five redirects.
`-o FILE` writes to a chosen file, and `-O` uses the final URL's filename.
`-v` writes request and response diagnostics to standard error. HTTP status
codes, including 404 and 500, remain response data. A failed transfer can leave
a partial output file.

HTTPS uses native TLS 1.3 and verifies server certificates against
signed persistent roots or the immutable `/system/security/ca-certificates.pem`
fallback, including validity against REALTIME and
DNS/IP SAN identity. Missing or malformed roots, unavailable time or entropy,
and authentication failures return exit status 1. There is no `-k` or CA-file
option. `-L` follows HTTP-to-HTTP/HTTPS and HTTPS-to-HTTPS redirects, but rejects
HTTPS-to-HTTP before connecting. Cross-origin redirects strip Authorization,
Proxy-Authorization, Cookie and a custom Host header. Numeric IPv4 URLs require
IP SAN and send no SNI. The request/body/file-output options above apply to both
schemes.

```text
goos> fetch https://example.org/
goos> fetch -I https://example.org/
goos> fetch -L -o /home/user/page.html https://example.org/start
```

These are syntax examples, not Internet-dependent release tests. The offline
native HTTPS gate passed 69 assertions, including authenticated transfers and
certificate/record rejection. The complete `release-check` passed; see
[ADR 0198](../adr/0198-native-tls-and-system-trust.md).

PID 1 launches the shell as a normal supervised native image after the
terminal service: `/system/services/shell` from the installed system in
normal mode, `/bootstrap/shell` from the boot archive in recovery (ADR 0175). The shell holds no console capability (ADR 0089): what is
typed reaches it as lines from the terminal service over its `tty` session
(ADR 0082), and what it prints goes back through the same service on `stdout`,
a write-only copy of the terminal's output stream. It assembles its output into
lines and sends each as one write, flushing before it asks for a line or waits.
Reading the system log is a separate `log` capability.

The shell resolves its `stdout`, `tty`, `log`, `vfs`, and `exec` handles from
the versioned startup object and asks the terminal service for each line with
its prompt. Editing, echo and the control keys belong to the terminal service
([tty.md](tty.md)); a command line is at most 4096 bytes, and the number of
words in it is bounded only by its length (ADR 0109). Runtime allocation
remains confined to the fixed userspace arena.

The 16 shell built-ins are `help`, `clear`, `cd`, `services`, `handles`,
`caps`, `config`, `hosts`, `log`, `jobs`, `kill`, `reboot`, `poweroff`, `exit`,
`diag`, and `time`. They are a static shell table. `help` prints that table and
the currently installed command index; `help NAME` first recognizes a builtin
and otherwise looks up the executable's build-time manifest. `NAME --help`
prints the same synopsis for either kind. The dynamic executable index is
`/system/commands` in normal mode and `/bootstrap/commands` in recovery, so it
tracks the commands that the selected system tree actually contains rather than
a second handwritten shell list. A command given the wrong number of arguments
prints `usage: ...` instead of running (ADR 0079).

`services`, `reboot`, and `poweroff` are requests to PID 1 over the control
protocol on the exec channel. `handles` and `caps` list the shell's own handle
table, raw and under its startup names. `config` and `hosts` use the shell's
configuration session: `hosts` lists the hosts table, and `hosts get NAME`,
`hosts set IPV4 NAME` and `hosts remove NAME` read and change one record
([configuration](configuration.md#hosts)). `diag`, in a `goos_diag` build, asks PID 1 to
have the kernel dump its state to the console, since the shell cannot write the
console itself. `clear` emits ANSI terminal control. `time` prefixes a whole command line, runs it through
the normal path, and prints the elapsed time from the kernel's nanosecond
clock as `time: <N> us` (ADR 0054). Commands are split into words bounded only by the line's 4096 bytes, with single
quotes, double quotes, and backslash escaping (ADR 0109). The shell owns a
fixed working-directory buffer of the native path limit, 4096 bytes, and
resolves
relative paths without heap allocation. `cd` commits a new directory only after
VFS `Stat` confirms it. A bare command name is looked up in each directory of
the shell's search path in turn, and the first regular file found is run: the
`PATH` PID 1 starts the shell with, `/system/bin:/home/user/bin` in normal
mode and `/bootstrap/bin` in recovery (ADR 0175). Only absolute entries count;
the working directory is never searched unless a command names it. A command
word with a slash is a program path, absolute or relative to the working
directory, such as `/home/demo/args` or `./args` (ADR 0097). The shell sends
the executable path, structured bounded argument and environment vectors, and
current directory to PID 1 and waits through a process handle. PID 1
independently validates the path: under `/bootstrap` it looks in the boot
archive, elsewhere it reads the file into an executable object, and the
kernel validates the executable image either way. A command in
`/home/user/bin` runs with its standard streams only, whatever its name.
The shell similarly restores its post-startup arena mark after every completed
command, so repeated IPC commands cannot exhaust the non-freeing TinyGo heap.

The QEMU boot test writes real commands to serial stdin and requires the shell's
responses in serial output. The shell receives non-transferable VFS and exec
endpoints and cannot delegate them. Every external command whose manifest asks
for filesystem access receives a fresh VFS session instead of sharing the shell
reply queue. Fast exit notifications that
arrive before a launch reply are retained in a bounded PID 1 queue. The exec
reply distinguishes launch status from the final child exit code. The native
base utilities are separate images under `userspace/bin`, each with a manifest
the build collects into `/system/commands` (ADR 0080). They receive structured
`argv`, the shell's `PATH`, the framebuffer console's size as `COLUMNS` and
`LINES` when there is one, a VFS
session when their manifest asks for one, a
read-only standard-input handle named `in`, and distinct write-only
`stdout` and `stderr` handles. Normal output uses `stdout`; diagnostics and
validation markers use `stderr`. Both are write-only copies of the terminal's
output stream, which the terminal service relays to the console, unless pipeline
or redirection composition substitutes an endpoint (ADR 0089). No command holds a
console capability. Standard input is always a
stream: a foreground command's is fed by the shell with the lines typed while it
runs, and a background command's is at end of input. The bounded `readline`
utility reads one line from `in`, rejects input beyond 4096 bytes, and writes
accepted bytes to `stdout`.
`stat [-b|-k|-m|-g] PATH` prints a path's type and size in KiB by default, or
in bytes, MiB or GiB, with two decimals and the exact byte count:
`file 2.46 KiB (2519 bytes)`. A directory's size is the total of the files
under it at any depth, followed by how many files and directories it holds;
mounts inside it are counted like the directories they appear as. The walk
keeps one cursor per level in fixed arrays, so its memory does not grow with
the tree, and costs one VFS request per entry.
Up to three unquoted `|` operators split a command into at most four bounded
invocations. PID 1 creates private streams, starts all children before waiting,
and gives each stage only its adjacent directional endpoints. It returns the
first failing exit code in left-to-right order. Quoted and escaped `|` bytes
remain ordinary argument data. One unquoted `>` or `>>` redirects stdout to a
single bounded VFS path. The shell adds an isolated `write` consumer to the
pipeline; it receives only the upstream reader endpoint and a fresh VFS session,
then truncates the target for `>` or seeks to its current size for `>>` before
draining the stream and exiting on EOF. This reuses typed
handles and does not add POSIX file descriptors. A redirected pipeline may
contain at most three user stages because the sink occupies the fourth bounded
exec slot. One unquoted `2>` or `2>>` target adds a second `write` consumer for the last
requested stage's diagnostics. It stays outside the stdout chain: the shell names
the diverted stage in the exec request, and PID 1 connects that stage's `stderr`
role to the sink through a private stream. The `2` is an operator only at the
start of a word, so arguments ending in `2` remain data. One unquoted `<` target prepends the existing `cat` utility as a file
producer with its own VFS session; its stdout stream becomes the first requested
stage's stdin. Input and output redirection may be combined while staying within
the four-stage bound. Quoted or escaped operator bytes remain argument data.
Stream merging such as `2>&1` and a persistent userspace TTY server remain
later work.

A line ending in an unquoted `&` runs as a background job with an empty
standard input. The shell prints `[N] started`, returns to the prompt, and
reports `[N] done`, `[N] exit status: C`, or the launch failure when PID 1
replies. The terminal service takes the line being typed off the screen, writes
the report, and shows the line again (ADR 0089). Up to seven background jobs may run (ADR 0111),
and one exec slot stays free for the foreground. Builtins and `;` sequences
cannot run in the background. A foreground command has no reply time limit
(ADR 0064).

`jobs` lists the running background jobs with their command lines, and
`kill %N` (or `kill N`) stops one; the job is then reported as `[N] killed`.
Ctrl-C stops the foreground command and prints `^C`, and at the prompt it
clears the line being typed (ADR 0067). A line typed while a command runs is
that command's input; Ctrl-D ends that input. At an idle prompt, `exit` and
Ctrl-D end only an SSH remote shell session. The local serial and framebuffer
shells refuse `exit` and ignore prompt EOF, so their supervisory shell remains
available. Typing ahead of a command that does not read keeps the text for the
next prompt (ADR 0082).

For `reboot` or `poweroff`, PID 1 synchronizes storage first. A local request
then performs the machine operation normally. For a request from an SSH shell,
PID 1 returns acceptance, the shell flushes its SSH-bound output and exits, and
the remote worker drains and closes the SSH transport before the machine action.
PID 1 allows that close a bounded three-second grace period; it proceeds when
the worker releases the connection or that period expires.

The local supervised shell is `restart: always` and restarts independently of
the terminal service (ADR 0089). When the terminal service dies, the shell keeps running —
its jobs, its working directory, its output, which waits in the terminal's
stream — and takes the new terminal's session from PID 1 as a notice on its exec
channel, then asks for its next line through it. When the shell itself dies, its
running jobs are stopped with it, and the new shell starts with a fresh
terminal session, exec channel, VFS session and configuration session, and no
jobs.

Up to eight commands or bounded pipelines may be separated by unquoted `;`
within the 4096-byte line limit. A trailing separator is accepted; empty
intermediate commands are rejected. The entire line is checked for balanced
quotes, escaping, argument bounds, and pipeline syntax before any command runs.
Quoted or escaped semicolons remain data. Builtins cannot be pipeline stages.
Each command completes before the next starts; a runtime failure is reported
and does not stop the sequence. `cd` accepts a quoted path and changes the cwd
inherited by subsequent commands. The shell resets its arena after each command.
This syntax does not implement conditional execution (`&&` or `||`).

Diagnostics name what failed. A utility reports `cmd: path: reason` on stderr,
with the reason taken from the VFS status — `no such file or directory`, `not a
directory`, `directory not empty`, `read-only filesystem`, `a mount depends on
it` — and exits with status 2. A command PID 1 cannot start is reported by the
name of the stage that failed, `NAME: command not found`, even inside a pipeline
(ADR 0079). `uptime` prints `up H:MM:SS` and `mem` prints total, used and free
memory in binary units.

## Log

`log [-f] [-n N] [--level debug|info|warn|error] [--component NAME]` reads the
64-record system ring. Options may be ordered freely but may occur only once.
`--level` and `--component` are exact structured-field filters, not text
searches; a component name is 1 through 16 ASCII letters, digits, hyphens,
underscores, or dots. `-n N` is a decimal count, clamped to the ring capacity.
Without `-n`, `log` selects all retained matching records. It selects the newest
matches from one snapshot, then emits those selected records in chronological
order.

```text
goos> log --component remoted --level warn -n 20
goos> log --component remoted -f
goos> log --component remoted
```

`log` is a builtin and cannot be a pipeline stage. Use an external command such
as `cat /state/system.log | grep remoted` when text-pipeline processing is what
you need. With `-f`, the initial snapshot follows the same selection rule and
subsequent matching records arrive from an independent log cursor. The shell blocks on the log cursor and
terminal input together; it does not poll. If the cursor falls behind the ring,
it reports the number of overwritten records and resumes at the oldest retained
record. Following requires an interactive terminal, and Ctrl-C stops it with
status 130.

## Native administration commands

These native executables are in the dynamic command index, so `help NAME` and
`NAME --help` expose the installed manifest. Except where stated otherwise,
usage, input, traversal, and I/O failures exit with status 2.

- `head [-n N] FILE` writes the first `N` logical lines; `N` is a decimal
  `uint64`, defaulting to 10. It scans at most 64 MiB. `head -n 3 /state/system.log`
  is a bounded file read; it has no follow mode.
- `tail [-n N] FILE` writes the last `N` logical lines, with the same decimal
  count, default, and 64 MiB limit **per pass**. It first counts lines and then
  rescans the file. A size or line-count change between passes is detected and
  reported, but same-size edits can appear in the output: this is not an atomic
  snapshot. `tail -n 20 /state/system.log` has no `-f` form.
- `grep [-i] PATTERN [FILE]` prints literal matching lines. Without `FILE` it
  reads standard input; `-i` folds ASCII case only. Patterns are at most 256
  bytes, retained lines at most 8192 bytes, and input at most 64 MiB. It exits
  0 on one or more matches, 1 on no match, and 2 on usage or input failure.
  `echo needle | grep match` is valid and returns 1; `cat /state/system.log |
  grep remoted` searches external file content.
- `wc [-l|-w|-c] [FILE]` reads `FILE` or standard input and reports LF line,
  ASCII-whitespace-delimited word, and byte counts in that order by default.
  One option chooses one count. Input is limited to 64 MiB. For example,
  `wc -l /state/system.log` prints just the LF-line count.
- `find PATH [-name NAME]` writes the starting path and descendants. `-name`
  is an exact basename match, not a glob; `NAME` must not contain slash or NUL.
  Traversal is limited to depth 16, 4096 nodes, 4096-byte paths, and 1024-byte
  names. `find /notes` walks that tree.
- `hexdump FILE` writes fixed GoOS rows of 16 bytes with offset, hexadecimal,
  and ASCII fields. It reads at most 64 MiB: `hexdump /state/system.log`.
- `hostname [NAME]` prints the durable typed `system.hostname` setting, or
  persists `NAME`. A name is an ASCII label of 1 through 63 letters, digits, or
  hyphens, with no leading or trailing hyphen; the default is `goos`. Run
  `hostname` to read it.
- `date [-u]` prints the realtime clock as a fixed UTC
  `YYYY-MM-DDTHH:MM:SSZ` timestamp. GoOS has no timezone database; `date -u`
  is the accepted explicit UTC form. Years outside 0000 through 9999 fail.

## Non-PTY SSH command execution

Set up keys, the listener, and host-key verification through
[Native SSH access](../development/remote-access.md); this section describes
execution only. Use `-T` to request a command without a PTY:

```sh
ssh -T -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS uname
ssh -T -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS 'net status'
ssh -T -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS 'echo one; echo two'
printf 'needle\nother\n' | ssh -T -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS 'grep needle'
```

The request uses the same native parser as an interactive shell, not `/bin/sh -c`.
Its 4096-byte command line, sequences, pipelines, redirections, quoting, and
escaping therefore have the same limits and meanings. Quoted literals are
allowed. An unquoted, unescaped background `&` rejects the complete request
before any command runs; a quoted or escaped `&` is data. A PTY request combined
with exec is also rejected.

SSH stdin, stdout, and stderr remain separate native streams. Input EOF closes
only stdin. GoOS drains stdout and stderr, sends the native exit status, and
then closes the SSH command channel. The one-shot command has no prompt,
banner, echo, or terminal newline conversion.

## Network commands

`ping`, `net`, `resolve`, `dnsq`, `udp` and `tcp` are native commands whose
manifests ask for sessions PID 1 opens for each launch: `ping` and `net` a
network session (ADR 0191), `ping` and `resolve` a resolver session
(ADR 0194), `udp` a UDP session (ADR 0195), `tcp` a TCP session (ADR 0196)
and `dnsq` one of each of the last two. None builds a packet: `netd` and
`resolved` do the work and answer.

`ping [-c COUNT] [-W TIMEOUT_MS] HOST` sends COUNT echo requests, 1 to 16 and
4 by default, one at a time and one second apart, each waiting up to
TIMEOUT_MS, 100 to 5000 and 1000 by default, for its reply. HOST is a
numeric address, used as it is, or a host name, which `ping` asks the
resolver for and announces as `PING NAME (ADDRESS)` before the first request;
of several addresses it takes the first. A numeric address works without the
resolver. Anything in `127.0.0.0/8`, `localhost` among the names, is answered
on lo at once and never reaches a device. Each request prints one line and a
summary follows, for example:

```text
goos> ping -c 2 10.0.2.2
40 bytes from 10.0.2.2: seq=1 time=0.412 ms
40 bytes from 10.0.2.2: seq=2 time=0.388 ms
2 sent, 2 received, 0% lost
```

A request that fails prints `seq=N: REASON`, for example `seq=3: timeout`,
`seq=1: neighbor unreachable` or `seq=1: interface down`; a router's ICMP
error prints `from A: seq=N destination unreachable` or `time exceeded`.
`ping` exits 0 when at least one reply came, 1 when none did, and 2 for a
usage error, a name that does not resolve, or when the network service is
unavailable.

`net status` prints two interfaces. `lo:` is always `up` at `127.0.0.1/8`
with MTU 65536 and the count of `echoes` answered on it. `net0:` gives
`state` (`down`, `configuring`, `ready` or `failed`), `link`, `mac`, `mode`,
`address` with its prefix or `none`, `gateway` or `none`, `mtu` while a link
is up, `lease` in seconds while a DHCP lease runs, the `frames` counters for
received, transmitted, dropped and malformed frames, `claims` (ARP probes and
announcements sent) and `conflicts` (conflicts while probing and on the live
address, defences, addresses given up, rate-limit waits). A `claim` line
says when the next address claim waits for the conflict rate limit, and a
`settings` line appears when the service runs on built-in defaults because
the stored settings were unreadable or invalid at startup. A `udp:` section
follows the interfaces: `endpoints` open against `max-endpoints`, with the
per-session limit; `datagrams` received and sent; `dropped` datagrams for no
endpoint, a full queue, a bad checksum and a payload too long;
`bind failures`; and the `limits` in force, the ephemeral port range, the
receive queue depth and the largest payload. The counters saturate. Each
field is indented under its section:

```text
goos> net status
lo:
  state: up
  address: 127.0.0.1/8
  mtu: 65536
  echoes: 0
net0:
  state: ready
  ...
udp:
  endpoints: 0/32 (8 per session)
  datagrams: rx 3 tx 3
  dropped: no-endpoint 1 queue-full 0 checksum 1 too-long 0
  bind failures: 0
  limits: ports 49152-65535 queue 8 payload 4096
```

`net reload` asks the service to apply the `system.net.*` settings; see
[configuration](configuration.md#network).

`resolve NAME` prints a name's IPv4 addresses and where they came from,
`localhost`, `hosts`, `dns` with the record's TTL, or `cache` with the time
left:

```text
goos> resolve localhost
localhost: 127.0.0.1 (localhost)
goos> resolve right.test
right.test: 192.0.2.11 (dns, ttl 60 s)
```

`resolve status` prints the upstream servers, each marked `dhcp` or
`config`, the cache's use against its limit and its positive, negative and
failure entries, hit and miss counts, upstream queries and timeouts, replies
dropped as malformed, mismatched or truncated, server errors, TCP fallbacks
started and answered, TCP failures and timeouts, and the generation of the
DHCP server list. `resolve flush` empties the cache, not the hosts table.
`resolve reload` applies the `system.dns.*` settings; see
[configuration](configuration.md#resolver). Only the exact words `status`,
`flush` and `reload` are subcommands: a host by one of those names is
`status.` and so on. A name under `.local` is refused, as GoOS has no
multicast DNS. An answer the server truncates over UDP is fetched again
over TCP from the same server.

`resolve` is a client of the system resolver. `dnsq` asks a DNS server
directly and shows its answer, so you can tell whether the server, UDP, TCP
or the fallback between them is at fault:

```text
dnsq @SERVER [OPTIONS] NAME [TYPE]
```

SERVER is a numeric IPv4 address and is required; `dnsq` never asks
`resolved` and never reads the DHCP server list. TYPE is `A` (the default),
`AAAA`, `CNAME`, `NS`, `MX`, `TXT`, `SOA` or `PTR`, in any case. NAME takes
any printable ASCII but the backslash, so `_sip._tcp.example` and
`in-addr.arpa` names work. Options go anywhere on the line:

- `-u`, `--udp` sends one datagram and never falls back to TCP;
- `-t`, `--tcp` skips UDP and uses one TCP connection;
- `--no-edns` leaves out the EDNS(0) record, which otherwise advertises
  1232 bytes with DO clear;
- `--timeout MS`, 1 to 60000 and 5000 by default, bounds the whole query,
  a TCP fallback included; closing the endpoint afterwards can add up to a
  second;
- `--short` prints the answer records' data only, one per line;
- `--raw` adds the header fields and counts and prints all four sections.

By default `dnsq` sends one datagram. A matching answer with TC=1 is not
shown: `dnsq` prints `;; UDP response truncated; retrying over TCP` (not with
`--short`) and repeats the same query, same ID and EDNS form, over TCP in
the time left. With `--udp` a truncated answer is an error.

```text
goos> dnsq @192.0.2.53 dnsq.test
;; server: 192.0.2.53:53
;; transport: UDP
;; id: 39174
;; status: NOERROR
;; flags: qr aa rd ra

;; question:
dnsq.test.    IN    A

;; answer:
dnsq.test.    60    IN    A    192.0.2.80

;; 1 answer, 54 bytes, 3 ms
goos> dnsq --short @192.0.2.53 mail.test MX
10 dnsq.test.
```

Names print with a trailing dot. Any byte outside printable ASCII in a name
or TXT string prints as `\DDD`, and a quote or backslash in TXT as `\"` or
`\\`, so a server cannot send the terminal control sequences. Record
types `dnsq` does not know print as `\# LENGTH HEX`, at most 64 bytes of it.
`dnsq` exits 0 for any complete, valid answer to the question asked,
NXDOMAIN, SERVFAIL, REFUSED and FORMERR included; 1 for a timeout, a
transport error, a truncated answer it may not repeat, or an answer that
does not match the query or breaks the protocol, which it prints nothing
of; and 2 for a usage error. A FORMERR without a question section, which
`resolved` takes as a refusal of EDNS(0), is a failure in `dnsq`: it does
not say which query it answers.
The error line names the step, for example
`dnsq: tcp connect: connection refused`.

`udp` sends and receives IPv4 UDP datagrams with nothing but its UDP
session:

- `udp send [-s SIZE] [-r] [-W MS] ADDRESS PORT [TEXT]` connects an endpoint
  to ADDRESS:PORT, which binds an ephemeral port, and sends TEXT, or SIZE
  bytes of a repeating `a` to `z` pattern; TEXT is required without `-s`.
  It prints `N bytes to ADDRESS:PORT from LOCAL:PORT`. With `-r` it then
  waits for one reply from that peer.
- `udp listen [-c COUNT] [-W MS] [-b ADDRESS] PORT...` binds one endpoint to
  each of up to four ports, on the wildcard or on ADDRESS, and prints COUNT
  datagrams, 1 to 64 and 1 by default, as they arrive on any of them.
- `udp self [-s SIZE] ADDRESS PORT [TEXT]` binds a listener at ADDRESS:PORT,
  sends to it from a second endpoint and prints what the listener got. To
  `127.0.0.0/8` or the interface's own address the datagram never leaves
  the machine, so this works without a NIC.

SIZE is 0 to 4096; the service applies `system.net.udp.max-payload` and,
off the host, the link MTU less 28 bytes. `-W` waits 100 to 60000 ms, 3000
by default, for each reply or datagram. A received datagram prints as
`N bytes from ADDRESS:PORT to LOCAL`, followed by `: ` and the payload when
it is printable text of at most 64 bytes:

```text
goos> udp self 127.0.0.1 9100 loop
4 bytes from 127.0.0.1:50211 to 127.0.0.1: loop
goos> udp send -r 192.0.2.53 7 echo me
7 bytes to 192.0.2.53:7 from 192.0.2.10:61022
7 bytes from 192.0.2.53:7 to 192.0.2.10: echo me
```

A refused operation prints `udp: OPERATION: REASON`, for example
`udp: send: invalid argument` for a payload over the limit or
`udp: bind: address in use`; a wait that runs out prints `udp: timeout`.
`udp` exits 0 on success and 2 on any failure or usage error.

`tcp` exercises the native GTCP1 byte-stream API:

- `tcp connect [-W MS] [-s SIZE] ADDRESS PORT [TEXT]` connects, writes the
  optional text or pattern, half-closes its write side, then reads through EOF;
- `tcp listen [-W MS] [-c COUNT] [-b ADDRESS] PORT` accepts COUNT connections,
  echoes each stream, and half-closes after its peer's EOF;
- `tcp self [-s SIZE] ADDRESS PORT [TEXT]` creates a listener and client in one
  process to check loopback or own-address delivery.

SIZE is 0 to 4096. `-W` is 100 to 60000 ms. `-c` is 1 to 8 and also sets the
listen backlog. Reads print `N bytes` and printable contents, then `eof` after
a FIN. Errors use `tcp: OPERATION: REASON`, including `connection refused`,
`reset`, `address in use` and `timeout`. The command receives only `tcp`,
`tcp-tx` and `tcp-rx`; it has no direct network stack or device authority.

The validation build's `fault net-driver` kills the network driver once;
PID 1 starts a new one with a new binding, and storage and the shell carry on.

## Linux programs

A path that names a static Linux x86_64 ELF runs like a native program: PID 1
recognizes the format, the kernel validates it, and the process starts with
the shell's working directory, its arguments and environment, the command's
standard streams, and a VFS session of its own. It can be a pipeline stage on
either side of a native program, a background job, and the target of Ctrl-C
and `kill`. Programs under `/system/linux` are installed with the system;
others run from any volume.

```text
goos> /system/linux/hello
hello from Linux ABI
goos> /system/linux/musl-hello | write /tmp/out
goos> /system/linux/dirprobe /persist
```

A Linux program has no terminal: its libc buffers output in blocks, so lines
appear when the buffer fills or the program exits. It may fork and run other
Linux programs; a static BusyBox `sh` runs command lines, pipelines, and
scripts that way. The shell waits for the program it started. Ctrl-C ends that
program and every process it forked; no signal reaches them (ADR 0170). [linux-x86_64-abi.md](../architecture/linux-x86_64-abi.md)
lists what such a program can call.

## Trust store command

```text
trust status
trust verify FILE META SIG
trust install FILE META SIG
trust update
```

Status names the selected root bundle, sequence, source date and fallback reason.
Verify authenticates the three supplied files without changing state. Install
requires a signed package, refuses rollback and commits a verified generation on
persistent FAT32. Equal-sequence content must match all three files exactly.
Update downloads the package from `/system/security/trust-update-url` through
current verified HTTPS roots, then checks its Ed25519 signature using the immutable
public key. An empty configured URL returns an error, not an insecure download.

After installation, new client processes use signed state roots. Missing or
corrupt state reports a reason and uses bootstrap roots; trust never accepts an
arbitrary local CA or disables certificate verification. Corrupt state stays on
disk for diagnosis. Trust sequence is independent of the GoOS version.
