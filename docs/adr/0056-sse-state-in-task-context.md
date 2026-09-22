# ADR 0056: SSE and x87 state are part of the task context

## Status

Accepted. Corrects the diagnosis recorded in ADR 0053.

## Context

After safe-return rescheduling (ADR 0055) the 1 kHz stress failed again in 4
of 16 guests with `VFS_WAIT_PERMISSION_DENIED`, and PID 1 intermittently
reported `APP_VFS_SESSION_FAILED`. The diagnostic build showed VFS asking to
wait on values such as `0x4000000000000003` (channel identifiers) or on
fragments of startup data (`"stdout"`, `"stderr"`, handle bindings).

Three further diagnostics narrowed this down:

- VFS recorded the interest count it computed. It was 7 or 9 while VFS held two
  sessions, so the corrupt values were already in the loop that copied the
  session table, not introduced by the kernel's read.
- A consistency check caught the moment: the copy was wrong while the session
  table itself, read back at the failure, was intact.
- An audit of every user page of VFS found no page owned by another process,
  by the kernel image, or unknown to the allocator.

The wrong values always came in aligned 16-byte pairs, sometimes only in the
tail of the copy. That is the width of an XMM register. TinyGo copies arrays
through XMM registers, and the context switch saved only the 15 general-purpose
registers. A task preempted between loading a pair into XMM and storing it
resumed with whatever the kernel or the next task had left there: channel
identifiers from the kernel's own loops, or startup data from `StartupRead`.
Preemption frequency scales with the timer rate, which is why the failures
clustered at 1 kHz. The kernel was exposed in the same way: its Go code keeps
XMM6–XMM15 live across calls under the Microsoft x64 ABI, including across
`KernelSleep`.

ADR 0053 attributed the same signature to physical pages aliased by an
allocator race. That race was real and its fix stands, but the evidence it
cited was this defect.

## Decision

Each scheduler slot owns a 512-byte FXSAVE area. Every path that switches away
from a task that will run again saves its state with `FXSAVE64`: timer
preemption, IRQ11 preemption, kernel yield, a blocking native call, and a
native call that yields on return. `.switch_to` restores the incoming task's
state with `FXRSTOR64`. Exit and fault paths do not save.

A slot's area is reset to a clean image when the scheduler starts and when an
exited task's slot is reclaimed. The clean image has every register cleared and
every floating-point exception masked (FCW 0x037F, MXCSR 0x1F80), so a new task
never observes SSE data left by the previous owner of its slot. The kernel sets
CR4.OSFXSR and CR4.OSXMMEXCPT explicitly instead of relying on firmware.

The targets enable SSE2 and nothing wider, so FXSAVE covers the complete state.
Enabling AVX or later requires XSAVE with a larger area.

## Alternatives

Lazy switching with CR0.TS and `#NM` saves the copy for tasks that do not touch
SSE, but every task here does, since TinyGo uses XMM for ordinary copies.
Building the kernel without SSE would protect only the kernel's own state; user
tasks would still corrupt each other across preemption.

## Consequences

The 1 kHz diagnostic stress passes 16 of 16 guests, where it failed 7 of 16 and
3 of 16 on the two preceding runs. Each switch copies 512 bytes out and 512 bytes
in, which is small next to the page-table and scheduling work of a switch.
The areas take 32 KiB of kernel data for 64 slots.
