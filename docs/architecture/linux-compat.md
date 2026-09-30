# Linux compatibility

**Status: GoOS 0.2.0 foundations in progress.** The kernel has immutable native
and Linux process identities, a dedicated x86_64 `SYSCALL`/IRETQ entry, bounded
static ELF loading, a System V initial stack, and a constrained PID 1 launch
policy. The syscall layer has a distinct Linux number and errno namespace,
bounded diagnostics, generation-safe blocking continuations, a 64-entry fd
table, stream `read`/`write`/`writev`, the memory and TLS calls, and
`exit`/`exit_group`. A pinned static musl program starts through its normal
libc entry. Through its VFS session a program opens, creates, reads, writes,
seeks, renames, and removes files and directories, by absolute path or
relative to a working directory it can change.

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
| `read`, `write`, `writev` on files | VFS bulk requests through the process's I/O object |
| `read`, `write`, `writev` on stdio and pipes | stream endpoints |
| `open`, `openat`, `close`, `lseek`, `fstat`, `getdents64` | VFS session requests plus the fd table |
| `getcwd`, `chdir` | the working directory in compatibility state, checked against the VFS |
| `mkdir`, `unlink`, `rmdir`, `rename` and their `*at` forms | VFS create, remove, rename, and replace |
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

### File calls

The file calls run on the bridge, several exchanges each where needed
(ADR 0141). The continuation keeps the stage, the resolved paths, and what one
stage hands the next, so a handler resumed after a wake reads no user memory
it already consumed.

- `open` and `openat` stat the path, create the file for `O_CREAT`, empty it
  for `O_TRUNC`, and open it. A directory gets a descriptor that keeps its
  canonical path, because the VFS lists directories by path. 32 directory
  records exist system-wide.
- `read`, `write`, and `writev` on a file move bytes through one 64 KiB
  memory object per process. The bridge lends it to the VFS for each request
  with only the rights that request needs. A longer request is a short
  transfer.
- `lseek` sets the position the VFS keeps per open file; the kernel mirrors
  it in the open-file description. `SEEK_END` and `O_APPEND` ask the VFS for
  the file length through a stat by handle.
- `getcwd` and `chdir` work on the stored working directory. `chdir` accepts
  a path only after the VFS confirms a directory.
- `mkdir`, `unlink`, `rmdir`, and `rename` map to the VFS operations, with
  their `*at` forms and the legacy numbers musl uses. Removal stats the name
  first so that each call refuses the other's kind.

The flags an open accepts and the errno of each refusal are in
[linux-x86_64-abi.md](linux-x86_64-abi.md). Unsupported flags return
`EOPNOTSUPP`. GoOS has no permission bits, so mode arguments are ignored and a
refused write is `EROFS`. Removing or replacing a file some session holds open
is `EBUSY`, and a directory moves only onto a free name.

`fstat`, `newfstatat`, `getdents64`, `readv`, `ftruncate`, and `fsync` are not
implemented.

### Path model

Linux paths resolve in the GoOS VFS namespace: `/` is the VFS root, with no
chroot and no mount namespace (ADR 0140). Each process keeps a working
directory in kernel-side state, inherited from its launch and stored in
canonical form.

One bounded pass joins a relative path to the working directory and removes
empty, `.`, and `..` components; `..` at the root stays at the root. An
absolute path ignores `dirfd`. A relative path starts at the directory its
`dirfd` names: `ENOTDIR` when that descriptor is not a directory, `EBADF` when
it is not open.

The pass refuses what GoOS cannot hold and shortens nothing: `ENOENT` for an
empty path, `ENAMETOOLONG` for a string with no terminator in 4096 bytes, a
component over 1024 bytes, or a result over 4096 bytes. A path that ends in a
slash, `.`, or `..` demands a directory; an open of it is `ENOTDIR` on a
regular file and never opens the file of that name.

A resolved path over 216 bytes does not fit a request message. The bridge puts
it in a one-page memory object charged to the process and moves the handle to
the VFS with the request, as native clients do.

### Namespace view

The expected layout for early targets:

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
build recipe, the checksum, and the syscall trace. PID 1 launches it in
`/system/linux` with two arguments and two environment variables, reads its
complete output, and requires exit status zero before it publishes
`LINUX_MUSL_OK`, `LINUX_VFS_SESSION_OK`, `LINUX_PATH_OK`, and
`LINUX_FILE_IO_OK`. `userspace/linux/run-on-linux.sh` runs the same binary on
a Linux kernel, where it prints the same lines.

The program reaches `main` through `__libc_start_main`. It checks `argv` and
`envp`, a TLS image with initialized and zero-filled data behind FS, `malloc`
through both `brk` and `mmap`, buffered stdio flushed at exit, an open/close
round trip on its VFS session, its own image under several relative
spellings, and file I/O and naming in a scratch directory under `/tmp`. musl startup needs `arch_prctl`,
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
