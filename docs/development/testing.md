# Testing

GoOS keeps fast host validation separate from the complete QEMU gate.

## Targets

- `make crypto-check` builds the isolated native crypto feasibility probes.
- `make test-crypto` runs the successfully built probes in one disposable guest,
  including after an independent compile failure. `CRYPTO_COMPAT=1` selects the
  native runtime compatibility overlay. These development targets currently
  return failure for TinyGo's device-offloaded TLS shim; they do not constitute
  an HTTPS release gate. See [native crypto feasibility](crypto-feasibility.md).
- `make test-tls` is the offline native HTTPS gate selected for `test-all` and
  `release-check`. It builds with `VALIDATION=1` and the explicit fixture CA,
  then runs `tests/boot/tls.sh` against host standard-Go TLS peers. It passed
  69 native assertions and measured production-root loading. The gate covers
  default port 443, numeric IPv4 SAN, GET,
  HEAD, relative redirects, downgrade rejection and VFS file output, plus
  wrong identity, unknown roots, invalid validity periods/signatures, critical
  extensions, plaintext, malformed TLS, bad CertificateVerify/Finished and
  corrupted record MACs. Peers fragment I/O to 137 bytes;
  fixture keys are public test data and never production trust.
- `make test-unit` runs all host-side Go tests.
- `make test-integration` builds and validates the UEFI kernel artifact.
- `make test-security` runs the capability, handle, IPC, VM, filesystem, and
  userspace security-focused host tests explicitly, and those of the
  build-time stack frame check (`tools/stackframes`, ADR 0088).
- `make test` runs the three fast targets above and does not start QEMU.
- `make test-boot` builds the validation ISO (`VALIDATION=1`, ADR 0058) and
  runs the two-phase QEMU boot and reboot persistence test.
- `make test-system-boot` runs the installed-system and recovery proofs
  (ADR 0175); see [the system boot test](#system-boot-test).
- `make test-network` builds the validation ISO and system tree and runs
  `tests/boot/network.sh`, the network gate (ADR 0190 to ADR 0195). Each
  scenario boots from its own disposable disk with the NIC at PCI slot 4:
  - `user`: QEMU user networking. DHCP must reach ready on `10.0.2.x` within
    15 seconds of the prompt, and `net status` must show the state, link,
    MAC, mode, address, gateway `10.0.2.2`, lease and `mtu: 1500`, lo at
    `127.0.0.1/8`, and no `settings` fallback line; `resolve status` must
    list `10.0.2.3 (dhcp)` from DHCP option 6, and `ping -c 1 localhost`
    must get a reply from `127.0.0.1`. Four rounds of
    `ping -c 4 -W 1000 10.0.2.2` must each print a reply for every sequence
    number and `4 sent, 4 received, 0% lost`. Between rounds `fault
    net-driver` kills the driver twice; a file written before the first
    kill reads back after it, and a replacement driver must start each time.
    The per-process counts `quota` prints for PID 1, `netd` and the driver
    must match between the first and the second round of pings, and
    between the rounds that follow the first and the second driver
    replacement.
  - `stream`: two guests and `tools/netpeer` on a private segment, each
    guest a QEMU `stream` client of the peer's Unix socket. The peer is the
    hub, the DHCP server for the first guest (`192.0.2.10`) and the DNS
    server at `192.0.2.53` for a `test` zone; the second guest stores the
    static `192.0.2.11/24` and `system.dns.server1` `192.0.2.53`. Each guest
    pings the other four times and must get every reply. `resolve status`
    must show the server as `dhcp` on the first guest and `config` on the
    second. On the first guest a name must resolve from DNS, then from the
    cache with exactly one query in the peer's log, then from DNS again
    after `resolve flush`, with a second query; a CNAME must resolve, an
    NXDOMAIN must say `name not found`, `localhost` must resolve without any
    query, and `ping -c 2 right.test` must announce the address and get both
    replies. The second guest must resolve the first by DNS, add a hosts
    record and resolve and ping it by that name. The first guest then sets
    a TTL and an ARP attempt count and must see `net reload` apply them;
    `config set` must refuse a 20-digit TTL above the `uint64` maximum, and
    a second reload must refuse `max-retry-ms` below `initial-retry-ms` by
    that key; pings and the cached name must survive both reloads. Then
    the first guest runs `udp` against the peer: a datagram the peer sends
    a second after `udp listen` sent its receive must arrive with its
    source and destination, completing the receive `netd` held, `udp send -r` to the echo
    port 7 must print the reply, two ports must listen at once, a connected
    endpoint must ignore the peer's decoy from port 8, a burst of three must
    arrive in order, and `udp self` must round-trip over `127.0.0.1`,
    `127.0.0.2` and the guest's own address. Zero-length datagrams must pass
    both ways, a 1472-byte payload must echo, 1473 bytes must fail with
    `invalid argument`, and a datagram with a corrupt checksum must never
    arrive while the next good one does. `net status` must count the
    checksum drop and the datagram sent to an unbound port, and show the
    default limits. With EDNS refused, a name must resolve after exactly
    two queries, one with EDNS and one without; a name the peer answers a
    second late must resolve after exactly one query, its answer taken from
    the receive `resolved` left waiting in `netd`. A name the peer answers
    with TC=1 must resolve over TCP with one UDP query, one TCP connection,
    one framed query and one answer larger than a GTCP1 slot, then from the
    cache with no query at all; a name whose TCP answer the peer resets must
    give `temporary failure`, `resolve status` must count two fallbacks, one
    answer and one failure, and with `system.net.tcp.time-wait-ms` at 1000
    for these lookups the TCP endpoint line of `net status` must match the
    one before. On the second guest `dnsq` asks the peer directly: an A
    query over UDP must print `;; transport: UDP` and the record, AAAA, MX,
    TXT and SOA queries must print their data, a TXT byte 7 as `\007`, an
    NXDOMAIN under `--raw` must print the rcode and the SOA in the authority
    section and exit 0, and `--tcp` must answer over TCP without UDP. A name
    the peer truncates over UDP must print the fallback notice and a TCP
    answer of more than 4096 bytes, while `--udp` must fail with exit
    status 1 and no fallback. The peer's log must show one UDP and one TCP
    query for the plain name and two UDP queries and one TCP query for the
    truncated one. Every query the first guest sent
    must have left from its own port in the ephemeral range, and the
    40-second lease must have been renewed. Last on that guest,
    `system.net.udp.max-payload` set to 1300 must apply on `net reload`,
    refuse a 1301-byte `udp self` and pass a 1300-byte one, and a reload
    with `ephemeral-port-min` above `ephemeral-port-max` must be refused by
    the `ephemeral-port-max` key and keep the previous limits. Last,
    the peer claims the second guest's address: the guest must defend it
    once and stay ready, give it up and fail at a second claim within the
    defend interval, and fail again on a new address whose probe the peer
    answers, while `ping -c 1 127.0.0.1` still works.
  - `none`: no NIC, and a stored `system.net.ipv4.probe-count` of 0. The
    guest must reach normal mode and read `/system/motd`; `net status` must
    say `down`, list lo and print the `settings: built-in defaults` line,
    `net reload` must refuse the probe count by key, and `ping -c 1 -W 500`
    must print a failed sequence, `1 sent, 0 received, 100% lost`, and exit
    status 1. With no device, `ping -c 1 127.0.0.1` and `ping -c 1
    localhost` must get their replies, `resolve localhost` and `hosts get
    localhost` must give `127.0.0.1`, another name must fail with `no
    DNS servers`, and `udp self 127.0.0.2 9100` must round-trip over lo.

  `NETWORK_SCENARIOS` picks some of them, for example
  `make test-network NETWORK_SCENARIOS=stream`. Logs go to
  `build/logs/network-*.log`. The target needs no host privileges and is
  part of `test-all` and `release-check`. A ping beyond `10.0.2.2` under
  user networking depends on the host's ICMP support and is no test of
  GoOS. The host tests and fuzz seeds of `userspace/netdev`,
  `userspace/netapi` and `userspace/network` run in `make test-unit` and
  `make test-security`, as do those of `userspace/udpapi`,
  `userspace/udpserve`, `userspace/upstream`, `userspace/dns`,
  `userspace/dnswire`, `userspace/dnstcp`, `userspace/dnsq` and
  `userspace/tcpupstream`. The last two hold the
  GUDP1 logic of `netd` and `resolved`, which build only for the guest:
  deferred `RecvFrom`, `Close` and session teardown on the service side,
  and the resolver's queries over a real stack through the GUDP1 codec,
  with late, mismatched and stale answers. `userspace/tcpupstream` holds the
  resolver's TCP fallback: its tests split the length prefix and the reply
  across Reads in every way, take partial Writes, fail on early EOF, a bad
  prefix, `Reset`, `Timeout` and the other transport results, settle the
  slots of replies that arrive after a cancel, keep two fallbacks at most,
  and fuzz the read pieces. Three of them run the fallback over a real stack
  with `tcpserve` and `udpserve` on both ends: one answer split across the
  prefix and two RX slots, then a cache hit; one answer cut short; and 16
  fallbacks in a row, each leaving the resolver's side in TIME_WAIT, then 8
  more once the interval ends. `userspace/dnswire`, the one DNS parser, has
  tests for names, compression pointers and their loops, label and name
  limits, query encoding for every supported QTYPE, response matching and
  validation, EDNS fields, record data checks and the text of every type,
  TXT and name escapes included, and fuzzes the name decoder and the
  parser with the formatter. `userspace/dnstcp` tests the length prefix
  split every way, a body over many reads and over one slot, the
  65535-byte maximum, a short prefix, early EOF and partial writes.
  `userspace/dnsq` runs the command's logic against a scripted GUDP1 and
  GTCP1 peer: argument errors, every type, EDNS and `--no-edns`, error
  rcodes, mismatched and malformed answers, UDP fallback to TCP with a
  multi-slot answer, `--udp`, forced TCP with 3-byte writes and 1-byte
  reads, each transport failure, one deadline across both transports,
  `--short`, `--raw` and TXT escapes, and checks that every lent slot comes
  back and every endpoint closes. The `userspace/network` tests check that a
  recycled TIME_WAIT slot keeps its four-tuple as a tombstone: a connect
  limited to the old port is refused, a second port is taken instead,
  another remote port or address may use the old port, the old ID stays
  invalid, a retransmitted FIN is acknowledged while other segments get
  nothing, each owner keeps its own share, and expiry lifts the block. The network gate remains the
  test of the services themselves.

  The config exchange unit tests cover the timeout, late reply, then next
  request ordering with a deterministic clock and message queue. The network
  boot gate has no per-message configd delay injection, so it does not
  reproduce that exact ordering.

  `tools/netpeer` is a host-side peer for stream-backend scenarios. Guests
  connect to its Unix socket with
  `-netdev stream,server=off,addr.type=unix,addr.path=SOCKET`; it relays
  every frame between them like a hub and is a station of its own on the
  segment (default `52:54:00:00:02:53`, `192.0.2.53/24`). It answers ARP and
  ICMP echo, serves static DHCP leases by client MAC (`-lease MAC=IP`), and
  answers DNS for a zone under `test` read from `-zone`, where a name can
  also be set to answer NXDOMAIN, NODATA, SERVFAIL or a truncated reply, to
  go unanswered, or (`name TTL SLOW ADDRESS`) to answer a second late. A
  `name TTL TCP ADDRESS...` name answers TC=1 over UDP and in full over TCP
  port 53, padded with TXT records to about 6 KB, the length prefix in a
  segment of its own; a `name TCPRESET` name answers TC=1 too, then sends
  22 bytes of its TCP answer and a reset. The log gets `dns-tcp-accept`,
  `dns-tcp-query`, `dns-tcp-answer` and `dns-tcp-reset` lines; `-no-edns` makes it refuse EDNS queries with FORMERR,
  and `-lease-time` sets the lease length. It echoes UDP on port 7 and, for
  a payload that starts with `decoy`, first sends `decoy` from port 8.
  Lines written to the `-control` FIFO inject ARP address conflicts:
  `claim IP` announces the address from a foreign MAC, `defend-probe IP`
  answers guest probes for it until `clear`. They also send UDP to a guest
  whose frames the peer has seen: `udp-send ADDRESS PORT SOURCE_PORT TEXT`,
  `udp-bad` with the same operands and a corrupt checksum, `udp-fill` with
  a SIZE of up to 1472 bytes and `udp-burst` with a COUNT of numbered
  datagrams. `edns off` and `edns on` switch EDNS refusal, and `quit` stops
  the peer. Each DNS query, DHCP message, UDP echo or send and conflict
  action becomes one line in the `-log` file. The peer prints `netpeer: listening SOCKET` once guests can
  connect. Its unit tests run with `go test ./tools/netpeer`.
- `make test-all` is the canonical complete Definition-of-Done gate. It runs
  the fast tests, lint, documentation checks, the ISO build, the real QEMU
  boot/persistence test, and the Linux end-to-end boot in sequence so host
  builds cannot contend with QEMU.
- `make test-linux-elf` and `make test-linux-abi` run the Linux host tests:
  the ELF parser against hostile and real images with every fixture's
  checksum, and the pure translation code with the VFS protocol it speaks.
- `make test-linux-boot` builds the validation ISO and runs
  `tests/boot/linux.sh`: one boot in which PID 1's Linux probes run, then the
  shell runs the release proofs, a static PIE, the conformance probe's
  hardening modes, a stack overflow into the guard, two Linux processes at
  once, Ctrl-C on a blocked Linux process and on a Linux root with a forked
  child, and the process-composition modes: `exec`, `fork`, `vfork`,
  `orphan`, `wait`, `pipe`, `signal`, `kill`, `eintr`, `poll`, `ppoll`,
  `hardening`, `fpstate`, and `limits`. Last come two injected failures: a fork that
  fails mid-copy and an `execve` whose VFS dies (ADR 0173). Its guest CPU has RDRAND
  (`-cpu qemu64,+rdrand`), so `getrandom` must work there; the general boot
  test keeps the default CPU, where it must return `ENOSYS`.
  `test-linux-musl`, `test-linux-vfs`, `test-linux-security`,
  `test-linux-process`, `test-linux-exec`, `test-linux-fork`,
  `test-linux-pipe`, `test-linux-signals`, and `test-linux-poll` are the same
  run. `make test-linux-all` adds the two host targets. `make test-all`
  includes the boot.
- `make test-linux-reference` runs the conformance probe on a Linux kernel in
  an amd64 container and requires exit status zero. It needs Docker and is no
  part of `test-all`. On an arm64 host the container runs under user-mode
  emulation, which answers a few hostile calls itself; the probe is lenient
  there.
- `make test-linux-kernel-reference KERNEL=/path/to/vmlinuz
  BUSYBOX=/path/to/busybox` boots a real Linux kernel in QEMU with the probe
  in an initramfs and requires the default, hostile, and process modes to
  pass strict; `make test-linux-process-reference` is the same run:
  every check must give the exact Linux result. An Alpine `vmlinuz-virt`
  works as it comes. The repository carries neither file, so the target is
  no part of `test-all`.
- `make test-linux-busybox BUSYBOX=/path/to/busybox`, also
  `make test-linux-shell`, places a static x86_64 BusyBox in the validation
  image and requires `echo`, `pwd`, `cat`, `ls`, `mkdir`, and `rm`, and a
  non-interactive `sh` that runs programs, pipelines, redirections, a script,
  and a background job; it reports on further applets without requiring them.
  With `DIAG=1` it also prints the kernel's record of refused calls. The
  repository carries no BusyBox. See the
  [static application catalog](linux-application-compatibility.md) for the
  tested operations and limits.
- `make test-linux-bench` times four Linux calls beside native `bench` cases
  into `build/reports/linux-abi-<variant>.md`. `QEMU_ACCEL=kvm` on an x86_64
  host times the guest on the host CPU; the default is `tcg`.
- `make test-stress` boots `STRESS_RUNS` guests (default 4) in parallel
  batches of `STRESS_JOBS` and drives `STRESS_ROUNDS` rounds of commands
  through each; `make test-all` includes two runs.
- `make test-fat32` runs the FAT32 format tests (ADR 0102, ADR 0112): every
  logical sector size and cluster size with one and two FATs, BPB validation
  and backup-boot recovery, the FSInfo free count, FAT mirror comparison,
  maximum-length ASCII, Cyrillic, CJK, emoji and mixed Unicode names, complete
  simple case mapping and collisions, malformed UTF-16, primary/backup FSInfo
  fallback and reconciliation, timestamps and attributes, 1, 16 and
  64 MiB files, fragmentation, a directory of long names, a corruption corpus
  and seeded random corruption, and a model-based workload with device-failure
  injection. Where the host has FAT tools it also formats volumes with them
  (`newfs_msdos` or `mkfs.fat`) and mounts, fills and hands them back to the
  host checker (`fsck_msdos` or `fsck.fat`); on macOS the host's own FAT driver
  writes files this code reads and reads back files this code writes. Two
  host-made volumes in `userspace/fat32/testdata` cover foreign volumes on
  every host; `GOOS_FAT32_FIXTURES=1 go test -run TestMakeForeignFixtures
  ./userspace/fat32` regenerates them on macOS. Tests that need a host tool
  skip without it. `make test-unit` includes all of this.
- `make test-partitions` runs the partition-layer and block-slice tests (ADR
  0115): MBR and GPT parsing at 512 and 4096-byte blocks, every selector, GPT
  backup fallback and each header, array and entry corruption, seeded random
  corruption, slice bounds and read-only slices. On macOS `hdiutil` makes MBR
  and GPT disks whose FAT32 partition this code mounts and writes, and the
  host's `gpt`, `fdisk`, `hdiutil` and `fsck_msdos` read, mount and check the
  tables and volumes this code writes; two host-made disks in
  `userspace/partition/testdata` cover foreign tables everywhere
  (`GOOS_PARTITION_FIXTURES=1` regenerates them). The boot test boots from an
  MBR and a GPT disk and reads the guest's file back out of the partition.
- `make test-storage-stress` (`make test-fat32-stress`) runs the same tests at
  scale: 128 KiB to 512 KiB clusters, a 1 GiB and a 4 GiB − 1 file on a sparse
  host file, thousands of seeded corruptions, a directory of 8000 long names,
  and a longer model-based workload.
- `make test-latency` boots one guest and measures command and VFS latency
  into `build/reports/latency-<variant>.md`, where `<variant>` is the build's
  directory under `build/variants/` (`hz100-opt2` by default). It is a
  measurement, not a gate, and is not part of `make test-all`.
- `make test-microbench` boots one guest and runs the in-guest
  microbenchmarks (context switch, IPC, VFS reads, RAMFS metadata) into
  `build/reports/microbench-<variant>.md` (ADR 0085). Like `test-latency` it is
  a measurement, not a gate.
- `make test-performance` runs `test-microbench` and then the host
  microbenchmarks, writing every report under `build/reports/`.
- `make lint` runs `go vet`, the `gofmt` check, and
  `tests/lint/unsafe_boundary.sh`, which refuses `unsafe` in the kernel outside
  `uefi`, `vm`, and `amd64`, and `unsafe`, `//go:linkname`, and `//go:extern` in
  userspace outside `userspace/runtime` (ADR 0095). The script tests itself
  first by planting each forbidden form in a scratch tree.
- `make docs` checks that the required documents exist and runs
  `tests/docs/check.sh`: every relative Markdown link resolves, every ADR has
  a status and a decision, and the current release has notes.
- `make release-check` is the release gate. It runs `test-reproducible`
  first — which cleans `build/` and builds twice, so a clean build is part of
  the gate — then lint, docs, `make test`, `test-storage-stress`, the ISO, `tests/release/check.sh`,
  `test-boot`, and a two-guest `test-stress`, and the release checks once more
  at the end. The release checks require notes for the version in
  `internal/version/version.go`, a kernel that reports it, no validation marker
  or fault-injection report in any normal image, no retired `DebugWrite` stub,
  and no untracked or ignored file outside `build/`. Timing measurements are
  not part of it: they depend on the host.

When comparing timings between two trees, put both disk images on the same
host volume: QEMU writes the image file synchronously, so a slower volume
shows up as slower persistent writes (ADR 0096).

Use `make test` during the normal edit loop. Use `make test-all` before delivery
of changes that affect the kernel, native userspace, startup protocols, storage,
shell composition, or boot behavior.

## TLS evidence and local trust checks

Host `userspace/tlsclient` tests cover authenticated transfers and certificate,
record and handshake failures. `tools/tlspeer` adds `tamper` (encrypted application
record corruption) and `badverify` (CertificateVerify signature corruption)
fixtures. `tools/tlsfault` changes one Finished byte in a temporary host standard
library copy before the real TLS stack encrypts its record; it does not replace
the server stack. Host results do not establish native interoperability.
The native crypto gate confirms the selected primitives and explicit-root
X.509 verification; stock TinyGo TLS still fails its no-wire shim check.
The native HTTPS gate has now passed 69 assertions with authenticated transfers,
certificate/handshake/record failures and production-root loading. The earlier
ClientHello probe alone proved only wire output. The stream network regression
also passed (`make test-network NETWORK_SCENARIOS=stream`). Full `test-all` and
`release-check` have not run.
[The feasibility report](crypto-feasibility.md) keeps measured native results;
[ADR 0198](../adr/0198-native-tls-and-system-trust.md) describes the chosen path.

The HTTPS boot gate takes `ISO OVMF_CODE SYSTEM_TREE`. It requires the fixture
root in the supplied tree, creates a fresh report directory under
`/Volumes/Data/tmp/`, and uses one disposable guest with ephemeral host peers.
It checks shell exit status and X.509/record diagnostics, verifies zero HTTP
requests for rejected peers and the downgrade sink, and captures validation
resource markers. Validation-only `trustprobe --production` measures the pinned
full bundle from a fixed auxiliary file; native fetch retains its single system
CA path. The guest must power off within 30 seconds. Neither this gate
nor ordinary builds download trust data or contact an Internet server.

Before packaging production roots, check the committed artifact without writes:

```sh
go run ./tools/truststore -check security/ca-source.pem security/ca-certificates.pem
```

Host generator and system-image tests reject modified or malformed roots,
metadata mismatches and missing explicit inputs, prove deterministic generation,
and check that failed input validation leaves an existing output tree intact.
Validation root overrides require both `VALIDATION=1` and an explicit
`VALIDATION_CA_BUNDLE`; production builds use the pinned bundle.

## Proxmox hardware lab

`make test-pve-smoke`, `make test-pve-smp`, `make test-pve-linux`, and
`make test-pve-7zip` boot GoOS under KVM on a remote Proxmox VE node. They
need private credentials and are part of no other target; see
[the Proxmox hardware lab](proxmox-lab.md).

## QEMU boot test

`tests/boot/qemu_boot.sh` captures the complete serial streams in:

- `build/logs/boot.log` for the main validation boot;
- `build/logs/reboot.log` for the persistence boot.

`make test-boot MEMORY=4G` runs the same test with more guest RAM (default
`512M`). The physical-memory group then requires the validation kernel to have
used a page above 1 GiB from 2 GiB, and one above 4 GiB from 3 GiB (ADR 0103).

The target installs the system on a fresh `build/boot-test-virtio-blk.img`
for each run (`make test-disk`, ADR 0175). Tests therefore cannot corrupt or
overwrite the persistent `build/goos-disk.img` used by `make run` and `make
run-gui`; both QEMU phases share the fresh test image to validate reboot
persistence. The exclusive volume check runs in a forced-recovery boot of the
same image, because the running system refuses it.

`make run-gui` is a manual target and is never invoked by the automated test
hierarchy. It opens a host window, while `make test-boot` remains headless and
does not require Cocoa, GTK, SDL, or another host display backend. The manual
target defaults to `QEMU_DISPLAY=cocoa` and permits a host-specific override.
The boot test asserts the serial `FRAMEBUFFER_TERMINAL_OK` initialization marker. It also
requires `PS2_KEYBOARD_READY`, proving QEMU acknowledged the bounded i8042
initialization before userspace starts.
It requires `FRAMEBUFFER_USER_ISOLATION_OK` after constructing both validation
process page-table roots, proving neither root inherited the kernel framebuffer
slot.

`make test-console-gui` is the automated framebuffer test. It creates a fresh
disk, boots the validation ISO with standard VGA and `-display none`, captures
serial output, and waits for `FRAMEBUFFER_VALIDATION_PATTERN_OK`. It then sends
`screendump` over a private QEMU monitor socket and verifies the exact
magenta/cyan 16x16 checkerboard in the lower-right corner of the resulting PPM.
The test then resumes the guest and injects editing keys plus the mandatory
`echo graphical-keyboard-ok` command through the private QEMU monitor. The
keys reach the framebuffer console's session, `tty-fb` and `shell-fb`
(ADR 0125). COM1 is a file sink; the validation build lets `tty-fb` copy its
output there from the first key, so the `PS2_KEYBOARD_OK` marker and exact
command output prove the input traversed IRQ1, the decoder, the keyboard ring,
`tty-fb`, and `shell-fb`. The test
also runs `mount` and `df` through that framebuffer session and requires their
validation markers, proving the filesystem-information commands use the same
terminal-independent command path. The test
also requires `FRAMEBUFFER_USER_ISOLATION_OK`; it does not inspect text or
require OCR. Its log and image are retained as
`build/logs/console-gui.log` and `build/reports/console-gui.ppm`.

`go test ./userspace/tui` drives the reusable TUI layer with a host model
terminal. It checks clipped geometry, the protected bottom-right cell, dirty-row
suppression, list/dialog layout at 80x24, text scrolling and overlays, rejected
dimension overflow, and zero allocations while changed steady-state frames are
rendered and flushed.

`go test ./userspace/viewer` uses in-memory and synthetic readers without a
filesystem-sized allocation. It covers line/page/Home/End navigation, a 1 GiB
synthetic file, lines larger than the 4 KiB read window, horizontal clipping,
binary detection, C0/ESC sanitization, invalid UTF-8, read failure, status-path
truncation and allocation-free changed frames. The TinyGo `gview` image is also
built through the normal command target.

The same run then drives the file manager through IRQ1 only (ADR 0124): `cd
/tmp`, `gfm`, F1 and Escape, Tab, arrows, Home, End, PageDown, PageUp, F7 with
a typed name, and F10. `tty-fb` copies `gfm`'s ANSI output to COM1, so the log
shows its key bar, help and status text; after it exits, `GFM_EXIT_OK`,
`GFM_RAW_INPUT_OK`, `GFM_FUNCTION_KEYS_OK`, `GFM_MKDIR_OK`, `TTY_RAW_MODE_OK`
and `SHELL_TERMINAL_RESTORED_OK` prove the path from PS/2 through the decoder,
the ring, raw mode, the shell and the key parser, and `ls /tmp` listing
`fmtest/` proves the shell reads lines again. A second `gfm` is stopped with
Ctrl-C, and `echo after-gfm-ok` must still run. A screendump of the manager is
kept as `build/reports/gfm.ppm` for review.

The graphical scenario then opens `/system/motd` in `gview`, sends page,
horizontal, Home and End keys, exits with F10 and verifies line-mode shell input
afterwards. The COM1 boot scenario opens the same file, sends navigation and F10
as raw ANSI input, and likewise verifies the restored shell. These are the two
terminal-session acceptance paths; no viewer-specific framebuffer or keyboard
path exists.

Both scenarios then run the 0.1.7 workflow end to end. On the framebuffer the
test creates `/home/gv/note`, starts `gfm` there, views the file with F3,
returns with F10, edits it with F4, types `X`, saves with F2 and leaves the
editor and the manager with F10; `cat` must print `Xhello`. `GVIEW_EXIT_OK`,
`EDIT_SAVE_OK`, `EDIT_EXIT_OK`, `GFM_LAUNCH_OK` and a new
`SHELL_TERMINAL_RESTORED_OK` prove each step reached the program that owned the
keys. The COM1 boot test sends the same keys as raw ANSI on `/home/fm/note.txt`,
checks `Xsaved text` and checks that `ls` shows no `.note.txt.~save` left by
the save.

`go test ./userspace/safesave ./userspace/fat32 ./kernel/vfs` covers safe
replacement: every save step fails in turn and the original survives, and a
FAT32 replacement is cut at each of its device writes, remounted and checked to
hold one whole version with at worst lost clusters (ADR 0129).
`go test ./userspace/exec ./userspace/init` covers the launch protocol and
PID 1's policy for launched children (ADR 0130), and `go test
./userspace/editor ./userspace/filemanager` the editor model and `gfm`'s
viewer, editor, date and capacity handling on model terminals.

The boot test drives the COM1 session and never types at the keyboard, so
`tty-fb` copies nothing to COM1 and the test's marker counts see the COM1
session alone.

Host tests cover the rest of the stack: every F key's make and break codes and
exact sequence, the E0 navigation keys and the eight-byte bound
(`kernel/keyboard`); multi-parameter CSI, CUP, CHA, erase variants, all SGR
colours, split and malformed sequences, and padding guards
(`kernel/framebuffer`); the key parser, output builder and width helpers
(`userspace/terminal`); the raw-mode protocol, raw input buffer and reset
sequence (`userspace/tty`); and the manager's sorting, movement, scrolling,
refresh, dialogs, copy, move, mkdir and delete, error paths, layout at several
geometries and allocation-free rendering, against a model filesystem and a
model terminal that fails on any write past a row or into the scrolling corner
(`userspace/filemanager`).

The main phase has a 180-second bound. It validates kernel initialization,
memory isolation, scheduler/process behavior, IPC and capabilities, userspace
services, storage, filesystems, shell commands, pipelines, failure recovery,
resource-reuse stress, redirection, background jobs, and the on-disk persistent
bytes. It runs two background jobs while a foreground command and `services`
are served, lists them with `jobs`, stops one with `kill` and the foreground
command with Ctrl-C, and runs a four-stage pipeline whose request is larger
than one IPC message. The
reboot phase has a 60-second bound and reads the persisted file in a fresh VM
without recreating it. A third phase runs the M7 acceptance inside one VM,
logged to `build/logs/m7-reboot.log`. It creates `/home/m7`, writes a file in
it, runs the shell's `reboot`, and requires both boots and both reads of the
file. It uses `/home` because that is where the specification puts user data.

`make test-latency` runs with the log service present. A drain pass that has
records to write contends with the benchmark for the single-threaded filesystem
service, so the in-process `bench vfs` figure varies with whether one landed in
its window; the command latency medians do not move. ADR 0073 records the
isolation measurement.

The log group asserts that the log service started, that `/state` holds both the
live file and one rotation, and that a formatted record reads back out of the
file. The reboot phase then reads the rotated file, which can only have been
written before that boot, so a record surviving the reset is a persistence
proof. The validation build uses a smaller rotation bound so one session crosses
it.

The storage group asserts that the driver, which the validation build kills once
after the system is up, starts twice and that the block service is told about
the replacement and does not itself restart.

A fourth QEMU phase boots the system on a volume claiming far more sectors than
its device holds, logged to `build/logs/corrupt-volume.log`. It requires the
system to reach a prompt, report the refused volume, list a root without the
persistent mounts, run a command and answer a configuration query: a corrupt
disk must cost the storage, not the system.

The config group also asserts that a setting is actually consulted: changing the
prompt applies on the next line and is then restored, and the log shows two
sessions created with different rights. That a confined session cannot reach
outside its scope is covered by the store's unit tests and the control
protocol's round trip; proving it from inside the system would need a client
built to try, which is a fixture rather than a component.

The config group asserts that a value is checked against the type its key
declares, that a published value cannot be rewritten, that an undeclared key is
not a new setting, that the exported file holds the writable entries and not the
published ones, and that a value set in one session is restored in the next.

The release-shell group also asserts that the shell's CPU total stays in three
digits and that no console input was dropped, which is the regression guard on
the interrupt-driven console: before it, the shell was charged over seven
thousand ticks in a session.

The release-shell group asserts the resource accounting: that `ps` reports
non-zero pages for PID 1, that `quota` shows its own `tiny` share
of the stream table rather than all of it, and that PID 1 holds the system
share. The `QUOTA_OK` marker comes from a validation-build check that drives the
same charge-then-create order `StreamCreate` uses and requires the refusal to
arrive while registry slots remain.

The release-shell group also asserts the terminal session (ADR 0089). The
shell's capability list names a write-only `stdout` stream and no console, and
the shell checks in the validation build that the kernel refuses a read on it
(`TERMINAL_STREAM_READ_DENIED_OK`). A line typed into `readline` has to be
printed by it and never run as a command. `fault tty` and `fault shell`, a
validation-only shell command that asks PID 1 to kill the service, restart each
once: the terminal's replacement has to serve the running shell's next command,
the shell's replacement has to be served by the running terminal, and the
background job the old shell left has to be gone. The ipc-capabilities group's
`STREAM_SHARED_ENDPOINT_OK` comes from PID 1 sending itself a writer attached
twice and checking that the end stays open until both copies close, and that
closed and wrong-direction handles are refused.

The filesystem group also asserts the namespace layout: that `ls /` names every
mount, that `/system` is the boot image with its utilities listed under
`/system/bin`, that `mkdir /system/backdoor` is refused with "read-only
filesystem", and that a file written through `/home` reads back through
`/persist/home` as the same bytes. It executes `mount` and `df`, checks the
system-image and persistent-volume rows, and verifies the capacity columns.

The main phase also reads `runtime.heapptr` through the QEMU monitor at the
first prompt and again at the end, and fails if it moved: the kernel leaks every
allocation, so no repeating path may allocate (ADR 0065). The check is skipped
with a `SKIP` line where the monitor or `nc -U` is unavailable.

Successful runs print one `PASS` line per architectural group and a final check
count. Repeated guest output remains only in the full serial log. Failed checks
print a stable name, expected and observed values, the log path, and a bounded
context excerpt. Timeouts identify the phase and completion marker. The harness
never prints an entire serial log automatically.

Inspect the raw artifacts directly when more context is required:

```sh
less build/logs/boot.log
less build/logs/reboot.log
```

The diagnostic reporting path has a deterministic self-test that intentionally
returns failure without starting QEMU:

```sh
tests/boot/qemu_boot.sh --self-test-failure
```

## System boot test

`make test-system-boot` (`tests/boot/system.sh`) boots the validation ISO
against disposable disks made from the build's system tree, types commands
at the serial prompt, and powers each guest off. It checks:

1. the production archive, built from the same inputs without the
   validation probes, holds the storage services, the shell, and the recovery
   commands and none of `gfm`, `bench`, `uname`, the Linux fixtures,
   `fault-probe`, `config`, or `log`; and `make run-disk` leaves an existing
   disk byte for byte as it was while `make disk` refuses it;
2. a normal boot starts `config`, `log`, and `shell` from the volume
   (`SERVICE_FROM_DISK_OK`), runs `uname`, which only the volume has
   (`SYSTEM_COMMAND_FROM_DISK_OK`), and gives the shell
   `PATH=/system/bin:/home/user/bin`;
3. native commands cannot change the installed system through `/system`,
   `/persist/system`, case variants, rename, copy, or a moved directory, and
   the Linux `fsprobe` cannot open it for writing under either name; a copy of
   `fsck-fat` in `/home/user/bin` gets no storage slice, and `fsck-fat` in
   normal mode is refused as busy without unmounting anything; pipelines,
   redirection, and a Linux program still work;
4. after a reboot of the same disk the user's file is there and a copied
   command runs from `/home/user/bin` by its bare name without grants; after
   `fault fat32`, `fault vfs`, and `fault driver` the system is attached
   again, still protected, and runs commands from the volume; `fault storage`
   ends in recovery mode with one shell per console, counted both in the
   supervisor's records and in `ps`; while the VFS holds back the answer to a
   read of the configuration service's program, PID 1 still answers the
   shell, then starts the service when the read ends;
5. a forced-recovery ISO checks the installed volume with `fsck-fat` and
   mounts it again afterwards;
6. a missing disk, a blank volume, a malformed record, an older release, a
   missing shell image, and a malformed index each boot to a recovery prompt
   within 60 seconds with the stage and reason printed, where the archive's
   commands work; and recovery can `edit` a file and check the volume.

Every guest must power itself off: QEMU has to exit with status 0 within 30
seconds of `poweroff`, or the test fails. The one boot whose storage is lost
must instead refuse to power off and report the failed flush. A validation
archive adds `ps` to the recovery commands for these checks. The
transcripts are `build/logs/system-*.log`.

## Stress test

`tests/boot/stress.sh` drives one guest through a FIFO instead of a timed
script: it sends a command, waits for the next prompt, and fails if the prompt
does not appear within `COMMAND_TIMEOUT` seconds (default 30) or if any fatal
service marker appears. Each round reads and writes the FAT32 mount, runs a
pipeline, launches a failing pipeline stage, creates and removes files, and
runs a command sequence, so every round exercises VFS session creation and
teardown, peer death, process-slot reuse, and PID 1 exec handling. Output is
checked for the round's persistent read-back and pipeline data.

`tests/boot/stress_matrix.sh` runs several guests in parallel, each with its
own disk under `build/stress/hz<rate>/`, and prints one line per guest. Run it
at every supported rate before changing scheduling or timer behavior:

```sh
make test-stress HZ=100 STRESS_RUNS=8
make test-stress HZ=250 STRESS_RUNS=8
make test-stress HZ=1000 STRESS_RUNS=8
```

On failure the driver sends `diag`, so a `DIAG=1` build leaves the kernel state
dump in that guest's log.

## Latency measurement

The kernel calibrates the TSC against the PIT at boot and serves nanoseconds
since boot through native operation 24 (ADR 0054). Two guest tools use it:

- `time <command line>` is a shell builtin. It runs the rest of the line
  through the normal path, so builtins, pipelines, redirections, and `;`
  sequences all work, and then prints `time: <N> us`. The time covers the
  shell's exec request, PID 1, every launch, and the wait for exit.
- `bench vfs N` sends N `Stat /` requests and `bench read PATH N` opens, reads
  to EOF, and closes PATH N times, both from inside one process with its own
  VFS session. Each prints `bench <mode>: N ops, total <us> us, avg <ns> ns/op`;
  `read` adds the file size and the rate. N is at most 10000. A file under
  `/persist` exercises VFS, FAT32, the block service, and the virtio-blk
  driver. The other modes are described
  under [In-guest microbenchmarks](#in-guest-microbenchmarks).

`tests/boot/latency.sh` drives one guest through a FIFO the way the stress test
does. After a warm-up pass it runs each case under `time` `LATENCY_REPEATS`
times (default 10) and writes the median, p90 (nearest rank), and minimum per
case, followed by the `bench` lines:

```sh
make test-latency
make HZ=1000 test-latency LATENCY_REPEATS=20
make HZ=1000 DIAG=1 test-latency
```

The `help` case launches nothing and shows the measurement floor. The serial
transcript is kept in `build/logs/latency-<variant>.log`. In a `DIAG=1` build
the script ends with
`diag` and adds the `DIAG clock` and `DIAG launch` lines to the report: per
stage of `spawnExecutable` (image authority and extent check, address-space creation, image load,
process and grant setup, task install), the number of launches since boot and
their total and average time. Failed lookups are counted too. Numbers from
QEMU TCG describe the emulated system on that host, not hardware.

## Performance measurement

The specification (§51) asks for microbenchmarks of the context switch, IPC
ping-pong and throughput, shared-memory throughput, page allocation, page
faults, cached and uncached VFS reads, and RAMFS metadata operations, with
results in `build/reports/`, and for no optimisation on assumptions.
`make test-performance` produces two kinds of report.

Host benchmarks (`go test -bench`) time the kernel data structures on the host:
`page-allocation.txt` (`pmm` allocate/free), `ipc-ping-pong.txt` (channel
send/receive in one address space), `shared-memory.txt`, `stream-roundtrip.txt`,
`ramfs-metadata.txt`, and `vfs-resolve.txt`. They measure the algorithms, not
the system: no trap, no context switch, no second process.

### In-guest microbenchmarks

`make test-microbench` (ADR 0085) boots one normal-build guest, runs each case
of `tests/boot/microbench.sh` once to warm up and `MICROBENCH_REPEATS` times
(default 5), and writes `build/reports/microbench-<variant>.md` with the median
over the runs and the raw `bench` lines. The transcript is kept in
`build/logs/microbench-<variant>.log`.

| Case | Command | What it measures |
| --- | --- | --- |
| IPC ping-pong | `bench vfs 1000` | a request/reply on a channel between the client and the VFS service, including the service's `Stat /` |
| Stream calls, one process | `bench stream N SIZE` | write, drain, and empty read of a private stream plus a clock read per operation; no switch |
| Stream handoff | `bench send 200 1 \| bench recv 200 1` | one-byte round trips between two processes |
| IPC throughput | `bench send N SIZE \| bench recv N SIZE` | bytes per second through a two-stage pipeline |
| Cached VFS read | `bench read /system/bin/uname 50` | a 10 KiB file of the memory-resident system image |
| Uncached VFS read | `bench read /persist/uname.bin 20` | the same bytes on FAT32; file data is not held in the service's sector cache, so each pass reads the device |
| RAMFS metadata | `bench meta /tmp/bench.tmp 500` | create and unlink of one file in the RAMFS `/tmp`, two VFS requests |

`bench recv` times from its first chunk to its last and reads the clock after
every chunk. While it is blocked on the empty stream, every write wakes it and
the kernel switches to it on the write's return (ADR 0055); it drains the chunk
and blocks again. A chunk preceded by exactly one wait is therefore one round
trip, and the report gives the median of those intervals separately from the
average. The sender never blocks, so a timer tick that preempts the receiver
lets the sender fill the stream and the pair then moves many chunks per switch:
that is the throughput case, and it is why the handoff case is short.

The context switch is derived: (handoff op median - one-process op median) / 2,
both at one byte and both with one clock read per operation. The difference is
one switch between two address spaces plus half of a blocking wait, its wake,
and the retried wait.

```sh
make test-microbench
make HZ=1000 test-microbench MICROBENCH_REPEATS=9
```

Page faults and shared-memory throughput between processes are not measured in
the guest; ADR 0085 records why. The guest TSC advances in steps of about 1 µs
under TCG on an arm64 host, so single intervals are quantised to that step.
Other QEMU guests running on the same host change the results by tens of
percent; rerun on a quiet host before comparing builds.

## Adding a boot assertion

Keep the guest command in the existing serial input sequence. Add the assertion
to the matching architectural group using `require_marker`, `require_regex`,
`forbid_regex`, `require_count`, or `require_min_count`. Give every assertion a
stable dotted name and, when useful, an anchor matching the initiating command.
Do not add another monolithic condition or remove stress iterations to reduce
host output.

## TCP validation

Run TCP parser, state-machine, GTCP1 and service tests on the host first:

```sh
go test ./userspace/network ./userspace/tcpapi ./userspace/tcpserve
go test ./tools/netpeer
```

`make test-network` is the only TCP VM gate. It reuses the existing private
network boots and adds remote echo, payloads across MSS boundaries, delayed
handshake and reads, retransmission, refusal, reset, FIN and EOF, guest listen,
loopback, own-address and no-NIC loopback checks. It also verifies TCP status
and atomic tuning reload. Do not add a separate boot for a TCP case that fits
an existing guest.
# HTTP client validation

Host tests in `userspace/httpwire` and `userspace/fetch` exercise split reads,
framing, URL validation, request encoding and redirects. `make test-network`
uses the deterministic HTTP listener in `tools/netpeer` on ports 80 and 8080.
The private-segment guest checks GET, HEAD, POST, custom headers, chunks,
redirects and VFS output with `cat` and `stat`.

## Signed trust update gate

`make test-trust-update` runs offline peers and a validation system with a
separate update public key and bootstrap CA. The gate passed 47 assertions over
three boots of one persistent disk: fallback status, signed download/install,
authentication with a newly added CA, rollback/bad-signature refusal, exact
same-sequence reinstall, persistence, corrupt-selector recovery, and corrupt
signature rejection with bootstrap HTTPS recovery. Only public package files
enter the guest; host TLS and signing private fixtures stay outside its tree.

Host tests cover strict canonical metadata, signatures, keys, digest/count/size,
PEM bounds, sequence reuse, partial state, selectors, install failpoints and
capacity boundaries. Download tests reject non-200 responses, oversized fixed
and chunked bodies, truncation and TLS failures on any component. Release checks
compare the installed public key with production input and scan for validation
public/private signing material and private-key PEM.

The gate proves reboot persistence and fail-closed state selection, not recovery
from arbitrary physical torn sectors. Full `test-all` and `release-check` are
separate release gates; record their actual results before calling a release ready.

On 2026-10-09 the complete `make release-check` passed after the signed trust
extension: two clean builds produced 167 identical artifacts, the boot gate
passed 456 checks, system boot passed 156, Linux ABI passed 126, and both stress
runs passed 15 rounds each. TLS passed 69 assertions and signed trust update
passed 47 assertions across three persistent boots. The initial `make test-all`
stopped on an obsolete three-directory `/system` assertion; changing it to four
matched the security directory and passed in the release boot gate. We did not
repeat the identical full VM matrix. Optional kernel-heap monitor evidence was
unavailable in the initial boot run; that skip is not heap-stability proof.
