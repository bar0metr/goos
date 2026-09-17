# Roadmap

This roadmap follows `GOOS_SPEC.md` and records implementation priority from the
current repository state. It does not replace the architectural ADRs.

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
- userspace TTY, RAMFS, and namespace-scoped VFS services;
- PID 1, bounded supervision, restart policy, lifecycle events, and
  capability-bounded process spawning;
- interactive native shell;
- bounded `/system/bin` lookup;
- isolated native `pwd`, `ls`, and `cat` processes;
- capability-protected process handles, wait, completion records, and process
  resource reclamation.

The project has therefore reached the point where additional kernel mechanisms
are no longer the main route to a more useful system.

## Priority 1 — Complete the native application environment

Before expanding Linux compatibility, make native GoOS applications a stable
execution environment.

Required work:

1. Define and document a versioned native process startup ABI.
2. Remove application dependence on globally assumed numeric handle IDs.
3. Pass named startup handles explicitly with bounded rights.
4. Stabilize `argv`, working-directory inheritance, exit status, and bounded
   startup/environment data.
5. Keep executable lookup bounded and policy-controlled.
6. Expand `/system/bin` with small native utilities:
   `echo`, `mkdir`, `touch`, `rm`, `cp`, `mv`, `stat`, `ps`, `mem`, `uptime`,
   `uname`, and `sleep`.
7. Keep `cd` as a shell builtin because it mutates shell-local state.
8. Keep each external command as an isolated userspace process with a minimal
   capability manifest.

Quoting may be added here. Full pipelines and job control are not prerequisites
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

The second `cat` must read data written before reboot.

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
