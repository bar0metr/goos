# ADR 0151: Hardware random bytes and recorded signal state

## Status

Accepted. Amends the `AT_RANDOM` decision of ADR 0133.

The signal-delivery portion is superseded by ADR 0163 through ADR 0167.
The RDRAND/getrandom decision is superseded by ADR 0185.

## Context

ADR 0133 left `AT_RANDOM` out of the initial stack because the kernel had no
entropy it could vouch for, and `getrandom` returned `ENOSYS`. A program that
needs a key or a hash seed had nowhere to get one.

`rt_sigaction` and `rt_sigprocmask` also returned `ENOSYS`. BusyBox `sh`
installs handlers at startup, and a C library's `abort` and `raise` block
signals first. Each of them worked around the refusal; a stricter program
would not.

## Decision

**Random bytes come from RDRAND or not at all.** When CPUID reports the
instruction, `getrandom` fills the buffer from it and every launch puts 16
fresh bytes behind `AT_RANDOM`. The kernel retries a draw ten times, keeps no
pool and no generator state of its own, and clears its copy after the bytes
reach the program. One call returns at most 256 bytes, the size Linux
promises to fill whole; a longer request is a short read. The three defined
flags are accepted and change nothing, because there is one source.

When the CPU has no RDRAND, `getrandom` returns `ENOSYS` and the stack has no
`AT_RANDOM` entry. The refusal is counted with the unsupported calls. The
kernel has timers and interrupt arrival times and nothing else, and output
mixed from those under an emulator is guessable. A value that looks random
and is not would be worse than a refusal: the caller would use it as a
secret. musl derives its stack-protector canary from addresses when the
entry is missing, as it does on an old Linux kernel.

QEMU's default CPU model has no RDRAND. The Linux boot test asks for one
(`-cpu qemu64,+rdrand`) and covers the source; the general boot test keeps
the default and covers the refusal.

This section describes the 0.2.0 behavior and is superseded by ADR 0163 to
ADR 0167, under which signals are delivered, interrupt blocked calls, and
return through `rt_sigreturn`.

**Signal state is recorded and nothing is delivered.** `rt_sigaction` stores
a disposition per signal and reports the previous one. `rt_sigprocmask` keeps
the blocked mask. Both follow the Linux rules a program can check: signals 1
to 64, a set size of 8, no disposition for `SIGKILL` and `SIGSTOP`, and
neither of those in a mask.

GoOS sends no signal. No handler runs, no call returns `EINTR`, and Ctrl-C
ends the process through the native kill path whatever it registered for
`SIGINT`, which is what `SIGKILL` does on Linux. `kill`, `tkill`, `tgkill`,
`rt_sigreturn`, `sigaltstack`, and `rt_sigsuspend` stay `ENOSYS`, so a
program that raises a signal sees the refusal instead of a delivery that
never comes.

## Consequences

A program that draws a key on a machine with RDRAND gets hardware output, and
the quality of that output is the CPU vendor's. On a machine without it the
program gets an error it has to handle. Two runs of one program on two
machines can therefore differ in whether `getrandom` works.

In 0.2.0, a program that installed a `SIGINT` handler to clean up on Ctrl-C
found the handler installed and did not get to run it; ADR 0163 to ADR 0167
supersede this. The state costs 2 KiB per
Linux process slot.
