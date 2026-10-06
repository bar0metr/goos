# ADR 0188: Task affinity and per-processor runtime counters

## Status

Accepted. Extends the scheduler of ADR 0182.

## Context

Tasks ran wherever the scheduler placed them, and nothing outside a
diagnostic build showed what each processor did. A program could not keep a
thread on one processor, `sched_setaffinity` returned `ENOSYS`, and
`sched_getaffinity` reported every online processor whatever the thread's
state. The scheduler already counted switches, steals, and migrations, but
only as machine-wide totals, and the per-processor tick and interrupt counts
were readable only through `diag`.

## Decision

### Affinity in the run state

`sched.Core` keeps a requested processor mask per task slot, zero meaning
every processor, and per processor the mask of tasks barred from it. A task
may run on the online processors its mask allows; the kernel's own tasks
stay pinned to the housekeeping processor. `Install` resets the mask, so a
reused slot starts unconstrained.

Every path that chooses a processor tests the barred mask:

- placement of a new or woken task considers only allowed processors, for
  its home, for an idle one, and for the least loaded one;
- a processor stealing work skips tasks barred from it;
- a preempted task moves only to an idle processor it is allowed on.

`SetAffinity` refuses a mask with no online processor, a dead task, and a
kernel task moved away from the housekeeping processor. A queued task whose
home is no longer allowed moves to an allowed queue. A running task whose
processor is no longer allowed keeps running there until that processor's
next scheduling point, which a reschedule request brings at once; its home
has already moved, so the processor does not pick it again, and the switch
that leaves it wakes the new home if it idles. The running mask still keeps
the task on one processor at a time. Held and killed tasks keep their
semantics: a held task moves queues but is not kicked to run. `Check`
verifies that no queue holds a task barred from its processor.

### Native interface

`TaskAffinity` (operation 54) takes a process handle, a mask, and an address.
Handle zero names the calling task. Another process needs a process handle,
with `Wait` to read and with `Write` to set; setting applies to every task of
the process, and reading returns the first task's mask. A mask of zero only
reads. Native processes run one task each, so a process handle is the task
handle they need; there is no handle type for one task of another process.

### Linux

`sched_setaffinity` and `sched_getaffinity` reach the same `SetAffinity`
through the target thread's task. They accept TID zero, the caller's TID,
and any TID of the caller's thread group, the group leader included; any
other is `ESRCH`. The kernel reads the first 64 bits of the set, which cover
every processor GoOS runs, and drops offline processors; an empty result is
`EINVAL`, as is a set shorter than 8 bytes, where Linux would zero-extend
it. A bad pointer is `EFAULT`. Threads and forked children inherit their
creator's mask, as on Linux. Both calls left the fast class: finding the
group leader's task reads the process registry.

### Runtime counters

The scheduler's switch, steal, migration, and kill counts are now kept per
processor. `CPURuntime` (operation 53) writes one processor's record: online
state, the task it runs or zero, timer ticks, idle ticks, switches, steals,
migrations, reschedule interrupts, and shootdown interrupts. The scheduler
counters are read under the scheduler lock the scheduler already takes, and
the tick and interrupt counts are atomic words their processor updates, so
the read stops nobody and adds nothing to a hot path. A record is one
processor's at one moment; records of two processors are not one instant.
Any registered process may read them, as it reads the CPU inventory.

The native `cpu` command prints one line per processor: state, busy and
idle shares of its ticks, the slot of its current task or `idle`, and its
switches, steals, and migrations. `cpu -r` prints the raw counters. A
processor with no ticks yet shows 0% busy. `lscpu` stays about topology and
features. `ps` does not show affinity: its record has no task identities to
hang one on, and the probe and `cpu` cover the need.

## Consequences

`make test-smp` restricts a probe thread to the last processor, checks with
`getcpu` that 2,000 samples all ran there, that a thread made before keeps
its full mask, and that an empty mask is `EINVAL`, then widens the mask to
two processors while a thread pinned to the first keeps it busy and waits
for the probe thread to reach the second. It also runs `cpu` and requires
every processor online with switches counted. On four processors the probe
reports `held to cpu 3 for 2000 checks, moved to cpu 0 after widening`.

Host tests in `kernel/sched` cover the default mask, a one-processor mask,
placement, stealing, restriction of queued and running tasks, widening,
refused masks, held and killed tasks, kernel tasks, and slot reuse, and the
randomized interleaving test now changes masks and checks that no processor
picks a task it may not run.
