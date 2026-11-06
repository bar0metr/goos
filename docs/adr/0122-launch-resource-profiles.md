# ADR 0122: Launch resource profiles

## Status

Accepted.

## Context

ADR 0072 gave every ordinary process the same quota. That policy kept one
process from draining a kernel table, but it reserved the same 32 MiB page
allowance and sixteen streams for a small utility, a filesystem checker, and a
long-running service. PID 1 already decides which executable and capabilities a
child receives. It also needs to choose a bounded resource class for that
workload.

The launch channel belongs to PID 1. A child cannot send a lifecycle launch
request and no kernel call changes a live process's quota. The ABI still must
prevent PID 1 from supplying arbitrary numeric limits because PID 1 is a policy
authority, not an unlimited allocator.

## Decision

The lifecycle request carries one profile identifier. The kernel accepts three
profiles and maps each identifier to constants compiled into the kernel:

| Profile | Channels | Streams | Tasks | Pages |
| --- | ---: | ---: | ---: | ---: |
| `tiny` | 2 | 8 | 1 | 2048 |
| `normal` | 4 | 16 | 16 | 8192 |
| `large` | 8 | 32 | 64 | 16384 |

The task column is the revised one; it was 1, 4 and 8. See the revision
below.

Pages are 4 KiB, so the page limits are 8 MiB, 32 MiB, and 64 MiB. The limits
count resident process pages and page tables. Reserved virtual memory costs no
pages until the process touches it.

PID 1 launches commands and system services with `normal` unless policy names
another profile. The `quota` diagnostic uses `tiny`; `fsck-fat` and `vmprobe`
use `large` because they scan or exercise memory at a scale ordinary commands
do not. Programs loaded from arbitrary filesystem paths receive `normal`.
Service definitions may select another known profile. An omitted service
profile means `normal`.

PID 1 sends only the profile identifier. The lifecycle decoder rejects unknown
identifiers and nonzero reserved bytes. The kernel performs its own mapping and
never accepts numeric limits from userspace. A child has no operation that can
replace its profile or raise a counter.

Quota exhaustion keeps the existing typed behavior. Launch returns
`LaunchResourceExhausted` if its initial pages exceed the selected limit. A
runtime allocation call returns `StatusResourceExhausted`; a page fault after
the process spends its page allowance retires that process with `page quota
exhausted`.

`ProcessInfo` continues to expose held and permitted counts for every resource.
The `quota` command therefore reports the applied profile's limits without
adding a second diagnostics interface.

PID 1 keeps its separate system quota: 256 channels, 64 streams, 16 tasks
(eight before the revision), and 16384 pages. `GrantSystemQuota` still accepts
PID 1 only and grants a limit, not an exemption.

**Revision: task quotas of 64, 16 and 1.** A task is a thread, and a quota of
eight left a Go or Linux program at eight threads on a machine with 256 task
slots. The large profile now allows `process.MaxTasksPerProcess`, 64 tasks,
the normal one 16, and the tiny one keeps its single task. Memory, not the
count, bounds a thread: each task past a process's first holds
`process.TaskPages`, six pages, of the page quota for its kernel stack and
guard, charged when `ThreadCreate` or `clone` reserves the task and returned
when the task ends or the creation fails. 64 tasks cost a large process 378
of its 16384 pages. The first task binds after the launch has charged the
address space and before it runs, where a refusal could not be undone, so it
pays nothing and a single-threaded process pays what it paid before. A
thread past either quota gets `StatusResourceExhausted` natively and `EAGAIN`
from `clone`. GoOS implements no `RLIMIT_NPROC`: `getrlimit` reports it
unlimited, and the task quota, the family's 64 processes and the scheduler's
native reserve answer `EAGAIN` where Linux would.

## Consequences

PID 1 can assign less capacity to a small diagnostic and more to a filesystem
check without raising the default for every process. All profiles remain below
system-wide structural bounds.

The lifecycle protocol version changes because the launch header gains a
profile byte and seven reserved bytes. Native programs do not consume this
protocol; PID 1 and the kernel update together in one system image.

The current profiles cover the resources already charged per process:
channels, streams, tasks, and pages. Handle tables, shared-memory object counts,
startup bytes, VM regions, and IPC queues retain their existing fixed bounds.
Future work may add counters only when a process can consume those resources in
a way the structural bound does not contain.
