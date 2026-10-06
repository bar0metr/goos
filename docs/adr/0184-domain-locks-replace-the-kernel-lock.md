# ADR 0184: Domain locks replace the kernel lock

## Status

Accepted. Supersedes the kernel lock of the SMP design (docs/kernel/smp.md
before this change) and amends ADR 0183.

## Context

Tasks ran on several processors at once, but every native call, Linux system
call, user page fault, and housekeeping step took one kernel lock. Threads
of a process computed in parallel and queued at the kernel's door: 7-Zip on
four processors reached a user-mode `parallel-peak` of 3 and a kernel one of
1. The lock also hid ordering bugs. Code that read a table, released nothing,
and wrote it again relied on running alone, and per-call buffers were
package globals.

## Decision

### Locks and their order

The kernel's state is split into domains. Each domain has one lock, and a
processor takes locks in increasing level order with local interrupts
disabled (`kernel/lock`):

| Level | Lock | Guards |
| ---: | --- | --- |
| 1 | registry (reader-writer) | process registry, process fields other than handles and address space, task-to-process map, PID allocation, exit records |
| 2 | linux | the Linux personality's tables: process states with descriptors, working directory, and signals; records; threads; open descriptions; directories; VFS sessions; exec staging |
| 3 | lifecycle | the queue of messages to PID 1 |
| 4 | handle (reader-writer) | every process's handle table |
| 5 | channel | channel registry, channels, executable page requests |
| 6 | stream | stream registry and streams |
| 7 | shmem | shared memory objects and the boot file cache |
| 8 | address space (reader-writer, one per process) | region map and page tables |
| 9 | executable | executable table and the object a fault or launch reads |
| 10 | futex bucket (32, ranked) | the waiters of the words that hash to it |
| 11 | wait tables (each its own) | channel, stream, and process waits; multi-object waits; Linux call continuations |
| 12 | log | kernel log, diagnostic ring, Linux call counters, console input readers |
| 13 | memory | physical page allocator |
| 14 | random | random generator |
| 15 | scheduler | run state, slot tables, kill and hold requests |
| 16 | console | serial and framebuffer output |

Handle tables share one lock because IPC moves handles from one table to
another; one lock makes that a single step. The address-space lock sits
above the object domains, so a call can copy to and from its caller under
any of them, and below the futex buckets, so a futex reads its word with the
bucket held.

### Lock sets

A call does not take locks as it goes. It takes a fixed set when it starts,
in level order, and holds it until it returns or blocks
(`kernel/locksets.go`). Nothing the set guards can change or go away during
the call, and two calls with disjoint sets run at once. `nativeCallLocks`
names the set of each native operation and `linuxCallLocks` the class of
each Linux call:

| Linux class | Set | Calls |
| --- | --- | --- |
| fast | caller's address space, read | futex, sched_yield, getpid, gettid, the uid and gid calls, uname, clock_gettime, gettimeofday, time, getrandom, sched_getaffinity, getcpu |
| memory | registry read, linux, shmem, address space write | mmap, munmap, mprotect, madvise, brk |
| process | registry write, linux, lifecycle, handle write, channel, stream, shmem, address space write, executable | execve, fork, vfork, clone, exit, exit_group, wait4, kill, tkill, tgkill |
| files | registry read, linux, handle write, channel, stream, shmem, address space read, executable | every other call |

A fast call takes the linux lock only for a moment before the call, to find
its process state and thread ID and select its thread's signal mask, and
again after it, to deliver signals. The other classes do both under the
linux lock they hold. The wait tables, futex buckets, logs, the page
allocator, the random generator, and the scheduler are taken during a call,
never held across another lock of their own level, and rank above every set
lock.

A call that waits for its set gives up when another processor stops its task
(`LockUnless` on the processor's stop word) and is retried after a scheduling
point (`ActionRetry`). A processor that holds locks while it waits for tasks
to stop, as exec does for the image's other threads, therefore never waits
for a task that waits for it. `kernelEnter` takes no lock: it refuses to
start a call for a stopping task and clears the task's stale wake under the
scheduler lock.

### Lifetimes

A task's process outlives every call the task makes: teardown runs on the
housekeeping task after the process's last task has left its processor. The
call finds its process through the task-to-process map, whose entry for a
task changes only while that task is not running, so it needs no lock. Every
other object is reached under the lock of its domain, and references that
cross domains are generation-checked IDs, looked up again under the target's
lock. A process slot is cleared only after the teardown has released the
locks it holds in the slot.

### Address spaces

Copies and faults hold their process's address space for reading, so faults
of several threads of a process resolve at once. Page-table pages and leaves
are installed with a compare-and-swap: the loser of a race for a table frees
its own and uses the winner's, and a fault that finds its page already
mapped runs the access again. Mapping changes hold the address space for
writing. A TLB shootdown runs under that write hold, so two shootdowns of one
root never overlap (ADR 0183).

A page of a program's image that an executable object holds needs the
channel and executable locks, which rank below and above the address space.
A copy that needs one fails and records the page; the dispatcher releases the
call's set, takes channel, address space, and executable in order, and either
maps the page and runs the call again or sends the request to the VFS and
parks the task. The reply's bytes are copied into a page at once, and the
faulting task maps that page when its access runs again.

### Waits

A waiter registers before it checks once more, and a publisher changes the
object's state under the object's lock before it publishes, so a wake cannot
fall between the check and the sleep. A Linux continuation accepts one wake,
matched by correlation, under its table's lock. A task another processor
kills after it registered fails its wakes, and its exit cancels what it left
registered before the slot is reused.

A futex reads its word with the word's bucket held. A requeue takes both
buckets, the lower rank first. A cancel follows a wait to the bucket a
requeue moved it to.

### Per-processor state

The buffers a call copies its arguments and results through, and the call
handlers, are per processor (`cpuScratch`). The bootstrap processor's is
static; each other processor's comes from the page allocator before it
starts.

### Checks

Each package that holds a domain's state checks, in validation builds, that
its caller holds the domain's lock (`lock.Guard`), and region maps check
their address space. Locks taken out of order or recursively stop a
validation build. Every build counts contention per level and reports a spin
longer than four seconds (`LOCK_STUCK`); a validation build stops there.
`tests/lint/no_kernel_lock.sh` refuses a kernel lock under its old names and
a kernel entry that takes any lock but the scheduler's.

## Consequences

Faults and fast calls of threads of one process run on several processors at
once, and calls of different processes do whenever their sets are disjoint.
The boot tests print the most processors that were inside calls together
and the most tasks of one process that were (`KERNEL_PARALLEL_PEAK` and
`KERNEL_THREAD_PEAK`, and `kernel-parallel-peak` and `kernel-thread-peak` in
`diag`). On four processors the probe's `kernel` mode raises the second from
1 to 4: four threads of one process were in the kernel at once.

Some domains are still wide. Most Linux calls hold the linux lock, so file
and process calls of all Linux programs run one at a time; every channel
shares one lock, and so does every handle table. Splitting them is a later
change that needs no new mechanism: a finer lock goes in at the level of the
one it replaces.
