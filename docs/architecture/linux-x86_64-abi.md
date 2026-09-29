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
| `read(0)` | implemented | fd 0 stream, blocking without polling |
| `write(1)` | implemented | fd 1/2 streams, bounded chunks and partial writes |
| `exit(60)` | implemented | exits the calling task |
| `exit_group(231)` | implemented | retires every task in the process |
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
are refused. It then launches the ELF with minimal authority; the program reads
stdin, echoes it through `write`, writes `LINUX_RAW_SYSCALL_OK`, and terminates
through `exit_group`. PID 1 observes its output and teardown before publishing
`LINUX_PID1_LAUNCH_OK` and `LINUX_RAW_SYSCALL_OK`.
