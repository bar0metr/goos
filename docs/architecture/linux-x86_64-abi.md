# Linux x86_64 ABI matrix

This document records the implemented GoOS 0.2.0 Linux compatibility contract.
It describes current code, not the final release target.

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

| Syscall | Status | Notes |
| --- | --- | --- |
| `read(0)` | implemented | streams block without polling; files read at their position, at most 64 KiB per call; a directory is `EISDIR` |
| `write(1)` | implemented | streams in bounded chunks with partial writes; files at their position or at the end for `O_APPEND`, at most 64 KiB per call |
| `open(2)` | implemented | `openat` from the working directory; musl's `open()` uses it |
| `close(3)` | implemented | frees the descriptor, then releases the stream handle, the VFS file, or the directory record |
| `stat(4)`, `fstat(5)`, `lstat(6)`, `newfstatat(262)` | implemented | synthetic mode, owner, and link count; `lstat` is `stat`; `newfstatat` takes `AT_EMPTY_PATH` |
| `lseek(8)` | implemented | files by `SEEK_SET`, `SEEK_CUR`, `SEEK_END`; streams are `ESPIPE`; no `SEEK_DATA` or `SEEK_HOLE` |
| `mmap(9)` | partial | kernel-chosen anonymous private mappings; no executable or file-backed mappings |
| `mprotect(10)` | partial | anonymous `PROT_NONE`, read-only, or read-write; executable protection rejected |
| `munmap(11)` | implemented | page-aligned anonymous ranges; owned pages and quota are released |
| `brk(12)` | implemented | 64 MiB bounded heap after the highest load segment |
| `readv(19)` | implemented | streams and files; at most 16 elements; one read fills the elements in order |
| `writev(20)` | implemented | streams and files; at most 16 elements and 64 KiB per call, short count when the stream fills |
| `madvise(28)` | partial | `MADV_DONTNEED` on anonymous ranges |
| `arch_prctl(158)` | partial | `ARCH_SET_FS` and `ARCH_GET_FS`; GS operations rejected |
| `exit(60)` | implemented | exits the calling task |
| `set_tid_address(218)` | implemented | stores the address, returns the compatibility PID; a single-task exit clears nothing |
| `exit_group(231)` | implemented | retires every task in the process |
| `getcwd(79)`, `chdir(80)` | implemented | the working directory; `chdir` requires an existing directory |
| `rename(82)`, `renameat(264)`, `renameat2(316)` | partial | files replace an existing target, directories need a free name; `renameat2` takes no flags |
| `mkdir(83)`, `mkdirat(258)` | implemented | the mode is ignored |
| `rmdir(84)`, `unlink(87)`, `unlinkat(263)` | implemented | a name some session holds open is `EBUSY` |
| `openat(257)` | implemented | files and directories; `O_CREAT`, `O_EXCL`, `O_TRUNC`, `O_APPEND`, `O_DIRECTORY`, `O_CLOEXEC`; other flags `EOPNOTSUPP` |
| `ioctl(16)` | `ENOSYS` | musl reads the failure as "no terminal" |
| other declared numbers | `ENOSYS` | decoded and typed; no fake success |
| every other number | `ENOSYS` | bounded diagnostics; no fake success |

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
calls resolve a relative path from the directory their `dirfd` names.

File bytes move through one 64 KiB memory object per process, charged to the
process. A read or write of more is a short transfer. Reading a write-only
descriptor or writing a read-only one is `EBADF`. A write through `O_APPEND`
asks the VFS for the file length, seeks there, and writes; it is not atomic
against another writer.

| Call | Errors beyond path resolution |
| --- | --- |
| `lseek` | `EINVAL` for a negative result or an unknown whence, `ESPIPE` for a stream |
| `getcwd` | `ERANGE` for a buffer shorter than the path and its terminator |
| `chdir` | `ENOENT`, `ENOTDIR` |
| `mkdir` | `EEXIST`, `ENOENT`, `ENOTDIR`, `EROFS`, `ENOSPC` |
| `unlink` | `EISDIR` for a directory, `EBUSY` for an open file or a mount point |
| `rmdir` | `ENOTDIR` for a file, `ENOTEMPTY`, `EBUSY` for a mount point |
| `rename` | `EISDIR` for a file onto a directory, `EEXIST` for a directory onto an existing name, `EBUSY` for an open target, `EXDEV` across mounts |

`getdents64`, `ftruncate`, `fsync`, and `statx` return `ENOSYS`.

## File status

`stat`, `lstat`, `fstat`, and `newfstatat` fill a 144-byte `struct stat`
written out from the Linux x86_64 header (ADR 0142).

| Field | Value |
| --- | --- |
| `st_mode` | `S_IFREG\|0644` or `S_IFDIR\|0755`; no write bits for a read-only file or mount; `S_IFIFO\|0600` for a stream |
| `st_uid`, `st_gid` | 1000 |
| `st_nlink` | 1 |
| `st_dev`, `st_ino` | the VFS mount identity and node identity; equal for two names of one file |
| `st_size`, `st_blocks`, `st_blksize` | the VFS size, that size in 512-byte units rounded up, 4096 |
| `st_atime`, `st_mtime`, `st_ctime` | one time: modification, else creation, else zero |

A file descriptor is stat-ed through its VFS handle, a directory descriptor by
the path it keeps, a stream without the VFS. `fstat` of a closed descriptor is
`EBADF`. A path that names a file and ends in a slash is `ENOTDIR`. An
unwritable buffer is `EFAULT`, checked before the VFS is asked. An unknown
`newfstatat` flag is `EINVAL`.

`readv` reads once and fills the vector's elements in order. Every element is
checked for writability before any byte leaves its source. A stream delivers
at most 256 bytes per call and a file at most 64 KiB; musl stdio reads a file
through it.

## ELF status

The pure parser accepts only ELF64 little-endian x86_64 `ET_EXEC`. It bounds
program headers at 64 and `PT_LOAD` segments at 16, produces page-rounded load
metadata, and validates source ranges, user ranges, configured reserved pages,
alignment/congruence, overlap, protections, and the executable entry.

`PT_PHDR`, `PT_TLS`, `PT_GNU_STACK`, and `PT_GNU_RELRO` are recorded and checked
against their containing loads. `PT_INTERP`, `PT_DYNAMIC`, executable stack,
W+X, unknown program-header kinds, and malformed arithmetic are rejected.
Static PIE (`ET_DYN`) remains P2. Parsing itself does not map or execute images.

## Mapping status

Validated loads become pinned image regions backed by the existing stable
executable source. Each region retains the page-aligned virtual range, aligned
file offset, file-backed byte count, final protection, and source identity.
The shared page-fault path reads file bytes on first touch and supplies zeroes
after `p_filesz`; its existing quota accounting and W^X checks remain in force.

The mapping boundary rechecks source length, range arithmetic, overlap, entry
placement, and protections. Linux `ET_EXEC` may start at `0x400000`; load pages
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
100. `AT_RANDOM` is deliberately absent; no deterministic bytes are presented
as entropy.

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

## Validation

The validation kernel runs a synthetic `LinuxX8664` ring-3 task through two
entry/return cycles and checks `-ENOSYS`, IF restoration, DF clearing, and
native-gate rejection. It then supplies an invalid return RSP and must be
retired rather than returned to. A native task checks Linux-gate rejection. The
live gate emits `LINUX_SYSCALL_OK`, `LINUX_ABI_ISOLATION_OK`, and
`LINUX_RETURN_FRAME_GUARD_OK`.

The validation initramfs also contains `/system/linux/raw-probe`, a static ELF.
PID 1 proves that a false format hint, a non-large profile, and an extra grant
are refused. It then launches two instances concurrently. Each instance grows
and shrinks `brk`, creates, reprotects, discards, and removes anonymous memory,
then selects a distinct FS base from its input. Both block with TLS installed,
resume, verify `ARCH_GET_FS` and an actual FS-relative load, exercise stdio,
and terminate through `exit_group`. PID 1 observes both outputs and teardown
before publishing `LINUX_MEMORY_TLS_OK`, `LINUX_PID1_LAUNCH_OK`, and
`LINUX_RAW_SYSCALL_OK`.

PID 1 then runs `/system/linux/musl-hello one two` in `/system/linux` with
`GOOS_LINUX_PROBE=musl`. The static musl program prints its argument count and
arguments, checks the environment, TLS, and both heaps, opens and closes its
own image twice through the VFS session, and confirms `EBADF` for a closed
descriptor and `ENOENT` for a missing path.

It then names the same image as `musl-hello` through `open`, and as
`./musl-hello`, `..//linux/./musl-hello`, and
`../../../system/linux/musl-hello` through `openat`. It expects `ENOENT` for an
empty path, `ENOTDIR` and `EBADF` for a relative path under a stream and a
closed `dirfd`, `ENOENT` for a missing 300-byte name, and `ENAMETOOLONG` for a
1100-byte one. PID 1 also creates a file with a 235-byte path under `/tmp` and passes it
in `GOOS_LINUX_LONG`; the program must open it, which needs the attached path
object.

Last it works in `/tmp/linux-files`. It creates a file with
`O_CREAT|O_EXCL`, writes 8192 bytes, seeks by all three origins, reads back a
range and the short tail, appends through `O_APPEND`, truncates with
`O_TRUNC`, writes with `writev`, opens the directory and a file under it by
`dirfd`, renames a file and a directory, and removes everything. It checks the
errno of each refusal on the way: `EEXIST`, `EINVAL`, `EBADF`, `ESPIPE`,
`EISDIR`, `ENOTDIR`, `ENOENT`, `ENOTEMPTY`, `ERANGE`, and `EROFS` for the
system image.

It stats the file by name, by descriptor, through `lstat`, and through
`newfstatat` with and without `AT_EMPTY_PATH`, and requires one identity for
all of them, a different one for the directory and for the program image, the
expected type, size, link count, and mode, and `ENOENT`, `ENOTDIR`, `EBADF`,
`EINVAL`, and `EFAULT` for the bad calls. It writes a file through `fprintf`
and reads it back through `fgets` and `fread`, which is `readv`, and reads end
of input from standard input the same way.

PID 1 compares the whole output, requires exit status zero, and publishes
`LINUX_MUSL_OK`, `LINUX_VFS_SESSION_OK`, `LINUX_PATH_OK`, `LINUX_FILE_IO_OK`,
and `LINUX_STAT_OK`. The same binary prints the same lines on Linux;
`userspace/linux/README.md` holds that trace and the three calls whose results
differ.

The boot test also runs two session checks from the shell. `musl-hello
session-timeout` opens the name the validation VFS answers late: the open must
end with `ETIMEDOUT` after the eight-second deadline, and the next open must
succeed, which requires the bridge to discard the late reply that arrives
with it. `musl-hello session-loss` starts behind a pipeline, the VFS restarts,
and the program must report `EIO` from its next open: a Linux process keeps
the session it was launched with and gets no new one.

Host tests cover the pieces that need no CPU: reply classification, session
correlation and failure, open flags and the open plan, seek arithmetic,
removal checks, directory records, two-path continuations, the `struct stat`
layout with a fuzz target, the VFS-to-status translation, path resolution
with its bounds and a fuzz target, vector validation,
compatibility PID allocation, descriptor close, and the fixture's checksum and
ELF acceptance.
