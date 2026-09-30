# Linux x86_64 ABI matrix

This document is the Linux compatibility contract of the current source: what
a static Linux x86_64 program can call, what each call does, and where the
answer differs from Linux. Release-specific contracts remain in
`docs/releases`. [linux-compat.md](linux-compat.md) describes how the layer is
built.

## Entry contract

- Process ABI must be `LinuxX8664`.
- Entry instruction is `SYSCALL`; native `int 0x80` is rejected.
- Arguments are `RDI`, `RSI`, `RDX`, `R10`, `R8`, and `R9`; RAX is the number.
- Success is non-negative RAX; failure is `-errno`.
- RCX and R11 are clobbered by the architectural entry.
- The user stack is never used as a kernel stack.
- Return uses IRETQ, not SYSRETQ.
- User GS is zero; `ARCH_SET_GS` is unsupported.

## Syscall status

"Partial" means the call works for the uses listed and refuses the rest with
an errno; nothing is accepted and ignored.

| Syscall | Status | Notes |
| --- | --- | --- |
| `read(0)` | implemented | streams block without polling; files read at their position, at most 64 KiB per call; a directory is `EISDIR` |
| `write(1)` | implemented | streams in bounded chunks with partial writes; files at their position or at the end for `O_APPEND`, at most 64 KiB per call |
| `open(2)` | implemented | `openat` from the working directory; musl's `open()` uses it |
| `close(3)` | implemented | frees the descriptor, then releases the stream handle, the VFS file, or the directory record |
| `stat(4)`, `fstat(5)`, `lstat(6)`, `newfstatat(262)` | implemented | synthetic mode, owner, and link count; `lstat` is `stat`; `newfstatat` takes `AT_EMPTY_PATH` |
| `lseek(8)` | implemented | files by `SEEK_SET`, `SEEK_CUR`, `SEEK_END`; streams are `ESPIPE`; a device stays at 0; no `SEEK_DATA` or `SEEK_HOLE` |
| `mmap(9)` | partial | anonymous private mappings at a kernel-chosen address; no fixed, file-backed, or executable mappings |
| `mprotect(10)` | partial | anonymous `PROT_NONE`, read-only, or read-write; executable protection refused |
| `munmap(11)` | implemented | page-aligned anonymous ranges; owned pages and quota are released |
| `brk(12)` | implemented | 64 MiB bounded heap after the highest load segment |
| `rt_sigaction(13)`, `rt_sigprocmask(14)` | partial | recorded signal state only: record and report dispositions and the blocked mask; no signal is delivered |
| `ioctl(16)` | implemented | `ENOTTY` for every request: no descriptor is a terminal |
| `pread64(17)`, `pwrite64(18)` | implemented | files at an offset, position unchanged; three VFS exchanges; streams are `ESPIPE` |
| `readv(19)`, `writev(20)` | implemented | streams and files; at most 16 elements and 64 KiB per call |
| `access(21)`, `faccessat(269)` | implemented | answers from the mode `stat` reports |
| `madvise(28)` | partial | `MADV_DONTNEED` on anonymous ranges |
| `dup(32)`, `dup2(33)`, `dup3(292)` | implemented | duplicates share position and status flags |
| `nanosleep(35)` | implemented | a timer deadline checked against the monotonic clock, never early; no signal interrupts it |
| `getpid(39)`, `getppid(110)`, `gettid(186)` | implemented | the compatibility PID; the parent is 1 |
| `execve(59)` | implemented | replaces the current image with a static Linux `ET_EXEC` or supported static PIE; no interpreter, shebang, PATH search, or native GoOS image |
| `exit(60)`, `exit_group(231)` | implemented | end the task, or every task of the process |
| `uname(63)` | implemented | `Linux`, `goos`, `6.0.0-goos`, the GoOS release, `x86_64` |
| `fcntl(72)` | partial | `F_DUPFD`, `F_DUPFD_CLOEXEC`, `F_GETFD`, `F_SETFD`, `F_GETFL`, `F_SETFL` for `O_APPEND`; others `EINVAL` |
| `getcwd(79)`, `chdir(80)` | implemented | the working directory; `chdir` requires an existing directory |
| `rename(82)`, `renameat(264)`, `renameat2(316)` | partial | files replace an existing target, directories need a free name; `renameat2` takes no flags |
| `mkdir(83)`, `mkdirat(258)` | implemented | the mode is ignored |
| `rmdir(84)`, `unlink(87)`, `unlinkat(263)` | implemented | a name some session holds open is `EBUSY` |
| `getuid(102)`, `getgid(104)`, `geteuid(107)`, `getegid(108)` | implemented | 1000 |
| `setuid(105)`, `setgid(106)` | partial | succeed for 1000, `EPERM` otherwise |
| `getgroups(115)` | implemented | no supplementary groups |
| `arch_prctl(158)` | partial | `ARCH_SET_FS` and `ARCH_GET_FS`; GS operations refused |
| `getdents64(217)` | implemented | one VFS exchange per entry; whole records only; no `.` or `..` |
| `set_tid_address(218)` | partial | stores the address, returns the compatibility PID; a single-task exit clears nothing |
| `clock_gettime(228)` | implemented | realtime and monotonic clocks and their variants; CPU-time clocks `EINVAL` |
| `openat(257)` | implemented | files, directories, `/dev/null`, and `/dev/zero`; `O_CREAT`, `O_EXCL`, `O_TRUNC`, `O_APPEND`, `O_DIRECTORY`, `O_CLOEXEC`; other flags `EOPNOTSUPP` |
| `prlimit64(302)` | partial | reports fixed limits of the caller; a new limit is `EPERM` |
| `getrandom(318)` | partial | bytes straight from the CPU's RDRAND, not a Linux entropy pool or CSPRNG; at most 256 per call; `ENOSYS` on a CPU without it |
| `statx(332)` | implemented | the status `stat` reports, as `struct statx`; `STATX_BTIME` when the filesystem has a creation time |
| every other number | `ENOSYS` | counted and recorded; nothing is faked |

Calls programs commonly make that stay `ENOSYS` in 0.2.0, with what musl or
BusyBox does about it:

| Syscall | Effect of `ENOSYS` |
| --- | --- |
| `kill(62)`, `tkill(200)`, `tgkill(234)`, `rt_sigreturn(15)`, `sigaltstack(131)` | no signal is sent or delivered; `raise` and `abort` see the refusal |
| `sendfile(40)` | BusyBox `cat` and `cp` fall back to `read` and `write` |
| `utimes(235)`, `futimesat(261)`, `utimensat(280)` | `touch` fails on an existing file: the VFS cannot set a time |
| `fork(57)`, `vfork(58)`, `clone(56)`, `wait4(61)` | no Linux process starts another |
| `pipe(22)`, `pipe2(293)`, `poll(7)`, `ppoll(271)`, `futex(202)` | no pipes, polling, or threads inside a process |
| `ftruncate(77)`, `fsync(74)`, `statfs(137)`, `preadv(295)`, `pwritev(296)` | not implemented |
| `socket(41)` and the network calls | no networking |

Wrong-ABI Linux entry returns `-EPERM`. A noncanonical or non-user return RIP
or RSP retires the task rather than attempting an unsafe IRETQ.

The declared planning namespace uses canonical x86_64 Linux numbers and is
separate from native kernel-call operations. It is bounded through `statx(332)`
and includes later candidates; membership claims neither implementation nor
0.2.0 release scope. The decoder snapshots RDI, RSI, RDX, R10, R8, and R9 as
scalars. Pointer interpretation remains the responsibility of a syscall handler
using checked copyin/copyout.

A blocking handler may persist one continuation per generation-safe Task ID.
Continuation storage is fixed at the 64 scheduler slots and rejects a second
active call, stale task generations, correlation mismatches, repeated issue or
advance, embedded NUL in copied path data, and paths above `PathMax`. Blocking
clears the runnable bit and switches tasks; wake resumes the exact stage without
busy-waiting. Lifecycle and kill paths remove the continuation before a task
slot can be reused.

`writev` copies the vector, then checks every element before it writes: a
length negative as `ssize_t` is `EINVAL`, and a non-empty element outside user
memory is `EFAULT`. An element that stalls after earlier bytes went out ends
the call with the count so far.

Each Linux process has a compatibility PID between 2 and 32767, allocated at
launch and distinct among live Linux processes. It is the only process or
thread identity a Linux program sees.

## VFS session

File calls translate to requests on the one VFS channel PID 1 granted. The
bridge numbers requests per session, blocks the task through its continuation,
and wakes it on a reply, on peer death, or after eight seconds.

| Event | Result |
| --- | --- |
| reply with the pending correlation and operation | the call completes with the translated VFS status |
| reply to an earlier, abandoned call | dropped; the wait continues |
| any other message on the session | `EIO`; attached handles are closed |
| VFS peer death | `EIO`; the session stays failed |
| deadline | `ETIMEDOUT` |
| process without the session handle or its rights | `EIO` |

## Paths

A path resolves in the GoOS VFS namespace from the process's working
directory, which the launch supplies. The resolver removes empty, `.`, and
`..` components in one bounded pass and produces an absolute path of at most
4096 bytes with components of at most 1024.

| Input | Result |
| --- | --- |
| unreadable path pointer | `EFAULT` |
| empty path | `ENOENT` |
| no terminator within 4096 bytes | `ENAMETOOLONG` |
| component over 1024 bytes, or result over 4096 bytes | `ENAMETOOLONG` |
| relative path, `dirfd` open but not a directory | `ENOTDIR` |
| relative path, `dirfd` not open | `EBADF` |
| path ending in `/`, `.`, or `..` that names a regular file | `ENOTDIR` |

A resolved path over 216 bytes reaches the VFS in a memory object attached to
the request.

## Image replacement

`execve(path, argv, envp)` resolves `path` from the stored cwd and accepts only
static x86_64 ELF classes supported at launch. Empty paths are `ENOENT`.
Missing files preserve the VFS error; native images, interpreters, malformed
ELF, and rejected layouts are `ENOEXEC`.

The pointer vectors may be null, which supplies an empty vector. Otherwise the
kernel reads each 64-bit pointer through the old root until a null pointer. The
limits are 256 arguments, 256 environment entries, 32 KiB for one string, and
64 KiB for all copied strings and terminators. An unreadable vector, pointer, or
string is `EFAULT`. A vector, string, aggregate, or final stack above its bound
is `E2BIG`. Raw environment entries need no equals sign.

Success keeps PID, parentage, cwd, process handles, quotas, and every descriptor
without `FD_CLOEXEC`. Retained descriptors keep their shared description and
position. Final VFS descriptions selected by `FD_CLOEXEC` are closed through
the family VFS context; a close failure does not roll back the replacement.
The call discards the old image, stack, brk, anonymous mappings, and FS base.

Caught signal dispositions reset to default. Ignored dispositions and the
blocked mask remain. No pending signal is carried across the call because the
current signal model never queues one. The process name becomes the path's
basename. `AT_EXECFN` contains the requested path.

## Files

A file call is one or more exchanges on the VFS session (ADR 0141). An open
stats the path, creates or truncates when the flags ask, and opens:

| Flags and target | Result |
| --- | --- |
| missing, no `O_CREAT` | `ENOENT` |
| missing, `O_CREAT` | the file is created and opened |
| existing, `O_CREAT\|O_EXCL` | `EEXIST` |
| regular file, `O_TRUNC` | the file is emptied and opened |
| regular file, `O_DIRECTORY` or a trailing slash | `ENOTDIR` |
| directory, read-only | a directory descriptor |
| directory with write access, `O_CREAT`, or `O_TRUNC` | `EISDIR` |
| `O_DIRECTORY\|O_CREAT`, or access mode 3 | `EINVAL` |
| any flag outside the accepted set | `EOPNOTSUPP` |

`O_LARGEFILE`, `O_NOCTTY`, and `O_NOFOLLOW` are accepted and change nothing.
A full descriptor table is `EMFILE`; a full VFS open-file table or directory
record table is `ENFILE`; a refused write open is `EROFS`.

A descriptor names a stream, a VFS file, or a directory. Duplicated
descriptors share one description with its position and status flags. A
directory description keeps its canonical path; 32 exist system-wide. `*at`
calls resolve a relative path from the directory their `dirfd` names. A
fourth kind of descriptor names a device; see [Devices](#devices).

File bytes move through one 64 KiB memory object per process, charged to the
process. A read or write of more is a short transfer. Reading a write-only
descriptor or writing a read-only one is `EBADF`. A write through `O_APPEND`
asks the VFS for the file length, seeks there, and writes; it is not atomic
against another writer.

| Call | Errors beyond path resolution |
| --- | --- |
| `lseek` | `EINVAL` for a negative result or an unknown whence, `ESPIPE` for a stream |
| `pread64`, `pwrite64` | `EINVAL` for a negative offset, checked before the descriptor; `ESPIPE` for a stream; `EISDIR` reading and `EBADF` writing a directory |
| `getcwd` | `ERANGE` for a buffer shorter than the path and its terminator |
| `chdir` | `ENOENT`, `ENOTDIR` |
| `mkdir` | `EEXIST`, `ENOENT`, `ENOTDIR`, `EROFS`, `ENOSPC` |
| `unlink` | `EISDIR` for a directory, `EBUSY` for an open file or a mount point |
| `rmdir` | `ENOTDIR` for a file, `ENOTEMPTY`, `EBUSY` for a mount point |
| `rename` | `EISDIR` for a file onto a directory, `EEXIST` for a directory onto an existing name, `EBUSY` for an open target, `EXDEV` across mounts |

`pread64` and `pwrite64` move the VFS position to the offset, transfer, and
move it back to where the description has it, so the position a program sees
never changes (ADR 0150). A transfer that fails still restores it. A
descriptor opened with `O_APPEND` writes at the end of the file whatever
offset `pwrite64` names, which is the Linux behavior.

`ftruncate` and `fsync` return `ENOSYS`.

## Devices

The kernel serves three device names itself, with no VFS exchange
(ADR 0150). It recognizes them after path resolution, so `/dev/../dev/null`
and a relative path that resolves there name the same device.

| Path | `open` | `read` | `write` | Device number |
| --- | --- | --- | --- | --- |
| `/dev/null` | a descriptor | 0, end of file | the count, bytes discarded | 1, 3 |
| `/dev/zero` | a descriptor | zero bytes, at most 64 KiB per call | the count, bytes discarded | 1, 5 |
| `/dev/tty` | `ENXIO`: the process has no controlling terminal | | | 5, 0 |

`stat`, `fstat`, and `statx` report `S_IFCHR\|0666` with the device number in
`st_rdev`, and `access` answers from that mode. `readv`, `writev`, `pread64`,
and `pwrite64` work as `read` and `write` do; a write never reads the memory
it is given, so the buffer need not be mapped. `lseek` returns 0 and `ioctl`
`ENOTTY`.

| Call on a device name | Result |
| --- | --- |
| `open` with `O_DIRECTORY` or a trailing slash, a path beneath the name | `ENOTDIR` |
| `open` with `O_CREAT\|O_EXCL` | `EEXIST` |
| `unlink`, `rename` from or onto it | `EACCES` |
| `mkdir` | `EEXIST` |
| `rmdir`, `chdir` | `ENOTDIR` |

`/dev` is no directory: `stat("/dev")` is `ENOENT` and no listing shows the
three names. Native programs do not see them.

## Directory listing

`getdents64` fills the buffer with whole `linux_dirent64` records, one VFS
exchange per entry (ADR 0143). `d_ino` is the identity `st_ino` reports,
`d_off` the position after the entry, `d_type` `DT_REG` or `DT_DIR`. The
position lives in the directory's description: `lseek` to 0 rewinds and to a
`d_off` resumes. A name over 152 bytes comes back through the process's I/O
memory object. GoOS directories have no `.` and `..`, and the listing has
none.

A buffer too small for the next record is `EINVAL` when nothing was written
and ends the call otherwise. A descriptor that is not a directory is
`ENOTDIR`. One call runs at most 128 exchanges and may return fewer records
than the buffer holds.

## File status

`stat`, `lstat`, `fstat`, and `newfstatat` fill a 144-byte `struct stat`
written out from the Linux x86_64 header (ADR 0142). `statx` fills the
256-byte `struct statx` from the same answer (ADR 0150).

| Field | Value |
| --- | --- |
| `st_mode` | `S_IFREG\|0644` or `S_IFDIR\|0755`; no write bits for a read-only file or mount; `S_IFIFO\|0600` for a stream; `S_IFCHR\|0666` for a device |
| `st_rdev` | the device number of a device, zero otherwise |
| `st_uid`, `st_gid` | 1000 |
| `st_nlink` | 1 |
| `st_dev`, `st_ino` | the VFS mount identity and node identity; equal for two names of one file |
| `st_size`, `st_blocks`, `st_blksize` | the VFS size, that size in 512-byte units rounded up, 4096 |
| `st_atime`, `st_mtime`, `st_ctime` | one time: modification, else creation, else zero |

A file descriptor is stat-ed through its VFS handle, a directory descriptor by
the path it keeps, a stream or a device without the VFS. `fstat` of a closed
descriptor is `EBADF`. A path that names a file and ends in a slash is
`ENOTDIR`. An unwritable buffer is `EFAULT`, reported after the path errors
as on Linux. An unknown `newfstatat` flag is `EINVAL`.

`statx` differs in three ways. `stx_mask` is `STATX_BASIC_STATS`, with
`STATX_BTIME` and `stx_btime` when the filesystem recorded a creation time;
the requested mask changes nothing. `stx_dev_major` and `stx_dev_minor` are
the halves `major()` and `minor()` give for `st_dev`. `AT_STATX_FORCE_SYNC`
and `AT_STATX_DONT_SYNC` are accepted; both at once, a reserved mask bit, or
an unknown flag is `EINVAL`. `statx` takes a null path with `AT_EMPTY_PATH`,
and `newfstatat` does when that is its only flag, as Linux 6.11 and later do.

## Vector I/O

`readv` and `writev` take at most 16 elements and move at most 64 KiB per
call (ADR 0144). The kernel copies the element array into fixed storage and
checks every element before a byte moves: a count over 16 or a negative
length is `EINVAL`, an element outside user memory `EFAULT`. Elements are
used in order and empty ones skipped.

A stream `readv` or `read` delivers what the stream holds, up to the request,
and blocks only while it has nothing. A stream `writev` or `write` returns
the count so far when the stream fills. On a file each is one VFS read or
write through the process's I/O memory object. musl stdio uses both calls.

## Identity, time, and limits

These calls answer from kernel state and ask no service (ADR 0145).

| Call | Answer |
| --- | --- |
| `getpid`, `gettid` | the compatibility PID, 2 to 32767 |
| `getppid` | 1 |
| `getuid`, `geteuid`, `getgid`, `getegid` | 1000 |
| `setuid`, `setgid` | succeed for 1000, `EPERM` for any other value |
| `getgroups` | no supplementary groups |
| `uname` | `Linux`, `goos`, `6.0.0-goos`, `GoOS <release> Linux ABI`, `x86_64`, `(none)` |
| `clock_gettime` | clocks 0 and 5 read realtime; 1, 4, 6, and 7 read monotonic; others `EINVAL` |
| `nanosleep` | sleeps whole ticks, rounded up plus one, and again while the monotonic clock has not advanced by the request; returns 0; bad fields `EINVAL` |
| `prlimit64` | `RLIMIT_NOFILE` 64, `RLIMIT_STACK` 8 MiB, `RLIMIT_DATA` 64 MiB, `RLIMIT_CORE` 0, others unlimited |

`prlimit64` refuses a new limit with `EPERM`, another PID with `ESRCH`, and a
resource over 15 with `EINVAL`. No signal exists to interrupt a sleep.

## Recorded signal state

`rt_sigaction` and `rt_sigprocmask` keep what a program registers and report
it back (ADR 0151). GoOS sends no signal, so no handler runs, no call returns
`EINTR`, and a blocked signal is never pending.

| Call | Behavior |
| --- | --- |
| `rt_sigaction` | stores the 32-byte kernel `sigaction` for signals 1 to 64 and returns the previous one; `SIGKILL` and `SIGSTOP` are removed from the stored mask |
| `rt_sigprocmask` | `SIG_BLOCK`, `SIG_UNBLOCK`, `SIG_SETMASK` on a 64-bit mask; `SIGKILL` and `SIGSTOP` never enter it; a call with no new set reports the mask whatever the operation |
| either, set size other than 8 | `EINVAL` |
| `rt_sigaction` for signal 0 or above 64, or a new disposition for `SIGKILL` or `SIGSTOP` | `EINVAL` |
| `rt_sigprocmask` with an unknown operation and a new set | `EINVAL` |
| an unreadable or unwritable pointer | `EFAULT` |

Ctrl-C ends the process through the native kill path whatever it registered
for `SIGINT`. The calls that send a signal stay `ENOSYS`.

## Random bytes

`getrandom` and `AT_RANDOM` have one source, the CPU's RDRAND generator
(ADR 0151). This is a direct hardware source, not the Linux entropy pool or
kernel CSPRNG: the kernel keeps no pool, mixes nothing, and reseeds nothing.
A program that takes a working `getrandom` for that abstraction gets the
quality of the CPU's generator, and a transient generator failure is `EIO`.

| CPU | `getrandom` | `AT_RANDOM` |
| --- | --- | --- |
| has RDRAND | fills up to 256 bytes per call; a longer request returns 256 | 16 bytes, drawn at launch |
| has none | `ENOSYS`, counted with the refused calls | absent |

`GRND_NONBLOCK`, `GRND_RANDOM`, and `GRND_INSECURE` are accepted and change
nothing. An unknown flag, or `GRND_RANDOM` with `GRND_INSECURE`, is `EINVAL`.
A buffer that runs off a mapping gets the bytes that fit; one that starts
outside any is `EFAULT`. The kernel keeps no pool and no copy. QEMU's default
CPU model has no RDRAND; `-cpu qemu64,+rdrand` adds it.

## ELF status

The pure parser accepts ELF64 little-endian x86_64 images of two types:
`ET_EXEC`, at the addresses the file names, and `ET_DYN` with no interpreter,
a static position-independent executable (ADR 0149). It bounds program
headers at 64 and `PT_LOAD` segments at 16, produces page-rounded load
metadata, and validates source ranges, user ranges, configured reserved pages,
alignment/congruence, overlap, protections, and the executable entry.

`PT_PHDR`, `PT_TLS`, `PT_GNU_STACK`, and `PT_GNU_RELRO` are recorded and checked
against their containing loads. `PT_NOTE`, `PT_GNU_EH_FRAME`, and
`PT_GNU_PROPERTY` must name a range inside the file and are otherwise ignored.
`PT_INTERP`, executable stack, W+X, every other program-header kind, and
malformed arithmetic are rejected. Parsing itself does not map or execute
images.

A static PIE loads at the fixed base `0x40000000`. The parser adds the base
to the entry and to every address a header names, then applies the checks
above to the moved addresses. `PT_DYNAMIC` is accepted in such an image, once
and inside a load segment, and refused in an `ET_EXEC` image. The kernel
applies no relocation: the program's own startup code does, from `AT_PHDR`.
The base is the same on every launch; GoOS randomizes no layout.

## Mapping status

Validated loads become pinned image regions backed by the existing stable
executable source. Each region retains the page-aligned virtual range, aligned
file offset, file-backed byte count, final protection, and source identity.
The shared page-fault path reads file bytes on first touch and supplies zeroes
after `p_filesz`; its existing quota accounting and W^X checks remain in force.

The mapping boundary rechecks source length, range arithmetic, overlap, entry
placement, and protections. Linux `ET_EXEC` may start at `0x400000` and a
static PIE starts at `0x40000000`; load pages
must remain below 2 GiB, away from fixed GoOS user windows. The Linux stack is
reserved as 8 MiB RW/NX demand-zero memory below `vm.UserStackTop`, with a
64 KiB guard. PID 1 launch uses this mapping directly.

## Initial stack

The bounded builder emits the Linux x86_64 entry layout:

```text
argc
argv pointers, NULL
envp pointers, NULL
auxv type/value pairs, AT_NULL
NUL-terminated strings
```

Initial RSP is 16-byte aligned. The complete occupied image is limited to
64 KiB, with at most 256 arguments and 256 environment entries. It contains
`AT_PHDR`, `AT_PHENT`, `AT_PHNUM`, `AT_PAGESZ`, `AT_ENTRY`, `AT_EXECFN`,
`AT_UID`, `AT_EUID`, `AT_GID`, `AT_EGID`, `AT_SECURE`, `AT_CLKTCK`,
`AT_PLATFORM="x86_64"`, and `AT_NULL`. UID/GID are 1000 and `AT_CLKTCK` is
100. `AT_PHDR` and `AT_ENTRY` are the loaded addresses, base included.
`AT_RANDOM` points at 16 bytes from RDRAND when the CPU has it and is absent
otherwise; no deterministic bytes are presented as entropy.

The installer maps only pages occupied by this image as user RW/NX owned
pages. Normal launch accounting will charge them before the task becomes
runnable. The remaining 8 MiB reservation stays absent and demand-zero. The
builder and installer are used for every accepted Linux launch.

The compatibility record also owns a 64 MiB `brk` interval beginning at the
first page after the highest load segment. Anonymous `mmap` uses deterministic
first-fit addresses in the ordinary kernel-selected mapping window. Both paths
reuse the native region table, demand-zero faults, physical-page quota, unmap,
discard, and protection machinery. Anonymous executable mappings, W+X,
file-backed mappings, fixed mappings, and unsupported flags fail explicitly.

FS base is saved per generation-safe scheduler slot and restored with the FPU
and address-space context on every switch. `ARCH_SET_FS` accepts only lower-half
user addresses; `ARCH_GET_FS` copies the saved value through checked user
memory. Task installation and slot reclamation clear the saved base. GS remains
owned by the syscall entry mechanism.

## Launch status

PID 1 classifies `GOOSNAT\0` and ELF magic and carries that candidate in
lifecycle protocol version 10. The kernel re-reads the stable executable source
and refuses a mismatch before selecting the native or Linux loader. A validated
Linux ELF creates a `LinuxX8664` Process through the common accounting,
executable-lifetime, task, wait, and teardown path.

The only accepted grants are `in` with Read/Wait, `stdout` and `stderr` with
Write/Wait, and one fresh `vfs` channel with Read/Write/Wait. The profile must
be `large`; extra grants, device authority, launch authority, and weaker
profiles are refused. Handle IDs and cwd stay in a bounded kernel-side
compatibility record. Linux user memory receives only the System V stack.

## Descriptors

`dup`, `dup2`, `dup3`, and the two `fcntl` duplication commands add a
descriptor for an existing description (ADR 0146). Duplicates share the file
position and status flags and keep separate close-on-exec flags. `dup2` onto
an open descriptor closes it first. The table holds 64 descriptors; the next
is `EMFILE`.

`fcntl(F_GETFL)` reports the access mode, `O_APPEND`, and `O_LARGEFILE` for a
file, adds `O_DIRECTORY` for a directory, and reports the direction alone for
a stream. `F_SETFL` changes `O_APPEND` on a file and refuses `O_NONBLOCK`,
`O_ASYNC`, `O_DIRECT`, and `O_NOATIME` with `EINVAL`.

`ioctl` returns `ENOTTY` on an open descriptor and `EBADF` on a closed one.
The standard streams are FIFOs to `fstat` and no terminal to `ioctl`, whatever
is at their other end. musl's `isatty` is false and its stdio buffers output
until the buffer fills or the program exits.

## Memory and pointers

`mmap` and `brk` refuse, with `ENOMEM` and an unchanged break, a request
larger than the page quota has left (ADR 0147). The quota of a Linux process
is 16384 pages, 64 MiB. Anonymous memory is never executable: `PROT_EXEC` is
`EINVAL` at `mmap` and at `mprotect`.

Every pointer argument is copied through the checked user-memory helpers. A
null, noncanonical, kernel-half, or unmapped pointer is `EFAULT`. A read or
write whose buffer runs off the end of a mapping moves the bytes before the
end and reports them; one whose buffer starts outside any mapping is
`EFAULT`. So is one whose length carries the range past the user half,
before any byte moves: Linux checks the range a call names first. A zero
length succeeds without touching the pointer. A call whose
argument lies in a page of the program's own image that is still loading from
its file waits for the page and runs again.

## Differences from Linux

Results a correct Linux program can observe to differ:

| Area | GoOS 0.2.0 | Linux |
| --- | --- | --- |
| terminal | no descriptor is a terminal; `ioctl` is `ENOTTY` | a terminal answers `TIOCGWINSZ` and termios |
| signals | dispositions and the mask are recorded; none is sent or delivered; Ctrl-C ends the process whatever handles `SIGINT`; `nanosleep` never returns `EINTR` | delivered, handled, masked |
| devices | `/dev/null`, `/dev/zero`, and `/dev/tty` only; `/dev` is no directory | a populated `/dev` |
| random bytes | `ENOSYS` and no `AT_RANDOM` on a CPU without RDRAND; 256 bytes per call | always available; 32 MiB per call |
| positioned I/O | three VFS exchanges per call | one atomic operation |
| load address | a static PIE always loads at `0x40000000` | randomized |
| users | every file and process is 1000:1000; mode bits are presentation | enforced ownership and permissions |
| directory listing | no `.` and `..`; a directory removed while open lists as empty | both entries; `ENOENT` |
| link count | 1 for files and directories | directories count their subdirectories |
| times | one time per file for all three fields | three times |
| open files | removing or replacing a file some session holds open is `EBUSY` | allowed; the file lives until closed |
| rename | a directory moves only onto a free name | an empty directory may be replaced |
| append | three exchanges; two writers can interleave | atomic |
| path text | `missing/../file` opens `file` | `ENOENT` |
| vectors | at most 16 elements | 1024 |
| transfers | at most 64 KiB per call, then a short count | 2 GiB |
| memory | no executable anonymous memory, no fixed or file-backed mappings | all three |
| limits | 64 descriptors, 64 MiB of pages, fixed | adjustable |
| open flags | `O_NONBLOCK` and the others outside the accepted set are `EOPNOTSUPP` | accepted |
| name length | 1024 bytes per component | 255 |

## Release limits

GoOS 0.2.0 runs x86_64, statically linked ELF programs, position-independent
or not. It has no `PT_INTERP`, dynamic linker, or glibc; no `fork`, `vfork`,
`clone`, or `execve`; no threads or `futex`; no signal delivery; no pipes
created by a Linux process; no networking; no `/proc` or `/sys`, and of
`/dev` three names; no file-backed `mmap`; no address-space randomization; no
ownership or permission enforcement; no root; no termios. BusyBox `sh` runs
builtins and redirections but cannot run a program. `nano` does not run.

## Bounds

| Resource | Bound |
| --- | ---: |
| ELF program headers | 64 |
| `PT_LOAD` segments | 16 |
| descriptors per process | 64 |
| directory descriptions, all processes | 32 |
| path | 4096 bytes |
| path component | 1024 bytes |
| initial stack image | 64 KiB, 256 arguments, 256 environment entries |
| one `execve` string | 32 KiB |
| copied `execve` strings | 64 KiB including terminators |
| concurrent `execve` preparation | 4 system-wide |
| stack reservation | 8 MiB, 64 KiB guard |
| static PIE load base | `0x40000000` |
| `brk` heap | 64 MiB |
| page quota | 16384 pages |
| continuations | 1 per task |
| VFS exchanges per `getdents64` | 128 |
| vector elements | 16 |
| bytes per read or write | 64 KiB |
| bytes per `getrandom` | 256 |
| signal dispositions per process | 64 |
| wait for a VFS reply | 8 seconds |
| compatibility PIDs | 2 to 32767 |
| refused-call record | 8 recent numbers and a 512-bit set |

## Diagnostics

The kernel counts Linux call entries, refused calls, calls that ended in
`EFAULT` with the number of the latest, and VFS bridge requests, failures,
and dropped late replies. It keeps the eight most recently refused call
numbers and the set of every number ever refused. A diagnostic build prints them as the `DIAG linux` line
of the `diag` dump. A normal build logs no Linux call.

## Validation

**Kernel self-test.** A synthetic `LinuxX8664` task runs two entry and return
cycles and checks `-ENOSYS`, IF restoration, DF clearing, and native-gate
refusal; an invalid return RSP must retire the task; a native task must be
refused at the Linux gate. Markers: `LINUX_SYSCALL_OK`,
`LINUX_ABI_ISOLATION_OK`, `LINUX_RETURN_FRAME_GUARD_OK`.

**PID 1 probes.** `/system/linux/raw-probe` is an ELF assembled byte by byte.
PID 1 proves that a false format hint, a smaller profile, and an extra grant
are refused, then runs two instances at once. Each moves `brk`, maps,
reprotects, discards, and unmaps memory, and installs its own FS base; both
block, resume, and check FS-relative memory. Markers: `LINUX_PID1_LAUNCH_OK`,
`LINUX_RAW_SYSCALL_OK`, `LINUX_MEMORY_TLS_OK`.

PID 1 then runs `/system/linux/abi-probe one two` in `/system/linux`. The
probe checks every result itself and prints one line per group: arguments
and environment, TLS and heaps, the VFS session, path resolution, file I/O,
file status, `statx`, positioned I/O, stdio, directory listing, vectors,
descriptors, naming, the devices, identity, clocks, limits, and sleep, the
signal state, and random bytes. PID 1 requires the whole output and exit
status zero, and prints the output it got between `LINUX_PROBE_OUTPUT_BEGIN`
and `LINUX_PROBE_OUTPUT_END` when it differs. Markers: `LINUX_MUSL_OK`, `LINUX_VFS_SESSION_OK`,
`LINUX_PATH_OK`, `LINUX_FILE_IO_OK`, `LINUX_STAT_OK`, `LINUX_DIRENT_OK`,
`LINUX_SYSTEM_OK`.

**Reference.** The same `abi-probe` binary prints the same lines on a Linux
kernel. `make test-linux-reference` runs it in an amd64 container, under
user-mode emulation on an arm64 host. `make test-linux-kernel-reference
KERNEL=path BUSYBOX=path` boots a real Linux kernel in QEMU and runs the
default and hostile modes strict: every check must give the exact Linux
result, which GoOS is always held to (ADR 0152). Both modes pass on Linux
6.12. `userspace/linux/README.md` holds the recorded call trace and the
results that differ.

**Linux boot.** `make test-linux-boot` runs from the shell of one validation
boot:

| Step | Requires |
| --- | --- |
| `hello` | `hello from Linux ABI`, status 0 |
| `hello-pie` | `hello from a static PIE`, printed only when `AT_PHDR` and `AT_ENTRY` match the address the program runs at |
| `musl-hello alpha beta` | `argc=3`, `hello from musl` |
| `fsprobe` on `/tmp` and on `/persist` | `write ok`, `read ok`, `stat ok`; native `cat` reads the file |
| `dirprobe` on `/tmp` and on `/persist` | every name, directories marked, the count |
| `musl-hello` and `abi-probe hostile` copied to `/persist` and run from there | the same results from an image paged in from FAT32 |
| Linux program on each side of a native pipeline | the bytes arrive |
| two `abi-probe pair` instances in one pipeline | both keep their own descriptors, working directory, TLS, heap, and files; different PIDs |
| `abi-probe hostile` | `EFAULT` for bad pointers in every call, short transfers at a mapping's end, 100000-byte transfers, stale descriptors |
| `abi-probe exhaust` | `EMFILE` at 64 descriptors, `ENOMEM` at the page quota, and recovery from both |
| `abi-probe isolation` | the native call gate refuses all 128 call numbers; native handle values are `EBADF` |
| `abi-probe random` | `random: hardware`: the guest CPU of this test has RDRAND |
| `abi-probe stack-overflow` | recursion reaches the guard below the stack; the kernel logs `user fault: guard page`, the process ends, the shell continues |
| `abi-probe port-io` | the process ends with a fault status and the shell continues |
| Ctrl-C on a blocked read, a sleep, and a VFS wait | the process ends, the shell reads the next line, later runs pass |

**General boot test.** `make test-boot` also restarts the VFS under a Linux
process, which must then get `EIO` and no new session, and makes the VFS
answer one request late, which must end in `ETIMEDOUT` with the session still
usable. Its guest CPU has no RDRAND, so PID 1's probe there requires
`getrandom` to return `ENOSYS` and `AT_RANDOM` to be absent.

**BusyBox.** `make test-linux-busybox BUSYBOX=path` requires `echo`, `pwd`,
`cat`, `ls`, `mkdir`, and `rm` of a static BusyBox the caller supplies
(ADR 0148). With BusyBox 1.35.0 built against musl these also run: `uname`,
`date`, `sleep`, `cp`, `mv`, `head`, `tail`, `wc`, `stat`, `ls -l`, `rmdir`,
`true`, `basename`, `env`, `id`, `md5sum`, `dd if=/dev/zero`, `sh -c true`,
and `sh -c "echo x > /dev/null"`. `touch` fails on the missing `utimensat`
and `df` on the missing `/proc/mounts`. Alpine 3.22's static BusyBox 1.37,
which carries `PT_NOTE` and `PT_GNU_PROPERTY` headers, runs the same set.

**Host tests.** `make test-linux-elf` parses hostile and real images and
checks every fixture against its checksum and the kernel ELF parser.
`make test-linux-abi` covers the pure translation code: reply
classification, session state, open planning, seek arithmetic, path
resolution, the `struct stat`, `struct statx`, and `linux_dirent64` layouts,
vectors, status flags, clocks, sleep rounding, limits, PIDs, the descriptor
table, the device names, the signal state, and the random flags. Fuzz targets
cover the ELF parser for both image types, the initial stack, path
resolution, and both record encoders.

## Timing baseline

`make test-linux-bench` writes `build/reports/linux-abi-<variant>.md`.
`QEMU_ACCEL` names the accelerator, `tcg` by default; an x86_64 host can pass
`kvm` and time the guest on its own CPU. The development host is an arm64
machine, which can only translate an x86_64 guest. There, under QEMU TCG at
100 Hz and `-opt=2`, the lowest averages of three runs were:

| Linux call | ns per call |
| --- | ---: |
| `getpid` | 25661 |
| `clock_gettime` | 32715 |
| `write` of 64 bytes to a pipe | 173662 |
| `read` of 4 KiB from a file | 749878 |

In the same boot a native VFS round trip took 212903 ns and a native
64-byte stream call 62757 ns. Under TCG every entry pays two address-space
switches in translated code, which is most of the `getpid` time. The numbers
are a baseline for comparing changes on one host.
