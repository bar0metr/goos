# Linux compatibility

**Status: GoOS 0.2.0 foundations in progress.** The kernel has immutable native
and Linux process identities, a dedicated x86_64 `SYSCALL`/IRETQ entry, bounded
static ELF loading, a System V initial stack, and a constrained PID 1 launch
policy. The syscall layer has a distinct Linux number and errno namespace,
bounded diagnostics, generation-safe blocking continuations, a 64-entry fd
table, stream `read`/`write`/`writev`, the memory and TLS calls, and
`exit`/`exit_group`. A pinned static musl program starts through its normal
libc entry, and `openat` and `close` reach files through the process's VFS
session.

## Principles

- Compatibility is a separate kernel subsystem. The native
  kernel ABI, handle model, and IPC protocols do not change to fit POSIX.
- Linux programs get no authority that a native program with the same
  manifest would not get. Translation must not bypass VFS, IPC, handles, or
  capabilities.
- Each translated syscall is bounded like a native call: validated lengths,
  bounded buffers, deadlines on blocking paths.
- Unsupported syscalls fail with `-ENOSYS` and are counted, rather than being
  approximated silently.

## Components

### ELF loader and mapper

Linux ELF is a distinct format from the native envelope
([native-images.md](../kernel/native-images.md)). The implemented parser:

1. accepts ELF64 little-endian x86_64 `ET_EXEC` only;
2. bounds the table at 64 program headers and 16 `PT_LOAD` segments;
3. validates file/memory arithmetic, page congruence, user and reserved ranges,
   page-rounded overlap, entry location, and `R--`/`RW-`/`R-X` protections;
4. rejects `PT_INTERP`, `PT_DYNAMIC`, W+X, executable stack, and unsupported
   program-header kinds;
5. records validated `PT_PHDR`, `PT_TLS`, `PT_GNU_STACK`, and `PT_GNU_RELRO`
   metadata from the existing stable executable source.

The mapper records that plan as pinned `region.Image` reservations backed by the same
stable system-image extent or executable object as native images. Pages remain
absent until touched. The common fault path reads the file-backed prefix,
zero-fills BSS, enforces final `R--`/`RW-`/`R-X` permissions, and charges every
private frame and new page table to the process. A fault outside a reserved
load remains an ordinary unmapped fault.

Congruent non-page-aligned loads retain their aligned file window. The mapper
revalidates the exported plan and current source length, preflights all region
collisions, and accepts conventional low addresses such as `0x400000` while
keeping loads below the fixed GoOS user windows. It also reserves an 8 MiB NX
demand-zero stack with a 64 KiB guard. `ET_DYN` remains P2 scope.

The stack builder converts the kernel's validated startup view into a Linux stack image,
not a mapped `GST4` object. It emits `argc`, NUL-terminated `argv` and `envp`,
and the static-ELF auxv required by the release specification. The complete
image is capped at 64 KiB and 256 entries in each vector, with 16-byte initial
RSP alignment. Synthetic UID/GID are 1000 and `AT_CLKTCK` is 100.
`AT_PLATFORM` is `x86_64`; `AT_RANDOM` is absent until trustworthy entropy is
available. Only occupied stack pages are installed; the remaining reservation
stays demand-zero. The common launch path installs this state before scheduling.

### Syscall entry

Linux programs use the `syscall` instruction with the Linux register
convention. That entry path is distinct from the native `int 0x80` gate, so
a process is either native or Linux and the kernel never guesses which ABI a
call uses. Process identity is kernel-owned and immutable. The native gate
authorizes only `Native`; the MSR-backed Linux gate authorizes only
`LinuxX8664` (ADR 0132).

The Linux gate switches from the untrusted user RSP to the current task's
guarded kernel-entry stack through supervisor-only GS data before using the
stack or entering Go. It then installs the kernel CR3, snapshots the Linux
register ABI into a fixed frame, and returns through a higher-half IRETQ
prefix. The dispatcher snapshots all six scalar arguments into a Linux request
whose number type is distinct from native `kcall.Operation`. Known but not yet
implemented calls and unknown calls both return `-ENOSYS`; an eight-entry ring
and saturating counter retain bounded diagnostics without per-call logging.

Every handler must validate flags and lengths before copying
through `vm.CopyIn`/`vm.CopyOut`. They may never retain or directly dereference
an arbitrary user pointer. Copied strings must fit their operation-specific
bound and contain a terminating NUL in user memory.

### Translation

Syscalls map onto native primitives:

| Linux | Native mechanism |
| --- | --- |
| `read`, `write` on files | VFS session requests |
| `read`, `write`, `writev` on stdio and pipes | stream endpoints |
| `openat`, `close`, `fstat`, `getdents64`, `lseek` | VFS session requests plus the fd table |
| `set_tid_address` | compatibility PID and stored address; no native ID |
| `pipe2` | `StreamCreate` |
| `exit`, `exit_group` | `Exit` |
| `clock_gettime` | `ClockReadNanos` |
| `execve`, `wait4` | exec request to PID 1, process handle wait |
| `ppoll`, `poll` | `WaitAny` |
| `brk`, `mmap`, `mprotect`, `munmap`, `madvise` | native bounded region map, demand-zero faults, page quota, and page release |
| `arch_prctl` | task-local FS base; GS remains architecture-owned |

Translation runs in a separate kernel compatibility subsystem for 0.2.0. It
may access caller memory after the ordinary address-space checks, but authority
still comes only from process-owned resources. It cannot call filesystem or
device implementations directly (ADR 0134).

Blocking translations use one fixed continuation slot per scheduler task. A
slot contains the generation-safe task ID, syscall number, stage, validated
scalar arguments, one bounded copied path, correlation, and partial progress.
The request is marked issued before an external effect, then persisted as
waiting before the architecture path removes the task from the runnable set.
Wake accepts only the exact task generation and correlation. The resumed task
retries the two-byte `SYSCALL`; the stage and issued bit prevent a duplicated
effect. Kill, fault, and process teardown cancel the slot. VFS peer death is
retained as a correlation-checked wake reason so the handler can return an
error, then completion clears the slot. Stream `read` and backpressured stream
`write` use this path without polling.

### File descriptors

Each Linux process has a bounded fd table mapping small integers to private
compatibility entries plus per-fd state. An fd is never a native handle.
Descriptors 0, 1, and 2 bind to the supplied `in`, `stdout`, and `stderr`
streams. Files are reached only through the process's explicit VFS session
(ADR 0135).

The table has 64 entries and separate shared open-file descriptions carrying
kind, status flags, offset, object reference, and reference count. Exhaustion is
`EMFILE`; teardown invalidates every descriptor and reports each final
description once.

Errno conversion is operation-specific. VFS permission denial becomes `EACCES`
or `EROFS` only with caller context, VFS resource exhaustion requires the
caller to identify the exhausted resource, stream EOF is successful for reads,
and a write to a closed reader returns `EPIPE`.

### VFS bridge

A file operation is one request on the channel PID 1 granted at launch and one
reply with the same correlation (ADR 0138). The bridge looks the channel up in
the process's handle table for every exchange and requires Read, Write, and
Wait rights. The kernel has no session of its own.

The calling task's continuation records the correlation before the request
enters the queue, then the task blocks in the channel wait set. The VFS reply,
the peer's death, or an eight-second deadline wakes it through the path native
receivers use. After the wake the bridge accepts the reply whose correlation
and operation match, drops replies to calls that gave up earlier, and fails
the call with `EIO` on anything else. It closes handles a message attached.

Peer death returns `EIO` and marks the session failed; later requests fail the
same way. The bridge opens no new session for a running process, so a VFS
restart cannot hand a process authority PID 1 did not grant. A deadline expiry
returns `ETIMEDOUT`.

`openat` and `close` run on the bridge today. `openat` takes an absolute path
of at most 216 bytes, an access mode, `O_CLOEXEC`, and `O_LARGEFILE`. Relative
paths and the other flags return `EOPNOTSUPP`; a longer path returns
`ENAMETOOLONG`. The VFS opens regular files only, so opening a directory
fails with `EIO` until directory descriptors exist. `close` frees the
descriptor, then closes the native stream handle or the VFS file it named.
File `read` and `write` do not use the bridge yet.

### Namespace view

Linux paths resolve through the process's VFS session. The expected layout
for early targets:

- `/persist`, `/tmp`, and `/system` as today;
- `/dev/null`, `/dev/tty` provided by the compatibility layer or a service;
- `/proc` and `/sys` absent until a program needs a specific file.

## Launch and security boundary

PID 1 may classify native magic versus ELF magic, but the kernel independently
validates the selected format. A Linux launch receives only supplied standard
streams, one fresh VFS session, argv, environment, cwd, and a resource profile.
It receives no device, loader, system-image, PID 1 launch, DMA, IRQ, port,
framebuffer, raw-keyboard, or kernel-log authority (ADR 0136).

The common launch path retains executable-source lifetime, address-space
creation, accounting, task creation, waits, and teardown. Format-specific code
owns validation, mappings, entry state, and startup layout. Linux programs do
not receive the native `GST4` startup object (ADR 0133).

The lifecycle protocol carries PID 1's bounded format candidate. The kernel
re-reads the stable source header, rejects mismatches, runs the selected loader,
and initializes `LinuxX8664` only after ELF validation. Linux launches require
the `large` profile and exactly four grants named `in`, `stdout`, `stderr`, and
`vfs`, with stream/channel types and rights fixed by the kernel. Resolved handle
IDs and cwd remain in a bounded kernel compatibility table; only argv, envp,
and auxv enter the Linux stack. The validation image
`/system/linux/raw-probe` exercises classification, policy, mapping, stack
installation, task creation, raw memory and stdio syscalls, FS-relative TLS,
wait, exit, and teardown. PID 1 runs two instances with distinct FS bases and
holds back their remaining input until both have installed TLS and blocked.

FS base is task context and is saved and restored on every context switch.
`ARCH_SET_FS` and `ARCH_GET_FS` are implemented. GS operations are rejected and
kernel GS remains architecture-owned (ADR 0137).

### musl startup

`/system/linux/musl-hello` is a static, non-PIE C program linked against a
pinned musl (ADR 0139). `userspace/linux/README.md` records the toolchain, the
build recipe, the checksum, and the syscall trace. PID 1 launches it with two
arguments and one environment variable, reads its complete output, and
requires exit status zero before it publishes `LINUX_MUSL_OK` and
`LINUX_VFS_SESSION_OK`.

The program reaches `main` through `__libc_start_main`. It checks `argv` and
`envp`, a TLS image with initialized and zero-filled data behind FS, `malloc`
through both `brk` and `mmap`, buffered stdio flushed at exit, and an
open/close round trip on its VFS session. musl startup needs `arch_prctl`,
`set_tid_address`, and `writev`. Its terminal probe, `ioctl(TIOCGWINSZ)`, gets
`ENOSYS`, which musl reads as "no terminal".

## Iterative targets

| Step | Target | Needs |
| --- | --- | --- |
| 1 | static x86_64 "hello world" | ELF loader, `write`, `exit` |
| 2 | static musl program | `brk`/`mmap`, `arch_prctl`, TLS, auxv |
| 3 | VFS probes | fd table, file I/O and directories through VFS |
| 4 | selected BusyBox applets | measured P1 syscalls; no shell |

Each step is gated on host tests and a QEMU check, like native features.
Dynamic linking, glibc, `fork`/`clone`, Linux `execve`, threads, full signals,
networking, BusyBox shell, and `nano` are outside GoOS 0.2.0.
