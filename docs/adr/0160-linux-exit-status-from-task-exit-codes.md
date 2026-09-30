# ADR 0160: Linux exit status comes from the task exit code

## Status

Accepted.

## Context

A zombie record must tell its parent whether the child called `exit` or died,
and with which status or signal. The scheduler records one 64-bit exit code
per task. A program's `exit` stores its status there. A CPU fault stores the
exception vector with bit 63 set. The native kill path stores `1<<63 | 9`,
and a task the kernel ends after a failed wake stores `1<<63 | 5`. Before this
decision the family registry copied the raw code as an exit status and never
recorded a signal.

## Decision

The compatibility layer decodes the code when the process exits:

- bit 63 clear: a normal exit with the low eight bits as status;
- vector 0, 16, or 19: `SIGFPE`;
- vector 1 or 3: `SIGTRAP`;
- vector 6: `SIGILL`;
- vector 4, 10, 13, or 14: `SIGSEGV`, which also covers a rejected syscall
  return frame;
- vector 11, 12, or 17: `SIGBUS`;
- a kill, a kernel termination, or any other value: `SIGKILL`.

Vectors 5 and 9 cannot come from a 64-bit user fault, so the kernel reasons
that reuse those numbers stay unambiguous.

The zombie record keeps the status, the signal, and the CPU ticks the Process
accumulated. Its wait status puts the exit status in bits 8 to 15 or the
signal in the low seven bits, as Linux `wait4` reports it. No core-dump bit is
set, because GoOS writes no core file.

## Consequences

A parent can learn that a child crashed or was killed without the child's
Process surviving teardown. Ctrl-C in the native shell reads as `SIGKILL`,
which matches what the kill does. The mapping follows the Linux x86_64
exception handlers for the vectors a user task can raise.
