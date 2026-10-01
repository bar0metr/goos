# ADR 0164: Linux handlers run on an x86_64 rt_sigframe

## Status

Accepted.

The general rt_sigframe and rt_sigreturn design remains current.
The floating-point/SSE portion is superseded by ADR 0174.

## Context

A Linux handler expects the x86_64 `rt_sigframe`: a return address, then
`struct ucontext` with the interrupted registers and mask, then `siginfo`.
musl installs every handler with `SA_RESTORER` and a restorer that calls
`rt_sigreturn`. The frame lies in user memory, so a handler, or a program
calling `rt_sigreturn` directly, can put any value in it.

## Decision

To enter a handler, the kernel places the frame below the interrupted stack
pointer and its 128-byte red zone, aligned as a function entry expects. The
frame holds the restorer address, `struct ucontext`, and `siginfo`. The
context saves the general-purpose registers, the call's return value as RAX,
RIP, RSP, RFLAGS, the user selectors, and the mask from before the handler.
It writes no floating-point state and sets `fpstate` to null; `uc_stack` says
no alternate stack is in use. Superseded by ADR 0174: current signal frames
carry a 512-byte FXSAVE image through `uc_mcontext.fpstate`.

The handler starts with RDI holding the signal, RSI the `siginfo`, RDX the
`ucontext`, and RAX zero. Its mask adds the action's mask and, without
`SA_NODEFER`, the signal itself. `SA_RESETHAND` restores the default
disposition. A missing `SA_RESTORER`, a handler or restorer outside the user
half, a stack too low for the frame, or a frame the kernel cannot write ends
the process with `SIGSEGV`.

`rt_sigreturn` reads `struct ucontext` at the caller's stack pointer, which is
where the handler's return left it. It restores the registers, RIP, RSP, and
the mask without `SIGKILL` and `SIGSTOP`, and returns the saved RAX as its own
result. A return address or stack outside the user half, or an unreadable
frame, ends the process with `SIGSEGV`. RFLAGS passes through the same filter
as every system call return, which keeps IF set and clears TF, DF, IOPL, NT,
RF, VM, and AC.

## Consequences

A handler can change the context it returns to, including the result of the
interrupted call. No frame value can return to kernel memory or raise the
task's privilege. Handlers run on the interrupted stack because
`sigaltstack` is not supported. FS base is not part of the frame and stays
what it was, as on Linux. The floating-point consequences are ADR 0174's:
the frame carries the interrupted x87 and SSE state and `rt_sigreturn`
restores it.
