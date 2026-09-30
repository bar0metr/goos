# ADR 0145: Linux identity, time, and limits

## Status

Accepted. Amended by ADR 0153: a sleep ends by the monotonic clock, and a
tick deadline that passes early waits again.

## Context

Programs ask who they are, what system they run on, what time it is, and what
limits apply. Linux answers from its process tree, its users, and its
resource limits. GoOS has capabilities in place of users, no Linux process
tree, and quotas set at launch. The answers must be stable and true of GoOS,
and must promise nothing GoOS does not provide.

## Decision

**Identity.** `getpid` and `gettid` return the compatibility PID (ADR 0139);
a process has one task, so they are equal. `getppid` returns 1: PID 1 launches
every Linux process, and no Linux process tree exists. `getuid`, `geteuid`,
`getgid`, and `getegid` return 1000, the value in the initial stack and in
`st_uid`. The number grants nothing. `setuid` and `setgid` succeed for the
identity the process already has and are `EPERM` for any other: there is no
other identity and no privilege to take one. `getgroups` reports no
supplementary groups.

**System name.** `uname` reports:

| Field | Value |
| --- | --- |
| `sysname` | `Linux` |
| `nodename` | `goos` |
| `release` | `6.0.0-goos` |
| `version` | `GoOS <release> Linux ABI`, with the GoOS release |
| `machine` | `x86_64` |
| `domainname` | `(none)` |

`sysname` is `Linux` because programs select behavior by it and the ABI they
get is the Linux one. The other fields say what implements it. No Linux
kernel of any release is present.

**Clocks.** `clock_gettime` reads the kernel clocks directly, with no service
and no wait. `CLOCK_REALTIME` and its coarse variant read the GoOS realtime
clock. `CLOCK_MONOTONIC`, its raw and coarse variants, and `CLOCK_BOOTTIME`
read the monotonic clock. The CPU-time clocks have no source and are
`EINVAL`, as is any other identifier. musl builds `time` and `gettimeofday`
on this call.

**Sleep.** `nanosleep` blocks the task on a timer deadline through its
syscall continuation. The duration rounds up to whole ticks plus one, so a
sleep never ends early. No signal can interrupt a GoOS Linux process: the
sleep always completes, returns zero, and never writes the remaining time.
A negative field or a nanosecond count of a second or more is `EINVAL`.
Killing the process cancels the wait.

**Limits.** `prlimit64` reports, for the calling process only, soft and hard
limits that are equal:

| Resource | Limit |
| --- | --- |
| `RLIMIT_NOFILE` | 64, the descriptor table |
| `RLIMIT_STACK` | 8 MiB, the stack reservation |
| `RLIMIT_DATA` | 64 MiB, the `brk` heap |
| `RLIMIT_CORE` | 0; no core file is written |
| the other twelve | no limit |

A new limit is refused with `EPERM`. Another process's PID is `ESRCH`. An
unknown resource is `EINVAL`.

## Consequences

Programs get consistent answers from every call that reports an identity.
A program that treats user 0 as privileged finds it is not user 0. A program
that waits for `EINTR` from a sleep never sees one. The page quota of a
process has no Linux resource and is not reported; exceeding it is `ENOMEM`
at the call that allocates.
