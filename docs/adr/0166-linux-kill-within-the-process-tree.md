# ADR 0166: kill reaches the caller and its descendants

## Status

Accepted.

## Context

A shell stops its own children with `kill`, and musl's `raise` and `abort`
signal the calling thread with `tkill`. Linux checks permission by user ID.
Every Linux process on GoOS runs with UID 1000, so that check would let any
Linux program signal any other, in every family. The compatibility PID space
is also shared by all families.

## Decision

`kill(pid, signal)`, `tkill(tid, signal)`, and `tgkill(tgid, tid, signal)`
accept a positive target that is the caller or one of its descendants,
followed through parent links in the family registry. A target outside the
caller's family is `ESRCH`, as if it did not exist, so a PID in another family
reveals nothing. A target in the family that is no descendant, the caller's
parent for instance, is `EPERM`. Process-group and broadcast forms, a PID of 0
or below, are `EINVAL` until GoOS has process groups. A single-task process has
one thread whose ID is its PID, so `tgkill` requires the two IDs to match and
answers `ESRCH` otherwise.

Signal 0 checks the target and sends nothing. A zombie target accepts a signal
and nothing happens. A signal GoOS cannot deliver is `EINVAL`. The deliverable
set grows by `SIGHUP`, `SIGABRT`, and `SIGALRM`, whose default action also ends
the process.

A signal the target would die of ends it at once: `SIGKILL`, or an unblocked
signal whose disposition is the terminating default. The kernel cancels the
target's waits and continuation and retires its task with that signal as its
termination status, the way the native kill path retires a task. Any other
signal becomes pending and acts at the target's next system call return. The
caller's own signals act as the call returns. `siginfo` names the sender's PID
and UID 1000, with `SI_USER` for `kill` and `SI_TKILL` for the other two.

## Consequences

A Linux family cannot touch another family or a native process through
compatibility PIDs. A child cannot signal its parent, which Linux would allow
for the same user. `SIGTERM` stops a child asleep in `read` or `wait4`. A
handled signal sent to a sleeping process waits until that process returns
from its call.
