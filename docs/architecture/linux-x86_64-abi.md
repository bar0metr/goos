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

## ELF status after Phase 3

The pure parser accepts only ELF64 little-endian x86_64 `ET_EXEC`. It bounds
program headers at 64 and `PT_LOAD` segments at 16, produces page-rounded load
metadata, and validates source ranges, user ranges, configured reserved pages,
alignment/congruence, overlap, protections, and the executable entry.

`PT_PHDR`, `PT_TLS`, `PT_GNU_STACK`, and `PT_GNU_RELRO` are recorded and checked
against their containing loads. `PT_INTERP`, `PT_DYNAMIC`, executable stack,
W+X, unknown program-header kinds, and malformed arithmetic are rejected.
Static PIE (`ET_DYN`) remains P2. Phase 3 does not map or execute ELF images.

## Validation

The validation kernel runs a synthetic `LinuxX8664` ring-3 task through two
entry/return cycles and checks `-ENOSYS`, IF restoration, DF clearing, and
native-gate rejection. It then supplies an invalid return RSP and must be
retired rather than returned to. A native task checks Linux-gate rejection. The
live gate emits `LINUX_SYSCALL_OK`, `LINUX_ABI_ISOLATION_OK`, and
`LINUX_RETURN_FRAME_GUARD_OK`.

An ELF-based `write`/`exit_group` program is not yet claimed: it requires the
mapping, startup, launch-policy, and semantic-syscall phases.
