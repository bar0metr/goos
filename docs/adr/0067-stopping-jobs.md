# ADR 0067: Stopping jobs

## Status

Accepted. Builds on the jobs of ADR 0064.

## Context

Nothing could stop a running process. A command that did not end on its own
held the shell forever, and a background job started with `&` ran until it
finished. The kernel retired a process only when it called `Exit` or faulted.
There are no signals, and adding them means a delivery model, handlers, and a
default action for every process, which is a larger decision than the one this
system needs to be usable from its own shell.

## Decision

**Kernel.** Native operation 31, `ProcessKill`, takes a process handle. The
handle must carry `Write`, so the right to stop a process is a capability like
any other: PID 1 gets it when it launches a child, and nothing else holds a
process handle at all. The kernel refuses to stop PID 1 and refuses a caller
asking to stop itself.

Stopping reuses the path a fault already takes. For every task of the target,
the kernel cancels its registrations in the IPC, stream, process, and
multi-object wait sets, then marks the task dead with the exit code
`kcall.ExitKilled` and clears it from the runnable set. The task is not
executing: the caller holds the single CPU inside the kernel exclusion domain
(ADR 0053). From there the ordinary teardown runs — the exit is delivered,
handles are released, peers see EOF or peer death, and the address space is
freed — so a stopped process leaves the system in the same state as one that
crashed.

**PID 1.** The exec control protocol, version 2, carries an argument byte with
each operation. `ControlKill` names a job slot; PID 1 stops every stage of that
job that has not been reaped. The reply to the job itself still arrives through
the normal path, with the killed exit code, so the shell needs no separate
notification.

**Shell.** `jobs` lists the running background jobs with the command line that
started them. `kill %N` or `kill N` stops one. Ctrl-C stops the foreground
command: while it waits for the reply the shell polls the console every tenth
of a second, and on `0x03` it sends `ControlKill` for the foreground slot and
prints `^C`. A killed job is reported as `[N] killed` rather than as an exit
code. Ctrl-C at the prompt clears the line being typed.

## Alternatives

Unix signals would give `kill -TERM` semantics and let a process clean up.
They need a delivery mechanism, a handler ABI, and a definition of what happens
when a process is interrupted inside a native call. That is worth doing when
the system runs programs that need to clean up; today every process is a short
utility or a service supervised by PID 1.

Terminating through the lifecycle channel, rather than a native operation,
would keep the kernel's interface smaller, but PID 1 would be asking the kernel
to stop a process it names by PID, without proving it started it. The process
handle is exactly that proof.

Letting the shell read `0x03` through a line discipline in a TTY server is how
this will eventually work. There is no TTY server yet, so the shell watches its
own console handle.

## Consequences

A command can be interrupted, and a background job can be stopped, from inside
the system. The boot test covers both.

Input other than Ctrl-C typed while a foreground command runs is discarded: the
shell has nowhere to buffer it, and the command owns the console. A process
killed in the middle of writing to a file leaves that file as it was, since
there is no journalling; `sync` on reboot is unaffected. A stage stopped in the
middle of a pipeline closes its stream, so the remaining stages see EOF.
