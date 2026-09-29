# ADR 0132: Linux process personality and syscall entry

## Status

Accepted and implemented through Phase 2 for GoOS 0.2.0.

## Context

Native GoOS programs enter the kernel through DPL 3 interrupt vector `0x80`.
Linux x86_64 programs use the `SYSCALL` instruction and a different register,
status, and startup contract. Selecting a dispatcher from a syscall number
would let a program cross ABI boundaries and attempt to use guessed native
handles.

## Decision

Every Process has a kernel-owned immutable ABI: `Native` or `LinuxX8664`. The
validated executable path supplies it when initializing the process slot; no
runtime setter exists. All Tasks in one Process share that identity.

The native `int 0x80` trampoline checks the current task's kernel-owned page
table root against the process registry before any normal or blocking native
operation is decoded. Only `Native` is accepted. Rejection returns the native
`PermissionDenied` status, increments a bounded counter, and logs the first
failure.

A distinct Linux x86_64 `SYSCALL` entry accepts only `LinuxX8664`, uses the
Linux register and negative-errno convention, switches through a supervisor-
only GS entry record to the current task's guarded kernel-entry stack, installs
the kernel CR3, and returns through a separate high-half IRETQ path. The
scheduler updates the entry record's CR3 and RSP0 on every task selection.

`IA32_EFER.SCE`, `IA32_STAR`, `IA32_LSTAR`, and `IA32_FMASK` are installed and
read back before userspace starts. FMASK clears IF, DF, TF, and AC. The entry
executes CLD before Go code; return flags force IF and clear DF, TF, AC, IOPL,
NT, RF, and VM. User GS is zero in 0.2.0. SWAPGS selects kernel-owned entry
data only while interrupts are masked inside a Linux compatibility call;
`ARCH_SET_GS` remains unsupported.

The GS entry record carries the current process root for ABI authorization;
the protected entry mapping supplies the kernel root used before dispatch.
The compatibility dispatcher validates the saved user RIP and RSP before
IRETQ. A malformed return frame retires the task instead of faulting in the
kernel. Unknown syscalls return `-ENOSYS`; Phase 2 intentionally implements no
semantic syscall. Neither gate infers personality from a request number.

## Consequences

Native behavior and request numbering remain unchanged. A Linux process cannot
use the native gate as a capability bypass. Adding another ABI requires a new
explicit identity and entry policy rather than widening either existing gate.

The validation build runs a synthetic ring-3 `LinuxX8664` task through two
SYSCALL/IRETQ cycles, checks `-ENOSYS`, IF/DF sanitation, and native-gate
rejection, then proves that an invalid return RSP retires the task. A native
task also proves that Linux entry rejects the wrong ABI.
