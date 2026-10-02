# ADR 0177: Linux threads

## Status

Accepted. Lifts the thread limit ADR 0176 recorded; amends the `clone`,
`futex`, `gettid`, and `set_tid_address` rows of the Linux ABI table.

## Context

7-Zip's default LZMA2 encoder runs a match finder and an encoder in two
threads, and other real programs use threads for anything parallel. A trace of
`7zzs a` on Linux shows what glibc's pthreads ask for:

- `clone` with `CLONE_VM|CLONE_FS|CLONE_FILES|CLONE_SIGHAND|CLONE_THREAD|
  CLONE_SYSVSEM|CLONE_SETTLS|CLONE_PARENT_SETTID|CLONE_CHILD_CLEARTID`, and
  musl adds `CLONE_DETACHED`;
- `futex` `FUTEX_WAIT` and `FUTEX_WAKE`, private for mutexes and condition
  variables and shared for `pthread_join`, 314 calls for one small archive;
- `exit` per thread, with the kernel zeroing the thread's TID word and waking
  it, which is how `pthread_join` returns;
- anonymous `mmap` with `MAP_STACK` or `MAP_NORESERVE` for stacks and arenas,
  and `fd` -1 passed as a 32-bit int.

The kernel already let a process hold several tasks, each with its own
registers, kernel stack, FS base, and FPU image, and `exit_group` already
ended all of them. The Linux layer assumed one task per process everywhere
else.

## Decision

**A thread is a task in its process's address space.** `clone` with the
thread flags creates a task on the same page-table root, resuming after the
call with RAX zero on the stack the caller gave, with the FS base
`CLONE_SETTLS` names and the caller's x87 and SSE state. The flags must
include all five sharing flags; `CLONE_SYSVSEM`, `CLONE_DETACHED`, and the TLS
and TID options may be added; anything else is `EINVAL`. The process's task
quota (8 for a Linux process) bounds its threads, with `EAGAIN` past it. Every
check runs before the task exists, because a runnable task cannot be withdrawn.

**A thread has a TID from the PID space.** The first thread's TID is the PID;
the others get one no live PID or TID holds, and no PID is later allocated
over a live TID. `gettid`, `set_tid_address`, and `set_robust_list` answer per
thread.

**futex sleeps.** A wait whose word still holds the expected value sleeps on
the pair (address space, address) until a wake with an overlapping bit set, a
signal with a handler (`EINTR`), or its timeout (`ETIMEDOUT`): an interval for
`FUTEX_WAIT`, a point on the monotonic or realtime clock for
`FUTEX_WAIT_BITSET`. A Linux process maps no memory another process shares, so
private and shared futexes are keyed alike. Requeue, priority inheritance,
wake-op, and waits on several words stay refused and counted.

**A thread's end is visible.** When a task ends and its process lives on, the
kernel zeroes the word its thread registered with `CLONE_CHILD_CLEARTID` or
`set_tid_address` and wakes one waiter there. A task that ends abnormally, by
a fault or a kill, ends every task of its process, as a fatal signal does on
Linux. `exec` ends every other thread first, at its point of no return, and
the caller becomes the first thread; the ended threads touch nothing in the
new image.

**Each thread has its own file I/O buffer.** File reads and writes move bytes
through a memory object lent to the VFS. With one object per process, a
thread's write gathered its bytes into the object while another thread's read
waited for the VFS to fill it, and 7-Zip's multithreaded encoder wrote a
corrupt archive. A thread now creates its own object on its first file call;
it goes, and its pages return to the quota, when the thread ends.

**A thread's kernel stack leaves the shared address space with it.** The
kernel maps each task's kernel-entry stack into its process root. A process
used to end with its only task, so the mapping went with the root. A thread
ending in a live process now has the mapping removed; before, the stale
mapping made a later task whose guard page reused those frames fail to start.

**Each thread has its own signal mask.** The process's signal state holds one
thread's blocked mask at a time; as each Linux call begins, the kernel saves
the mask in force to the thread that had it and loads the caller's, and a
signal is delivered at a call's return only if that thread's mask allows. A
new thread starts with its creator's mask. This matters at once: musl and
glibc block every signal in a thread on its way out and never unblock, and
with one mask per process the first thread to end left the whole process
deaf to signals.

**sched_yield returns at once.** The scheduler preempts every task at its tick,
so a thread spinning on a lock another holds loses at most one quantum.

## Consequences

7-Zip runs with its default thread count: `7zzs a`, `-mx9`, `t`, `x`, and the
in-memory benchmark `7zzs b -mmt2` produce correct results. `abi-probe
threads` checks pthreads, preemption, concurrent file I/O, and `exit_group`
from a thread, on GoOS and on a Linux kernel.

Pending signals are per process: one pending set for all threads, taken by
the first thread whose call returns with the signal unblocked, and a
process-directed signal interrupts the first thread found waiting. Whether a
waiting thread is interrupted is decided by the mask in force, which is the
last caller's. `tkill` and `tgkill` aimed at a thread deliver to its process. A forked child of a multithreaded process gets only
the calling thread, as on Linux.

Threads run on one CPU. A Linux process can hold at most 8 tasks and the
system 64, so a program that starts one thread per reported CPU stays well
inside both.
