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

## Syscall status after Phase 2

| Syscall | Status | Notes |
| --- | --- | --- |
| every number | `ENOSYS` | entry transport only; no fake success |

Wrong-ABI Linux entry returns `-EPERM`. A noncanonical or non-user return RIP
or RSP retires the task rather than attempting an unsafe IRETQ.

## Validation

The validation kernel runs a synthetic `LinuxX8664` ring-3 task through two
entry/return cycles and checks `-ENOSYS`, IF restoration, DF clearing, and
native-gate rejection. It then supplies an invalid return RSP and must be
retired rather than returned to. A native task checks Linux-gate rejection. The
live gate emits `LINUX_SYSCALL_OK`, `LINUX_ABI_ISOLATION_OK`, and
`LINUX_RETURN_FRAME_GUARD_OK`.

An ELF-based `write`/`exit_group` program is not a Phase 2 claim: it requires
the separate Linux ELF loader and semantic syscalls from later phases.
