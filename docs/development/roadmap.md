# Roadmap

This roadmap records implementation priority from the current repository state.

## Current baseline

The following foundation is already implemented:

- UEFI boot and `ExitBootServices`;
- physical and virtual memory management with NX/W^X foundations;
- GDT/IDT, timer interrupts, guarded stacks, and preemptive scheduling;
- Process/Task separation and per-process address spaces;
- generation-safe typed handles and capability enforcement;
- ring-3 native processes and a versioned native image format;
- versioned typed IPC, bounded queues, request/reply, handle passing, waits,
  deadlines, peer-death handling, and shared-memory objects;
- bounded readiness-only multi-object waits for channels, streams, and processes;
- userspace TTY, RAMFS, and namespace-scoped VFS services;
- PID 1, bounded supervision, restart policy, lifecycle events, and
  capability-bounded process spawning;
- interactive native shell;
- bounded `/system/bin` lookup, with the boot image itself mounted read-only at
  `/system` so the executables it resolves are the ones a listing shows;
- isolated native `pwd`, `ls`, and `cat` processes;
- capability-protected process handles, wait, completion records, and process
  resource reclamation;
- the versioned `GST3` native startup ABI and named startup handles without
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
Remaining performance items are demand-zero stack and heap pages, TinyGo
utility startup cost, and a deadline index so the housekeeping loop wakes only
when a deadline is due.

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
release is consolidation rather than new subsystems: making the components that
own a setting consult the configuration service instead of a constant, the
shell's busy-poll of the serial console (ADR 0072), FAT32 hardening, and storage
failure recovery.

## Priority 5 — Linux ABI compatibility

Begin Linux compatibility only after native process startup, persistent storage,
and basic shell composition are stable.

Order:

1. static Linux x86_64 hello world;
2. minimal Linux syscall translation;
3. static musl program;
4. BusyBox;
5. termios/signals/polling;
6. `nano`.

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

## Explicit non-priorities for the current phase

Do not move these ahead of the native application and storage milestones unless
a concrete blocker requires them:

- SMP;
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
