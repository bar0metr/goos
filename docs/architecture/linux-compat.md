# Linux compatibility

**Status: implemented in GoOS 0.2.0.** Unmodified, statically linked Linux
x86_64 programs run as GoOS processes. The kernel has immutable native and
Linux process identities, a dedicated `SYSCALL`/IRETQ entry, bounded static
ELF loading, a System V initial stack, and a constrained PID 1 launch policy.
A translation layer in the kernel turns Linux calls into the primitives a
native process uses: VM regions, byte streams, the kernel clocks, and one VFS
session per process. Static musl programs and BusyBox applets run on it.
[linux-x86_64-abi.md](linux-x86_64-abi.md) is the contract: every call, its
status, and each difference from Linux.

## Principles

- Compatibility is a separate kernel subsystem. The native
  kernel ABI, handle model, and IPC protocols do not change to fit POSIX.
- Linux programs get no authority that a native program with the same
  manifest would not get. Translation must not bypass VFS, IPC, handles, or
  capabilities.
- Each translated syscall is bounded like a native call: validated lengths,
  bounded buffers, deadlines on blocking paths.
- Unsupported syscalls fail with `-ENOSYS` and are counted, rather than being
  approximated silently. A call is added when a measured program or the
  release scope needs it, with real semantics.

## Components

### ELF loader and mapper

Linux ELF is a distinct format from the native envelope
([native-images.md](../kernel/native-images.md)). The implemented parser:

1. accepts ELF64 little-endian x86_64 `ET_EXEC`, and `ET_DYN` with no
   interpreter, which is a static position-independent executable
   (ADR 0149);
2. bounds the table at 64 program headers and 16 `PT_LOAD` segments;
3. validates file/memory arithmetic, page congruence, user and reserved ranges,
   page-rounded overlap, entry location, and `R--`/`RW-`/`R-X` protections;
4. rejects `PT_INTERP`, W+X, executable stack, `PT_DYNAMIC` in an `ET_EXEC`
   image, and unsupported program-header kinds;
5. records validated `PT_PHDR`, `PT_TLS`, `PT_GNU_STACK`, and `PT_GNU_RELRO`
   metadata from the existing stable executable source;
6. checks the file range of `PT_NOTE`, `PT_GNU_EH_FRAME`, and
   `PT_GNU_PROPERTY` and otherwise ignores them.

A static PIE loads at one fixed base, `0x40000000`. The parser adds the base
to every address the image names before it validates them, so the mapper and
the fault path see an ordinary plan. The kernel applies no relocation; the
program's startup code relocates itself from `AT_PHDR`.

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
demand-zero stack with a 64 KiB guard.

The stack builder converts the kernel's validated startup view into a Linux stack image,
not a mapped `GST4` object. It emits `argc`, NUL-terminated `argv` and `envp`,
and the static-ELF auxv required by the release specification. The complete
image is capped at 64 KiB and 256 entries in each vector, with 16-byte initial
RSP alignment. Synthetic UID/GID are 1000 and `AT_CLKTCK` is 100.
`AT_PLATFORM` is `x86_64`. `AT_RANDOM` points at 16 bytes from the CPU's
RDRAND generator and is absent on a CPU without one (ADR 0151). Only occupied
stack pages are installed; the remaining reservation
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
implemented calls and unknown calls both return `-ENOSYS`. Saturating
counters, an eight-entry ring, and a set of every refused number retain
bounded diagnostics without per-call logging.

Every handler must validate flags and lengths before copying
through `vm.CopyIn`/`vm.CopyOut`. They may never retain or directly dereference
an arbitrary user pointer. Copied strings must fit their operation-specific
bound and contain a terminating NUL in user memory.

### Translation

Syscalls map onto native primitives:

| Linux | Native mechanism |
| --- | --- |
| `read`, `readv`, `write`, `writev` on files | VFS bulk requests through the process's I/O object |
| `read`, `readv`, `write`, `writev` on stdio and pipes | stream endpoints |
| `open`, `openat`, `close`, `lseek` | VFS session requests plus the fd table |
| `getdents64` | VFS directory entries by the path a directory descriptor keeps |
| `stat`, `lstat`, `fstat`, `newfstatat`, `statx` | VFS stat by path or by handle; a synthetic record for streams and devices |
| `pread64`, `pwrite64` | a VFS seek, a bulk request, and a seek back |
| `open`, `read`, `write` on `/dev/null` and `/dev/zero` | the kernel itself; no VFS exchange |
| `rt_sigaction`, `rt_sigprocmask` | compatibility state; nothing is delivered |
| `getrandom` | the CPU's RDRAND generator |
| `getcwd`, `chdir` | the working directory in compatibility state, checked against the VFS |
| `mkdir`, `unlink`, `rmdir`, `rename` and their `*at` forms | VFS create, remove, rename, and replace |
| `set_tid_address` | compatibility PID and stored address; no native ID |
| `exit`, `exit_group` | `Exit` |
| `clock_gettime`, `nanosleep` | the kernel realtime and monotonic clocks; a timer deadline |
| `getpid`, `getuid`, `uname`, `prlimit64` and their relatives | compatibility state and fixed bounds |
| `dup`, `dup2`, `dup3`, `fcntl` | the descriptor table; no native call |
| `access`, `faccessat` | a VFS stat, answered from the mode `stat` reports |
| `brk`, `mmap`, `mprotect`, `munmap`, `madvise` | native bounded region map, demand-zero faults, page quota, and page release |
| `arch_prctl` | task-local FS base; GS remains architecture-owned |

The next milestone plans `pipe2` on `StreamCreate`, `execve` and `wait4` on
an exec request to PID 1 and a process-handle wait, and `poll` on `WaitAny`.

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

Each Linux process has a bounded fd table mapping small integers to local
entries. An fd is never a native handle. Descriptors 0, 1, and 2 bind to the
supplied `in`, `stdout`, and `stderr` streams. Files are reached only through
the process's explicit VFS authority (ADR 0135).

The table has 64 entries. Its entries point to bounded, family-owned open-file
descriptions carrying kind, status flags, offset, object reference, and
reference count. Inheritance and `dup` retain the same description, so file
position, `O_APPEND`, stream endpoint, directory cursor, and device state stay
shared. Close-on-exec remains in the local fd entry. The last reference closes
the underlying resource. Exhaustion is `EMFILE`; teardown invalidates every
local fd and reports each final description once (ADR 0156).

Errno conversion is operation-specific. VFS permission denial becomes `EACCES`
or `EROFS` only with caller context, VFS resource exhaustion requires the
caller to identify the exhausted resource, stream EOF is successful for reads,
and a write to a closed reader returns `EPIPE`.

### VFS bridge

A file operation is one request on the channel PID 1 granted at launch and one
reply with the same correlation (ADR 0138). The bridge looks the channel up in
the calling process's handle table for every exchange and requires Read, Write,
and Wait rights. The kernel has no filesystem authority of its own.

Related processes share a bounded family VFS context with one current owner
and a FIFO queue. A process retains its own VFS channel handle and file I/O
buffer. The shared context serializes correlations, service-owned file
positions, and directory cursors without spinning. Completion wakes only the
owning generation-safe task. Kill, fault, and exit remove queued tasks before
their scheduler slots can be reused (ADR 0156).

The calling task's continuation records the correlation before the request
enters the queue, then the task blocks in the channel wait set. The VFS reply,
the peer's death, or an eight-second deadline wakes it through the path native
receivers use. After the wake the bridge accepts the reply whose correlation
and operation match, drops replies to calls that gave up earlier, and fails
the call with `EIO` on anything else. It closes handles a message attached.

Peer death returns `EIO` and marks the session failed; later requests fail the
same way. The boot test proves both ends of this in the VM: a program whose
VFS restarted gets `EIO`, and one whose request went unanswered gets
`ETIMEDOUT` and then keeps using its session. The bridge opens no new session for a running process, so a VFS
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

- `stat`, `lstat`, `fstat`, and `newfstatat` translate a VFS stat into a
  hand-laid-out `struct stat` (ADR 0142). Mode, owner, and link count are
  synthetic presentation. `st_dev` and `st_ino` come from the VFS mount and
  node identity, so two names of one file compare equal.
- `readv` reads once and fills its elements in order, from a stream or a
  file.

- `getdents64` lists a directory descriptor one VFS entry per exchange and
  packs whole `linux_dirent64` records (ADR 0143). The listing position is
  the VFS cursor in the directory's description.
- `readv` and `writev` take 16 elements and 64 KiB per call, with every
  element checked before a byte moves (ADR 0144).
- `statx` runs the same stat and writes the 256-byte `struct statx`, with a
  birth time when the filesystem has one (ADR 0150).
- `pread64` and `pwrite64` seek the VFS position to the offset, transfer, and
  seek back, because the VFS reads and writes only at the position it keeps.
  The description's position never changes.

`ftruncate` and `fsync` are not implemented.

### Devices

The kernel compares a resolved path with `/dev/null`, `/dev/zero`, and
`/dev/tty` before it asks the VFS (ADR 0150). The first two open as
descriptors of a synthetic kind: a read of `/dev/null` is at its end, a read
of `/dev/zero` returns zeros, and both discard writes. `/dev/tty` has a
status and opens with `ENXIO`, because a GoOS Linux process has no
controlling terminal. All three stat as character devices. No VFS node
stands behind them, so native programs and directory listings do not show
them, and the names cannot be removed or renamed.

### Signal state and random bytes

`rt_sigaction` and `rt_sigprocmask` store dispositions and the blocked mask
in the process's compatibility state and report them back (ADR 0151).
Nothing delivers a signal from that state. The calls that would send one
stay `ENOSYS`.

`getrandom` returns bytes from RDRAND, 256 per call at most, and `ENOSYS` on
a CPU without the instruction. The kernel has no other source and makes none
up.

### Identity and time

`getpid`, `gettid`, `getppid`, the four user and group calls, `uname`,
`clock_gettime`, `nanosleep`, and `prlimit64` answer from kernel state with no
service (ADR 0145). The PID is the compatibility PID, and the user and group
are 1000; none of them is GoOS authority. A launch root has
parent PID 1. Compatibility-created descendants use the parent stored in the
Linux family registry, and orphans return to PID 1 (ADR 0154). `uname` names the
Linux ABI and the GoOS release that implements it. `clock_gettime` reads the
GoOS realtime and monotonic clocks. `nanosleep` blocks on a timer deadline
through the syscall continuation and never ends early: a deadline that
passes before the monotonic clock has advanced by the request waits again
(ADR 0153). `prlimit64` reports
the descriptor table, stack, and `brk` bounds and accepts no change.

### Process families

The kernel keeps a bounded Linux family registry apart from the native process
registry (ADR 0154). Each record connects a compatibility PID to a
generation-safe GoOS Process ID and stores its parent, family identifier,
lifecycle state, exit status, and terminating signal.

Process exit releases the Linux address-space, descriptor, directory, and VFS
state before retaining a compact zombie record. The parent consumes that record
through the family reaper. A zombie keeps its PID reserved. Parent exit moves
running children to conceptual PID 1 and removes orphan zombies, so records
cannot leak after their Linux parent disappears.

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

A Linux process sees the GoOS VFS namespace: `/system`, `/tmp`, `/persist`,
`/home`, `/state`, and whatever else is mounted. Nothing is added for it.
`/proc` and `/sys` do not exist, and a program that opens `/proc/mounts`
gets `ENOENT`. Of `/dev` the kernel serves three names to Linux processes;
`/dev` itself is no directory.

### Descriptors and the terminal

`dup`, `dup2`, `dup3`, and `fcntl` rearrange descriptors over shared
descriptions (ADR 0146). `ioctl` answers `ENOTTY`: the standard streams are
byte streams, and no descriptor is a terminal even when the terminal service
is at the other end. libc therefore buffers output in blocks.

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

### Programs in the image

`/system/linux` holds six prebuilt static programs, pinned by checksum and
built by one offline recipe (ADR 0139, ADR 0148): `hello` with no libc,
`hello-pie`, the same linked position-independent, `musl-hello` for libc
startup, `fsprobe` and `dirprobe` for files and directories, and `abi-probe`,
the conformance suite. `userspace/linux/README.md` records the toolchain,
the checksums, and the Linux call trace.

`abi-probe` checks each result it gets and prints one line per group. PID 1
runs it at every validation boot and requires the whole output. The same
binary runs on a Linux kernel and must print the same lines, so an
expectation is wrong on Linux before it can mislead the kernel. One
reference run uses a container; a second boots a real Linux kernel in QEMU
and holds the probe to the exact Linux result in every check (ADR 0152). Its
other modes pass bad pointers and lengths to every call, exhaust the
descriptor table and the page quota, try the native call gate, touch an
I/O port, and recurse into the stack guard.

musl startup needs `arch_prctl`, `set_tid_address`, and `writev`. Its
terminal probe, `ioctl(TIOCGWINSZ)`, gets `ENOTTY`.

### Hardening

A pointer from a Linux program reaches kernel code only through the checked
copy helpers. A read or write across the end of a mapping is short, as on
Linux, and one that starts outside a mapping, or whose length leaves the
user half, is `EFAULT` (ADR 0147, ADR 0150). `mmap`
and `brk` refuse what the page quota cannot back, so exhaustion is `ENOMEM`
at the call. A full descriptor table is `EMFILE`. Ctrl-C ends a Linux process
through the native kill path: its continuation, its waits, its descriptors,
and its VFS session go with it, and no Linux signal is delivered.

## Scope

GoOS 0.2.0 runs static x86_64 programs, position-independent or not.
Dynamic linking, glibc, `fork`, `clone`, `execve`, threads, signal delivery,
pipes created by a Linux process, networking, `/proc`, termios, file-backed
`mmap`, and address-space randomization are outside it. BusyBox `sh` runs
builtins and redirections but cannot start a program, and `nano` does not
run. The next Linux milestone is process composition: `execve`,
`pipe2`, `wait4`, a chosen `fork` semantics, minimal signals, and `poll`.
