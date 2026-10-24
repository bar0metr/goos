# GoOS Roadmap

This roadmap records the current implementation state and the next major development priorities.

GoOS is no longer at the stage where adding kernel mechanisms is automatically the most useful work. The current system already has a substantial reclaiming and concurrent native userspace, persistent storage, SMP, Linux static-binary compatibility, native IPv4 networking, HTTPS, and native SSH remote administration. The next milestones should therefore focus on making those environments more complete without weakening capability boundaries or turning the native ABI into POSIX by accident.

## Current baseline

The following foundation is implemented.

### Kernel and execution

- x86_64 UEFI boot and `ExitBootServices`;
- physical and virtual memory management with per-process address spaces;
- NX/W^X foundations, guarded stacks, lazy anonymous and executable pages;
- GDT/IDT, calibrated monotonic/realtime clocks and interrupt-driven timers;
- preemptive SMP on up to 64 processors;
- per-CPU run queues, migration, remote rescheduling and TLB shootdown;
- fine-grained kernel locking rather than one global kernel lock;
- Process/Task separation;
- generation-safe typed handles and capability enforcement;
- bounded process/task/page/channel/stream quotas;
- native ring-3 processes and a versioned native image/startup ABI;
- shared memory and capability-protected IPC;
- bounded request/reply, handle passing, deadlines and peer-death handling;
- event-driven multi-object waits;
- cursor-aware log waits while preserving the existing `WaitAny` ABI;
- process kill, completion records and resource reclamation;
- CPU affinity and per-CPU runtime statistics.

### Native userspace and services

- PID 1 supervision, restart policy and capability-bounded process spawning;
- separate userspace terminal, VFS, FAT32, configuration, logging, networking and device services;
- RAMFS plus namespace-scoped VFS mounts;
- immutable installed `/system`, bootstrap/recovery `/bootstrap`, persistent `/home` and mutable `/state`;
- userspace virtio-blk and virtio-net drivers;
- persistent FAT32 storage with interoperable metadata, VFAT long names, MBR/GPT slices, checking/repair, stable open-file identity and restart recovery;
- versioned typed configuration and persistent system log;
- independent restart of storage/network/terminal-facing services where supported;
- capability-scoped VFS authorities for sensitive paths and administration operations;
- a reclaiming per-process native heap with conservative stop-the-world collection;
- native sibling threads with kernel-created guarded stacks and shared-process execution;
- a dedicated M:N goroutine target with channels, select, sync, timers and scheduler-aware waits;
- a goroutine-backed VFS read pool as the production default, with a serial VFS fallback kept under the boot gate.

### Native shell and applications

- interactive native shell;
- quoting and escaping;
- bounded command sequences with `;`;
- native pipelines;
- stdin/stdout/stderr as typed streams;
- `<`, `>`, `>>`, `2>` and `2>>` redirection;
- background jobs and `jobs`/`kill`;
- foreground Ctrl-C and Ctrl-D;
- PATH-based executable lookup;
- manifest-backed native commands under `/system/bin`;
- local serial and framebuffer terminals;
- full-screen native `gfm`, `gview` and `edit`;
- dynamic foreground terminal geometry for running native TUI applications;
- shell/admin commands including `head`, `tail`, `grep`, `wc`, `find`, `hexdump`, `hostname` and `date`;
- structured `log` filtering and event-driven `log -f`.

### Native networking

- userspace virtio-net;
- Ethernet II, ARP, IPv4 and ICMP;
- DHCP and static IPv4 configuration;
- RFC 5227-style address-conflict handling;
- loopback and own-address delivery;
- hosts table;
- native DNS resolver with cache;
- DNS-over-TCP fallback for truncated replies;
- generic native UDP sessions;
- generic native TCP sessions;
- `ping`, `net`, `resolve`, `dnsq`, `udp` and `tcp`;
- native HTTP/1.1 `fetch`;
- native TLS 1.3 and X.509 verification;
- persistent system trust store and signed trust-store updates;
- HTTPS redirects, explicit root verification and hostname/IP SAN checks.

### Native remote administration

- native SSH service `remoted`;
- Ed25519 public-key authentication;
- persistent Ed25519 host identity;
- isolated host signing: workers receive signing endpoints rather than host private keys;
- worker revocation when the signer peer dies;
- interactive SSH PTY shell;
- remote `exit`;
- graceful remote `reboot` and `poweroff`;
- non-PTY SSH command execution using the native shell parser;
- separate stdin/stdout/stderr and propagated SSH exit status;
- PTY `window-change` and live resize for `gfm`, `gview` and `edit`;
- `remote status` / `remote reload`;
- transactional `remote keys`, `remote key add` and `remote key remove`;
- authorized-key changes without exposing host private-key authority;
- dedicated QEMU and Proxmox/KVM remote-administration validation.

### Linux compatibility

GoOS runs unmodified statically linked Linux x86_64 ELF programs through a\ndedicated kernel-resident ABI translation subsystem, separate from the native\nABI but not from the kernel.

Implemented support includes:

- static `ET_EXEC` and static PIE;
- musl and static glibc applications;
- Linux process families;
- `fork`, `vfork`, `execve` and supported `clone` forms;
- Linux threads and TLS;
- pipes;
- signals and signal frames;
- futex waits;
- polling;
- descriptor tables and shared open-file descriptions;
- anonymous memory;
- file and directory access through VFS;
- clocks, sleep, limits and identity calls;
- static BusyBox non-interactive shell workloads;
- multithreaded applications such as 7-Zip.

Linux compatibility remains intentionally separate from the native ABI.

---

# Completed release milestones

## 0.1.x — Native foundation

### 0.1.0

Established the first coherent native system: native process model and shell, typed IPC and capability handles, initial VFS/RAMFS, persistent FAT32 path, supervision, configuration/logging, process controls and the native command model.

### 0.1.1

Architectural cleanup: removed early kernel/path shortcuts, removed `DebugWrite`, confined userspace `unsafe`, separated FAT32 into its own service, split kernel/PID 1/shell responsibilities and introduced `make release-check`.

### 0.1.2

Prepared storage and execution for Linux compatibility with executable objects outside `/system`, a shared-memory file cache and service restart/reconnect paths.

### 0.1.3

Removed storage/execution scalability limits: more complete FAT32, memory above 1 GiB, native VM regions and demand-zero, page-backed RAMFS, lazy executable loading, shared-memory bulk I/O, bounded dynamic registries, interoperable FAT32 metadata and monotonic/realtime clocks.

### 0.1.4

Completed the native storage/execution base with foreign FAT32 geometry, raw/MBR/GPT slices, dynamic mounts, FAT checking and repair, stable open-file identity, high-half entry trampolines, lazy file-backed executable pages and bounded launch profiles.

### 0.1.5

Added the UEFI GOP framebuffer console, Spleen text rendering, PS/2 keyboard input, shared serial/framebuffer input, geometry-aware redraw, `make run-gui` and headless framebuffer/keyboard validation.

### 0.1.6

Added `gfm`, expanded keyboard sequences, CSI/SGR terminal support, scoped raw terminal mode and reusable terminal/TUI layers.

### 0.1.7

Expanded filesystem metadata and native TUI applications with capacity/mount discovery, typed timestamps/attributes, `df`, `mount`, `gview`, `edit`, nested TUI launch and safer whole-file replacement.

## 0.2.x — Compatibility, SMP, networking, remote administration and native concurrency

### 0.2.0 — Linux ABI

Introduced the static Linux x86_64 compatibility layer: Linux ELF loading, Linux syscall entry, descriptor tables, files/directories/memory/clocks/randomness, initial signals/polling and conformance against a real Linux kernel.

### 0.2.1 — Linux process composition

Added process families, `fork`, `vfork`, `execve`, supported `clone`, `wait4`, pipes, signal delivery, `poll`/`ppoll` and non-interactive BusyBox `sh`.

### 0.2.2 — Installed system and recovery

Moved the normal system onto persistent disk with `/bootstrap`, installed `/system`, executable-origin tracking, normal/recovery modes, storage-stack restart recovery and non-destructive `make run`.

### 0.2.3 — Static glibc and threads

Added static glibc compatibility, Linux threads, per-thread TLS/TIDs, futex waits, open-file survival through FAT32 service restart and multithreaded 7-Zip workloads.

### 0.2.4 — Symmetric multiprocessing

Added SMP across all supported processors, per-CPU queues/timers, migration, TLB shootdown, fine-grained kernel locking, affinity, per-CPU accounting and KVM/Proxmox SMP validation.

### 0.2.5 — Native IPv4 networking

Added the native network path from userspace virtio-net through IPv4, UDP, TCP and DNS to the HTTP/1.1 `fetch` client. DHCP/static addressing, loopback, hosts, DNS cache/TCP fallback and generic UDP/TCP sessions are included.

IPv6 and Linux socket syscalls remain outside the native networking design.

### 0.2.6 — Native HTTPS and trust updates

Added native HTTPS to `fetch` using pinned Go software TLS/X.509 compiled through the TinyGo compatibility path.

Implemented TLS 1.3, X25519, standard TLS 1.3 AEAD suites, X.509 chain validation, DNS/IP SAN identity, native entropy/realtime validation, pinned Mozilla/curl trust roots, persistent signed trust-store updates and offline authenticated TLS regression gates.

Stock TinyGo TLS remains unsuitable as the selected byte-stream TLS implementation; GoOS uses the pinned/adapted software stack instead.

### 0.2.7 — Native remote administration

GoOS 0.2.7 adds a native administration plane rather than bolting OpenSSH or a POSIX shell onto the system.

Implemented:

- native SSH interactive PTY sessions;
- non-PTY SSH exec using the GoOS shell parser;
- isolated host-key signing;
- persistent host identity;
- signer peer-death revocation;
- authorized-key administration;
- graceful remote reboot/poweroff;
- remote `exit`;
- proper CRLF behavior on OpenSSH/serial host terminals;
- bounded stdout ordering before prompts;
- live resize of running native TUI programs;
- structured `log -f`;
- native admin utilities;
- persistent hostname;
- dedicated exact-SHA Proxmox/KVM remote-admin validation;
- repository-local portable build/test scratch space under `build/tmp`.

The remote shell still intentionally excludes SFTP/SCP, SSH forwarding, agent/X11 forwarding, POSIX shell semantics and Linux PTYs.

See [remote access](remote-access.md), [ADR 0199](../adr/0199-native-remote-terminal-sessions.md), and [ADR 0200](../adr/0200-native-administration-contracts.md).

### 0.2.8 — Reclaimable and concurrent native userspace

GoOS 0.2.8 replaces request-scoped bump-allocation discipline with a reclaiming
native heap and extends native userspace from one execution stream to
multithreaded and selected M:N goroutine workloads.

Implemented:

- size-classed slab/page-run allocation, conservative mark-and-sweep, real
  `runtime.free`/`runtime.GC`/`runtime.ReadMemStats`, `VMDiscard`, and
  scoped `arena.Region` scratch (ADR 0201);
- measured allocator tuning, diagnostics, the 1360-byte class, bounded stack
  scrub and configurable free-page retention (ADR 0202);
- native sibling threads with guarded kernel-created stacks, generation-safe
  IDs, join/yield, per-thread runtime state, thread-owned slabs and
  stop-the-world snapshots (ADR 0203);
- a source-pinned TinyGo overlay for M:N goroutines, channels, select, sync,
  timers, preemption and scheduler-aware waits (ADR 0204);
- goroutine-aware GC stack planning across G and M stacks, full held GPR/XMM
  snapshots, allocator/region transition windows and fail-closed rendezvous;
- a bounded concurrent VFS read pool with ordered mutation barriers,
  quarantined-cache recovery, independent control handoff and fenced
  filesystem replacement; the goroutine VFS is the default and the serial
  build remains a regression fallback;
- a userspace concurrency/caller audit and dedicated native-thread,
  goroutine, GC, I/O, VFS and KVM validation;
- deterministic `swaphold` coverage of all 15 reachable
  `goos_g_swap` boundaries and all 47 reachable
  `goos_preempt_entry` boundaries, plus isolated coverage of XMM0-XMM15
  and the 15 saved general-purpose register roots.

The preemption stress exposed and fixed an asynchronous SSE-state corruption:
the preemption entry now saves vector state before making its native call.
The integrated concurrency soak and full `test-all` passed before the final
context-save coverage additions, which then passed their targeted TCG and KVM
gates.

See [ADR 0201](../adr/0201-reclaimable-userspace-memory.md),
[ADR 0202](../adr/0202-allocator-measurements-and-tuning.md),
[ADR 0203](../adr/0203-native-multithreaded-userspace.md),
[ADR 0204](../adr/0204-native-goroutines-and-concurrency.md), the
[native concurrency validation report](native-concurrency-validation.md) and
[0.2.8 release notes](../releases/0.2.8.md).

---

# Next priorities

The next major compatibility milestone is the Linux terminal and interactive
BusyBox. Linux runtime and ABI work remains driven by real applications after
that.

## Priority 1 — Linux terminal support

The next major compatibility milestone is to give Linux programs a real terminal.

This should not be implemented as a broad `/dev/pts` clone first. Build the smallest coherent Linux terminal contract on top of the existing native terminal/session model.

Required work:

1. Add a Linux terminal-backed descriptor type.
2. Implement `isatty` behavior.
3. Add `TIOCGWINSZ` and terminal geometry propagation.
4. Add the minimal termios subset needed by real interactive programs.
5. Add Linux sessions and process groups.
6. Track one foreground process group per terminal.
7. Generate terminal signals: `SIGINT`, `SIGTSTP`, `SIGCONT`.
8. Connect Linux foreground job lifetime to the existing native terminal/session lifecycle.
9. Add an interactive BusyBox `sh` gate.

Acceptance target:

```text
goos> /system/linux/busybox sh
/ # echo hello
hello
/ # stty
...
```

Interactive shell editing, Ctrl-C and terminal resize must work without granting Linux processes native terminal or console authority directly.

### After the first Linux terminal

Once the terminal contract is stable:

- run `nano`;
- add missing terminal ioctls only when a real application requires them;
- add missing file timestamp/metadata semantics only where real editors expose a gap;
- consider additional static CLI/TUI applications as compatibility probes.

Do not turn this milestone into full Linux TTY subsystem emulation unless an actual application demonstrates the need.

## Priority 2 — Linux runtime completeness

After interactive Linux programs work, improve the static Linux environment where concrete programs expose gaps.

Likely work:

- thread-directed signal cases not yet represented cleanly;
- remaining file metadata/time semantics exposed by real applications;
- remaining practical ioctls required by common CLI programs;
- stronger `/proc`-like introspection only if an application needs it;
- better diagnostics for unsupported syscalls;
- compatibility tests based on real programs rather than syscall-count goals.

The rule remains:

> Add Linux ABI surface because a useful program requires it, not because Linux has the syscall.

## Priority 3 — Entropy and load randomization

Two security/portability items are independent of the terminal milestone and can be done separately.

### CPU-independent random source

Add a native entropy source path that also works when RDRAND is unavailable, while preserving fail-closed behavior for cryptographic consumers.

### Randomized executable load base

Add bounded randomized base selection for compatible executable mappings.

This is not a full ASLR project. Do not expand it into complete Linux/macOS-style address-space randomization before the simpler mechanism is measured and validated.

## Priority 4 — Native shell/application polish

The native environment is already usable. Future improvements should remain small and capability-oriented.

Candidates:

- `which`;
- stream merge such as `2>&1`, if it can be expressed cleanly in the existing typed-stream model;
- additional small native utilities where they materially improve administration;
- better command discovery/help presentation;
- TUI usability improvements;
- optional history/editing improvements that remain bounded.

Avoid importing POSIX shell semantics wholesale.

## Priority 5 — Networking beyond the current native IPv4 stack

The current network stack is sufficient for HTTP/HTTPS, DNS and SSH administration.

Future networking work should be driven by real use cases.

Possible later milestones:

- IPv6;
- multicast/mDNS;
- stronger TCP compatibility/performance;
- additional application protocols;
- Linux socket syscalls backed by the native network services.

Linux networking must remain an adapter over native services rather than a privileged parallel network stack.

Encrypted DNS, HTTP/2, HTTP/3 and QUIC are not immediate priorities.

## Priority 6 — Dynamic Linux linking

Dynamic ELF support remains separate from the static Linux ABI.

Do not begin with broad glibc compatibility. First define:

- ELF interpreter handling;
- shared-object mappings;
- relocation model;
- loader trust and path policy;
- bounded mapping/accounting rules;
- interaction with executable origin and capability grants.

A minimal musl dynamic binary is a better first target than a general desktop Linux loader.

This milestone should come after the static interactive environment is strong enough to justify it.

## Priority 7 — In-system compilation experiment

A compiler running inside GoOS remains a long-term proof that the system is a usable computing environment rather than only a bootable image.

Do not start with TinyGo itself.

First target a small compiler that can do:

```text
goos> cc hello.c -o hello
goos> ./hello
Hello GoOS
```

Only after that should a larger Go/TinyGo toolchain be evaluated.

The compiler experiment is not a reason to weaken filesystem, executable-origin or capability boundaries.

---

# Ongoing engineering priorities

## Correctness and isolation

Prefer explicit capabilities, typed IPC, narrow grants, fail-closed validation, generation-safe identities, bounded memory/protocol state, peer-death handling and deterministic cleanup.

Do not introduce global ambient authority merely for compatibility convenience.

## Measured performance

Continue measuring before redesigning.

Current important areas include:

- native/TinyGo process launch overhead;
- housekeeping wakeup only when deadlines/lifecycle work exists;
- TCP throughput/latency under real KVM;
- TUI/terminal redraw efficiency;
- Linux syscall/descriptor overhead under practical workloads.

Avoid scheduler rewrites without evidence.

## Release reproducibility

Every release candidate should retain:

- exact candidate SHA;
- toolchain versions;
- reproducible-build checks;
- host/unit/race gates;
- VM regression gates;
- hardware/KVM evidence where the release changes hardware-sensitive code;
- release notes that describe actual results rather than planned tests.

Build/test scratch data belongs under ignored repository-local `build/` paths and must not depend on one developer's machine layout.

---

# Explicit non-priorities

Do not move these ahead of the milestones above unless a concrete blocker requires them:

- full ASLR;
- broad POSIX conversion of the native ABI;
- full Linux TTY emulation before the minimal interactive contract is proven;
- IOMMU work beyond keeping DMA APIs ready for it;
- USB;
- desktop GUI;
- ext4;
- broad dynamic-linker compatibility;
- broad Linux ABI coverage for its own sake;
- HTTP/2, HTTP/3 or QUIC without an application need;
- scheduler redesign without measured need.

Correctness, isolation, capability boundaries, useful real programs and measured performance remain higher priority than feature count.
