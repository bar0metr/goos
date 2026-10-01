# ADR 0165: rt_sigaction accepts only flags GoOS honors

## Status

Accepted.

## Context

`rt_sigaction` stored the 32-byte `sigaction` whatever its flags said
(ADR 0151). Signals are now delivered (ADR 0163), so a stored flag either
changes what happens or silently means nothing. A program that asks for a
behavior GoOS does not provide should learn so when it asks.

## Decision

`rt_sigaction` refuses a new action with any flag outside this set with
`EINVAL`. It reads the low 32 bits of the 64-bit `sa_flags`: libc stores a C
`int` there, and `SA_RESETHAND` sign-extends into the upper half, which Linux
ignores.

- `SA_SIGINFO`: the handler receives `siginfo` and `ucontext`, which every
  frame carries;
- `SA_RESTORER`: required to enter a handler on x86_64;
- `SA_NODEFER` and `SA_RESETHAND`: applied when the handler is entered;
- `SA_NOCLDWAIT` on `SIGCHLD`: children of the process leave no zombie;
- `SA_NOCLDSTOP`: no child ever stops, so no stop is reported;
- `SA_ONSTACK`: no alternate stack can be installed, so the handler runs on
  the current stack, as Linux runs it when none is installed;
- `SA_RESTART`: no system call is interrupted yet, so every call already
  completes as a restarted one would.

A `SIGCHLD` action of `SIG_IGN` also leaves no zombie, as on Linux. Such a
child's record clears as soon as it exits. `SIGCHLD` is still generated for a
handler installed with `SA_NOCLDWAIT`. `wait4` then sleeps until the last
child ends and returns `ECHILD`.

## Consequences

A flag that would change behavior GoOS lacks fails at installation, not later.
`SA_RESTART` must gain its real meaning when blocked calls become
interruptible. Linux ignores unknown flag bits; GoOS refuses them.
