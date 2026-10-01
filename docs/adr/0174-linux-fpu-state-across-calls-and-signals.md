# ADR 0174: Linux x87 and SSE state survives system calls and signals

## Status

Accepted.

## Context

The kernel is built with SSE and uses XMM registers in ordinary code. The
scheduler saves a task's FXSAVE image when it switches tasks, and the user
page-fault path saves it around the Go code it calls. The Linux `SYSCALL`
path saved nothing, so kernel code could change a Linux program's XMM
registers and MXCSR during any system call. Linux preserves them.

A signal handler runs between a system call and the code that made it. The
x86_64 `rt_sigframe` carries the interrupted floating-point state through
`uc_mcontext.fpstate`, and `rt_sigreturn` restores it. GoOS wrote a null
`fpstate` (ADR 0164), so a handler that used SSE corrupted the interrupted
code's registers. A forked child started from the initial state instead of
its parent's (ADR 0158).

## Decision

The Linux entry path saves the caller's state with `FXSAVE` into a per-task
image before any kernel code runs, and the return path restores it with
`FXRSTOR`. A call that blocks restores the image before the scheduler saves
the task, so the retried call sees the caller's state. The format is the
512-byte FXSAVE image, which is what Linux exposes when the frame carries no
XSAVE header; no translation is needed.

Signal delivery copies the saved image to user memory above the frame, 64-byte
aligned and below the 128-byte red zone, and points `uc_mcontext.fpstate` at
it. The handler starts from the initial state: every register cleared, MXCSR
0x1f80, and the x87 control word 0x037f, as on Linux.

`rt_sigreturn` checks that a non-null `fpstate` lies in the user half, copies
the whole image into kernel memory, and clears every MXCSR bit outside the
processor's MXCSR_MASK before the image is used, as Linux clears them. Loading
a reserved bit would fault in the kernel. An unreadable image, one in the
kernel half, or one that runs off a mapping ends the process with `SIGSEGV`
before any state changes. A null `fpstate` restores the initial state.

`fork`, `vfork`, and `clone` give the child the parent's image at the call.
`execve` starts the new image from the initial state.

## Consequences

XMM0 to XMM15, MXCSR, and the x87 registers and control word survive system
calls, handlers, nested handlers, and `fork`. A handler can edit the saved
image and the interrupted code gets the edited state. AVX and AVX-512 state is
not saved: GoOS does not enable it, so a Linux program cannot use those
registers. Each Linux system call costs one `FXSAVE` and one `FXRSTOR`.
