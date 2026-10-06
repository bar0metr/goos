# Roadmap

This roadmap records implementation priority from the current repository state.

## Current baseline

The following foundation is already implemented:

- UEFI boot and `ExitBootServices`;
- physical and virtual memory management with NX/W^X foundations;
- GDT/IDT, timer interrupts, guarded stacks, and preemptive scheduling on
  every processor;
- Process/Task separation and per-process address spaces;
- generation-safe typed handles and capability enforcement;
- ring-3 native processes and a versioned native image format;
- versioned typed IPC, bounded queues, request/reply, handle passing, waits,
  deadlines, peer-death handling, and shared-memory objects;
- bounded readiness-only multi-object waits for channels, streams, and processes;
- a userspace terminal service that owns the console's input and relays all
  shell and command output, restartable independently of the shell;
- RAMFS and namespace-scoped VFS services;
- PID 1, bounded supervision, restart policy, lifecycle events, and
  capability-bounded process spawning;
- interactive native shell;
- bounded `/system/bin` lookup, with the boot image itself mounted read-only at
  `/system` so the executables it resolves are the ones a listing shows;
- isolated native `pwd`, `ls`, and `cat` processes;
- capability-protected process handles, wait, completion records, and process
  resource reclamation;
- the versioned native startup ABI (`GST4` since ADR 0109) and named startup handles without
  application dependence on numeric handle slots.

The project has therefore reached the point where additional kernel mechanisms
are no longer the main route to a more useful system.

## Priority 1 — Complete the native application environment

Before expanding Linux compatibility, make native GoOS applications a stable
execution environment.

Completed in ADR 0031:

- define and document a versioned native process startup ABI;
- remove application dependence on globally assumed numeric handle IDs;
- pass named startup handles explicitly with bounded rights.

Completed in ADR 0032:

- stabilize structured `argv`, working-directory inheritance, exit status,
  and bounded startup/environment data;
- expand `/system/bin` with `echo`, `mkdir`, `touch`, `rm`, `cp`, `mv`, `stat`,
  `ps`, `mem`, `uptime`, `uname`, and `sleep`.

Maintained invariants:

5. Keep executable lookup bounded and policy-controlled. This is implemented
   for `/system/bin` and remains an invariant for later launch paths.
7. Keep `cd` as a shell builtin because it mutates shell-local state. This is
   implemented.
8. Keep each external command as an isolated userspace process with a minimal
   capability manifest. This is implemented and remains an invariant.

Priority 1 is complete. Full pipelines and job control are not prerequisites
for persistent storage.

## Priority 2 — Persistent storage

Persistent storage is the next major system milestone.

Implement the path as separate capability-isolated layers:

```text
application
    |
   VFS
    |
filesystem service
    |
block service
    |
virtio-blk driver
    |
virtio device
```

Required work:

1. Add a userspace `virtio-blk` driver.
2. Expose only the required device/MMIO/IRQ/DMA authority to that driver.
3. Define a bounded, versioned block-device IPC protocol.
4. Add a block/storage service rather than teaching VFS about virtio.
5. Add a simple persistent filesystem service. FAT32 is the preferred first
   target because images are easy to inspect and populate from the host.
6. Mount persistent storage through the existing VFS namespace model.
7. Keep initramfs as bootstrap/recovery storage rather than the permanent root
   of all mutable state.
8. Add a reboot persistence test.

Acceptance:

```text
goos> mkdir /home/demo
goos> write /home/demo/message.txt "persistent"
goos> cat /home/demo/message.txt
persistent
goos> reboot
...
goos> cat /home/demo/message.txt
persistent
```

The second `cat` must read data written before reboot. This runs verbatim: ADR
0071 mounts the volume's `home` and `state` directories at `/home` and `/state`
beside the whole-volume `/persist`, and the boot test performs the sequence
through `/home` inside one VM.

Current progress: ADRs 0033 through 0036 implement the bounded `GBLK001`
protocol, capability-scoped PCI/IRQ/DMA driver authority, the userspace
virtio-blk data path, and a bounded FAT32 backend mounted at `/persist`,
`/home` and `/state`. QEMU
validates file creation and readback through VFS and verifies the bytes in the
persistent raw image. The boot gate then starts a fresh VM on the same image and
reads the file without rewriting it. Items 1 through 8 are complete for the
initial bounded single-device design. Priority 2 hardening beyond the
deliberately constrained FAT32 subset remains incremental work rather than a
blocker for Priority 3.

## Priority 3 — Shell composition

After persistent storage works, finish the shell composition primitives:

- quoting and argument parsing;
- explicit stdin/stdout/stderr startup handles;
- I/O redirection;
- pipes implemented with native capability-protected stream/channel objects;
- exit-code propagation;
- sequential command execution.

Do not introduce POSIX file descriptors into the native ABI merely to make the
shell familiar. POSIX fd semantics belong to the Linux compatibility layer.

Current progress: ADRs 0037 and 0038 give every external utility distinct named
standard streams: read-only input role `in` plus write-only `stdout` and `stderr`.
Non-zero-exit diagnostics use `stderr`; all three streams currently target the
serial console. ADR 0039 adds bounded `readline` as the first utility that
consumes the explicit input role. ADR 0040 adds independent capability-protected
byte streams with directional endpoints, bounded buffering, backpressure, and
EOF after writer close. ADR 0041 adds scheduler-backed stream readiness and a
real two-process `producer | consumer` shell pipeline with stream endpoints
substituted into the child startup roles. ADR 0042 adds bounded sequential
execution with `;`, whole-line syntax validation, and completion before advancing
to the next command. ADR 0043 extends pipelines to four concurrent stages with
versioned bounded requests, downstream-first launch, and partial-failure cleanup.
ADR 0044 adds bounded stdout file redirection with `>` by composing the existing
stream abstraction with an isolated `write` utility and a fresh VFS session.
ADR 0045 extends that consumer with bounded `>>` append mode by seeking through
the existing VFS protocol. ADR 0046 adds bounded `<` input redirection by
prepending the existing `cat` utility as a VFS-backed pipeline producer. ADR 0047
adds bounded `2>` and `2>>` redirection with a stderr sink outside the stdout
chain, carried by exec protocol version 5, and fixes the task-slot reuse race it
exposed in the kernel.

Priority 3 is complete. Stream merging (`2>&1`) and job control are deliberately
out of scope and do not block Priority 4.

ADR 0048 fixes a freeze on persistent-storage reads whose root cause was the
freestanding arena allocators handing out uncleared memory, and adds resident
sectors, answered protocol rejections, and bounded storage deadlines. Measured
command latency is now dominated by process launch (about half a second) rather
than by storage. ADR 0049 names the timer rate and every deadline in one place
and records the measurements: at 1 kHz storage commands are three to five times
faster, while process launch is unchanged and therefore not timer-bound. ADR 0050 fixes the
liveness defects that experiment exposed: teardown now wakes waiters and closes
handles before any cleanup that can fail, completion records no longer depend on
a bounded queue, undeliverable lifecycle messages are retried instead of
dropped, every wait registers before re-checking its condition, and deadline
expiry is driven by the kernel loop. ADR 0051 adds bounded event-driven
multi-object readiness waits and migrates PID 1 lifecycle/exec handling, VFS
control/session handling, and block service/filesystem handling away from
polling.

ADR 0052 selects the timer rate at build time (`HZ=100|250|1000`) and adds a
diagnostic build with a kernel event trace and state dump. ADR 0053 moves kernel
housekeeping into the exclusion domain native calls run in. ADR 0054 adds a
calibrated nanosecond clock, the shell `time` builtin, `bench`, and
`make test-latency`. ADR 0055 reschedules on the return from the native call or
IRQ that woke a task, and lets kernel tasks sleep instead of halting through
their round-robin turn. ADR 0056 fixes the defect behind the 1 kHz failures:
SSE state was not part of the task context, so preemption corrupted XMM
registers. ADR 0057 records the launch-path analysis and the latency baseline.
Commands now take 4–9 ms at 100 Hz instead of 160–330 ms, a persistent write
21 ms instead of 1.9 s, and a VFS round trip 0.15 ms instead of 40 ms, with
latency nearly independent of the tick rate. `make test-stress` passes 16 of 16
guests at 100, 250, and 1000 Hz.

The default rate remains 100 Hz because nothing now depends on a faster one.
Stack, heap, BSS, and anonymous pages already arrive on first touch through
the common region fault path (ADR 0104). A deadline index lets the
housekeeping tick look only at the waits that are due (ADR 0189). Remaining
performance items are TinyGo utility startup cost and waking housekeeping
only when a deadline or lifecycle work is due.

## Priority 4 — Separate normal boot from validation boot

The production boot path should become progressively quieter and should not be
the permanent home of integration self-tests.

Keep automated checks, fault injection, isolation validation, and `*_OK`
markers in dedicated test/validation configurations where practical. Normal
`make run` should converge on a real system boot:

```text
GoOS ...
Memory: ...
Scheduler initialized
Starting PID 1
Starting services...
goos>
```

Boot and integration tests remain mandatory; only their placement changes.

The 0.1.5 framebuffer work keeps `make run` headless for serial automation and
adds `make run-gui` for manual graphical-console validation. Both targets boot
the same ISO and persistent disk and retain COM1 output. The preferred GOP mode
is 1280x800 with bounded fallbacks. IRQ-driven PS/2 input now joins COM1 in the
existing console queue; scan-code decoding, US layout translation, arrow CSI
sequences, and the shared Ctrl-C path do not change the userspace TTY ABI. The
GUI target explicitly selects a configurable display backend while automated
and ordinary runs remain headless. Automated framebuffer validation uses a
QEMU monitor screendump and exact validation-only checkerboard pixels, without
OCR or a host window. The same headless harness injects editing keys and
`echo graphical-keyboard-ok` through QEMU's PS/2 path; COM1 remains a file
sink, and exact shell output proves end-to-end keyboard delivery.
The TTY now reads capability-checked console geometry, falls back to 80x25 on
serial-only boots, and erases and restores every row of wrapped edited input
when asynchronous output arrives.
The graphical console uses Spleen 12x24 cells, yielding approximately 106x33
cells at the preferred 1280x800 GOP mode and making manual QEMU output readable
without reducing framebuffer resolution.
Framebuffer, GOP, and PS/2 regression coverage now includes hostile GOP
metadata, framebuffer guard words, the complete required key translation set,
and an in-guest assertion that process page tables do not inherit the kernel
framebuffer mapping.

Current progress: ADR 0058 splits the build into a normal and a validation
variant. A normal build boots quietly to a banner, `Starting services...`, one
`[ OK ]` line per service, and the prompt. `VALIDATION=1` keeps the kernel
self-tests, the scheduling demonstration, the isolation and fault-injection
processes, PID 1's supervision scenario, and every `*_OK` marker; `make
test-boot` always uses it, while the stress and latency harnesses run the
normal build.

The 0.1.0 shell and logging gaps are closed as well:

- ADR 0059 adds a structured system log: a kernel ring fed by the kernel and
  the services, with warnings echoed to the console and `log` in the shell.
- ADR 0061 adds `services`, `handles`, `caps`, `reboot`, and `ps` with
  type and state columns, built on public kernel operations and a PID 1
  control protocol.
- `reboot` syncs storage and resets through a capability held only by
  PID 1, and the boot test now runs the M7 write/reboot/read acceptance
  inside one VM.

ADR 0066 gives FAT32 long names, so `/persist` no longer restricts files to
the 8.3 form.

ADR 0067 adds `ProcessKill`, so the shell can stop a background job with
`kill` and the foreground command with Ctrl-C, and ADR 0068 makes `sleep` take
seconds and block instead of spinning.

ADRs 0069 through 0071 close the gap between the namespace and the system it
describes. A mount is now listed in its parent directory, so `ls /` names every
mount instead of only the entries one backend happens to own, and a directory
another mount points at cannot be removed, under its own name or under an alias.
`/system` is the boot archive mounted read-only through a capability only the
filesystem service holds, so `ls /system/bin` lists the executables the launch
path actually resolves and every write to them is refused with "read-only
filesystem"; the specification's acceptance criterion for the native application
environment therefore holds. `/home` and `/state` name user data and mutable
system state on the persistent volume, which is what the immutable-system-image
split needs and what the persistent-storage acceptance criterion is written
against.

ADR 0062 moves startup data through shared memory, so `write PATH "TEXT"`
from the spec fits and the M7 test uses it. ADR 0063 adds `MemoryMap`, so a
process can map a shared page it holds a capability for, and ADR 0064 uses one
for the exec protocol: PID 1 now runs up to four requests as jobs instead of
blocking on each command, and the shell can start a job with `&`.

ADR 0072 adds per-process resource quotas. Channels, streams, tasks and pages
are charged to the process that causes them to exist, so a process that creates
streams until it is refused leaves slots for everyone else instead of taking the
whole registry and breaking every later pipeline. CPU is accounted per process,
`ps` reports memory and CPU, and `quota` prints each process's share. That
accounting also exposed a defect it does not fix: the shell busy-polls the
serial console while waiting for input and is charged most of the ticks in a
session.

ADR 0073 adds the persistent log service. A supervised userspace process drains
the bounded kernel ring onto `/state`, so a session's records outlive its reset:
`cat /state/system.log` reads this boot and `cat /state/system.log.1` the one
before it. The ring stays the ordering point every component writes to; the
service is a consumer of it, holding only the console capability that authorises
reading it and a VFS session for its own file.

ADR 0074 adds the typed configuration service, which was the last of the 0.1.0
gaps. Keys are declared with a type, a write is checked against the declaration,
a session's scope and rights decide what it may see and change, and the writable
settings persist at `/state/config` in a form a person can read and edit. The
shell's `config` builtin lists, reads and writes them.

The 0.1.0 feature set of spec §56 is therefore complete. What remains before the
release is consolidation rather than new subsystems.

ADR 0075 closes the first of those: the console is interrupt-driven, its input
is a waitable object, and the shell blocks instead of retrying the read. It is
charged two timer ticks over a twenty-second session instead of 7,174. Fixing it
also uncovered and fixed a latent defect in the IOAPIC accessors, which were
ordinary Go memory operations the optimiser was free to reorder or discard.

ADR 0076 closes the second: the shell reads its prompt and the log service reads
the setting that governs it, and sessions are now created through a control
endpoint with per-client scopes and rights — the shell administers
configuration, the log service only reads the one namespace that governs it.

ADR 0077 closes the third: the geometry an image declares is checked against the
device before any offset is derived from it, and a volume the filesystem refuses
no longer takes the system with it. The machine boots to a prompt with the
persistent mounts absent, which is the state the volume can be inspected from.

ADR 0078 closes the last: the storage driver restarts, PID 1 gives its
replacement a fresh channel and tells the block service, and a dead peer no
longer kills the service its other clients depend on. The validation build kills
the driver on every boot so the path is exercised rather than assumed. It also
fixed a wider supervisor defect — a scheduled restart was only acted on when the
next event happened to arrive — and a log service that created its file only at
startup.

Consolidation is done. The 0.1.0 release remains a matter of judging the
existing surface rather than adding to it.

ADR 0079 is the first such judgement: every command has help and a synopsis
from one table, a malformed invocation is answered with its usage, diagnostics
name the command, the path and the reason, a launch failure names the stage
that failed, and `uptime` and `mem` report time and memory in the units a
person reads.

ADR 0080 makes every command its own image under `userspace/bin`, described by
a manifest in its source; the build generates the command index PID 1 grants
from and the shell lists, so adding a command is adding a directory.

ADR 0081 maps the read-only pages of a system image from one shared copy instead
of copying them into every process. Under TCG a launch no longer retranslates
code it has run before: `uname` takes 1.5 ms instead of 3.8 ms and a
three-stage pipeline 3.0 ms instead of 10.2 ms.

ADR 0084 compiles the kernel's RAMFS and VFS self-tests only into the
validation build, so the normal kernel links no filesystem code, and drops the
toolchain line from the boot banner.

ADR 0082 gives the console's input to a userspace terminal service, and ADR 0089
removes the two limitations it recorded. Stream endpoints are reference counted
in the kernel, so one writing end can be shared, and the shell's and every
command's output goes through the terminal service, which redraws the line being
typed around it; the shell and the commands hold no console capability. The
terminal service and the shell restart independently: whichever dies is
relaunched on a fresh session and the survivor is handed the other end, and a
dead shell's jobs are stopped with it. Building it found that a foreground
command had never received what was typed while it ran; that is fixed and
tested. Each printed line now costs a relay through the terminal, about ten
percent on a one-line command under emulation.

ADR 0088 widens the gap below every user stack to 64 KiB and the kernel stacks'
guard to two pages, and the build now fails if any function's stack frame could
step over them: TinyGo emits no stack probes for userspace, and the kernel's
probe routine is an empty stub. ADR 0091 gives the boot task, the last kernel
stack without a guard, a guarded stack of its own, and delivers double faults on
a separate stack, so a kernel stack overflow is reported with the guard address
it hit rather than resetting the machine; the boot test overflows one on purpose.
ADR 0090 makes TinyGo's `-opt=2` the default
build level, measured a fifth to a third faster on IPC, VFS and command paths
than the size-optimised `-opt=z`; building at that level found that the system
image window had stayed at two MiB after the boot archive bound grew to four
(ADR 0087 addendum). The release version is written once, in
`internal/version/version.go`.

ADR 0092 adds `poweroff`, which flushes storage and switches the machine off
through ACPI S5; the kernel reads the firmware's ACPI tables once at boot as
untrusted input.

GoOS 0.1.0 is released (docs/releases/0.1.0.md).

GoOS 0.1.1 (docs/releases/0.1.1.md) is an architectural cleanup that removes
0.1.0 shortcuts before Linux compatibility depends on them: the kernel launches
extents of the system image instead of resolving paths (ADR 0093), the
identity-authorized `DebugWrite` is retired (ADR 0094), userspace `unsafe` is
confined to `userspace/runtime` (ADR 0095), FAT32 runs as its own service
(ADR 0096), the kernel, PID 1 and shell sources are split by responsibility,
and `make release-check` gates a release.

GoOS 0.1.2 (docs/releases/0.1.2.md) prepares storage for Linux
compatibility. Programs anywhere outside `/system` run from executable
objects PID 1 fills from the file (ADR 0097), so a Linux ELF program on the
persistent volume will have a way to be launched. File data between the VFS
and FAT32 moves through a shared-memory file cache (ADR 0098), and the VFS,
the configuration and log services and the block service restart on failure
with their clients handed new sessions (ADR 0099, 0100, 0101).

GoOS 0.1.3 (docs/releases/0.1.3.md) removes the scalability limits Linux
compatibility would otherwise inherit: FAT32 as a complete filesystem
(ADR 0102), physical memory beyond the first GiB (ADR 0103), native
virtual-memory regions and demand-zero memory (ADR 0104), page-backed RAMFS
(ADR 0105), lazily loaded executables (ADR 0106), shared-memory bulk I/O
(ADR 0107), non-blocking program preparation in PID 1 (ADR 0108), a
variable-length startup object (ADR 0109), one name and path contract
(ADR 0110), bounded dynamic kernel registries (ADR 0111), interoperable FAT32
metadata (ADR 0112), and independent monotonic and realtime clocks initialized
from UEFI (ADR 0113).

GoOS 0.1.4 (docs/releases/0.1.4.md) completes the native storage and execution
base for Linux compatibility. It adds foreign FAT32 geometry and metadata,
raw/MBR/GPT block slices, dynamic mounts, native FAT checking and repair,
stable open-file identity, high-half kernel entry trampolines, file-backed
executable pages, and bounded launch resource profiles (ADR 0112 through
0122).

GoOS 0.1.5 (docs/releases/0.1.5.md) adds the UEFI GOP framebuffer console,
Spleen 12x24 text rendering, unified interrupt-driven PS/2 and serial input,
geometry-aware wrapped-line redraw, a manual graphical QEMU target, and
headless pixel and keyboard validation while retaining COM1 as the primary
diagnostic and fallback path.

GoOS 0.1.6 (docs/releases/0.1.6.md) adds `gfm`, a two-pane file manager, as
the first full-screen program, and the terminal support under it: F1-F12 and
navigation key sequences through an eight-byte decoder with whole-sequence
enqueue, multi-parameter CSI with cursor addressing and 16-colour SGR in the
framebuffer terminal, scoped raw terminal mode that the shell grants to
manifests declaring `GrantTerminal` and ends with the job (ADR 0124), separate
COM1 and framebuffer consoles with a terminal service and shell each
(ADR 0125), and the reusable `userspace/terminal` package. A viewer, an editor, recursive copy and
cross-filesystem move are left for later releases.

GoOS 0.1.7 extends native filesystem metadata (ADR 0126) and capacity discovery
(ADR 0127). FAT32, RAMFS and the immutable image return typed metadata through
version 10 of both filesystem protocols: explicit UTC timestamps, stable
attributes, checked 64-bit capacity, read-only state and a non-authorizing,
generation-safe VFS mount ID. VFS provides bounded mount enumeration, while the
read-only `df` and `mount` commands expose it without device authority.
The bounded `userspace/tui` layer now provides caller-owned screens, checked
geometry, dirty rows, dialogs and text/list viewports (ADR 0128). Native `gview`
uses a fixed read window for large-file navigation, clips long lines and
sanitizes file controls before terminal rendering. Version 11 of both protocols
adds whole-file `Replace` (ADR 0129), through which the new gap-buffer editor
`edit` saves without truncating the original (ADR 0131). `gfm` starts `gview`
and `edit` through a launch channel PID 1 grants by manifest, with the child's
own grants and the caller's input (ADR 0130), and shows modification times and
free space. GoOS 0.1.7 is released (docs/releases/0.1.7.md). GoOS 0.2.0
follows it with Linux ABI compatibility and GoOS 0.2.1 with Linux process
composition (Priority 5).

Known gaps left by ADR 0089: the system services that start before the terminal
still write the console directly, as does the kernel's warning echo, so either
can land in a line being typed; and what was typed but not finished is lost if
the terminal service dies. ADR 0123 removes the one-row redraw limitation.

## Priority 5 — Linux ABI compatibility

GoOS 0.2.0 ([docs/releases/0.2.0.md](../releases/0.2.0.md)) is the first
Linux ABI release. Unmodified, statically linked Linux x86_64 programs run as
GoOS processes through a kernel-resident ABI adapter backed by the
process-scoped capabilities and the VFS service.

What it contains:

- immutable `Native`/`LinuxX8664` process identity and a dedicated
  `SYSCALL` entry that the native gate and the Linux gate each refuse to the
  other;
- a bounded static ELF64 loader for `ET_EXEC` and static PIE images with lazy
  file-backed pages, and a System V initial stack that exposes no native
  startup object;
- a PID 1 launch policy of three streams and one VFS session, enforced again
  by the kernel;
- a syscall layer with its own number and errno namespaces, continuations for
  calls that block, a 64-entry descriptor table, anonymous memory, FS-base
  TLS, and files, directories, status, and naming through the VFS session;
- identity, clocks, sleep, limits, and signal state answered from kernel
  state, and random bytes from the CPU's generator;
- `/dev/null` and `/dev/zero`, positioned I/O, and `statx`;
- six pinned fixtures, a conformance probe that prints the same lines on
  Linux and on GoOS, a strict reference run on a real Linux kernel, a Linux
  boot gate, and a BusyBox harness.

[linux-x86_64-abi.md](../architecture/linux-x86_64-abi.md) is the contract,
with every call, each difference from Linux, and the release limits.

### Process composition

GoOS 0.2.1 ([docs/releases/0.2.1.md](../releases/0.2.1.md)) lets one Linux
program run another:

- a bounded family registry with zombie records, reparenting, and process
  budgets that keep native slots free;
- per-process descriptor tables over shared open-file descriptions and one
  serialized VFS context per family;
- in-place `execve`, private-snapshot `fork`, `vfork`, and the process forms
  of `clone`;
- `wait4`, `pipe`, and `pipe2` with atomic `PIPE_BUF` writes;
- signal delivery on an `rt_sigframe`, `kill` on the caller's tree,
  interrupted calls with `SA_RESTART`, `rt_sigsuspend`, and `pause`;
- `poll` and `ppoll` on native readiness;
- a non-interactive BusyBox `sh` gate.

### The installed system

GoOS 0.2.2 ([docs/releases/0.2.2.md](../releases/0.2.2.md)) runs the system
from disk:

- a small boot archive at `/bootstrap` with the storage services, the shell,
  and the recovery commands;
- the installed system on the FAT32 volume, checked by PID 1 and published at
  `/system`, protected by the VFS on its directory's node under every name;
- executable origin by path, file-backed service launch, and grants only from
  the index of a command's own origin;
- normal and recovery modes, restart of the storage stack under a running
  system, and recovery when it does not come back;
- `make run` on a disk that is never overwritten implicitly.

### GoOS 0.2.3: static glibc and threads

GoOS 0.2.3 ([docs/releases/0.2.3.md](../releases/0.2.3.md)) adds static glibc
programs (ADR 0176) and Linux threads: thread-form `clone`, per-thread TLS,
TIDs, signal masks, and sleeping `futex` waits (ADR 0177). Open files survive a
FAT32 service restart (ADR 0178). 7-Zip runs with its own thread count.

### Symmetric multiprocessing

Every processor the firmware lists, up to 64, starts and runs tasks
([SMP](../kernel/smp.md), ADR 0181 to ADR 0183). Threads of one process run
in parallel; per-processor run queues, local APIC timers, reschedule
interrupts, and TLB shootdown carry it. Kernel calls take the locks of the
domains they touch (ADR 0184), so faults and the fast Linux calls of one
program's threads run in the kernel at once. Each process's handle table,
each Linux family, and each channel has a lock of its own (ADR 0187), so
unrelated Linux programs make file and process calls at once. A family's
threads still run their file calls one at a time. A task can be limited to
some processors through `TaskAffinity` or `sched_setaffinity`, and the `cpu`
command shows each processor's load and scheduler counters (ADR 0188).

### Next: a Linux terminal

The next release is to give Linux programs a terminal: a terminal descriptor,
`isatty`, `TIOCGWINSZ`, a termios subset, sessions and process groups, a
foreground group, terminal-generated `SIGINT`, `SIGTSTP`, and `SIGCONT`, and an
interactive BusyBox `sh`. Signals aimed at one thread, rather than at its
process, remain.

Later: interactive programs such as `nano`, and `utimensat`, which needs the
VFS to set a file's times. A random source for CPUs without RDRAND and a
randomized load base are independent steps. Dynamic linking stays separate
work.

Linux syscalls translate into native GoOS primitives and userspace services.
They must not create a privileged bypass around VFS, IPC, handles, or
capabilities.

## Priority 6 — In-system compilation experiment

A compiler running inside GoOS is a long-term proof that the system is a usable
computing environment rather than only a bootable image.

Do not start with TinyGo itself. First target a small compiler that can:

```text
goos> cc hello.c -o hello
goos> ./hello
Hello GoOS
```

Only after that milestone should a larger Go/TinyGo toolchain be evaluated.

## Explicit non-priorities

Do not move these ahead of the native application and storage milestones unless
a concrete blocker requires them:

- splitting the linux, channel, or handle lock without a measured need;
- full ASLR;
- IOMMU implementation beyond keeping DMA APIs IOMMU-ready;
- networking;
- USB;
- GUI;
- ext4;
- glibc;
- broad Linux ABI coverage;
- scheduler redesign without measured need.

Correctness, isolation, capability boundaries, and measurable performance remain
higher priority than feature count.
