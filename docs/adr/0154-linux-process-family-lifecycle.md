# ADR 0154: Linux families retain lightweight zombie records

## Status

Accepted.

## Context

Linux child waiting needs parentage and exit status after the GoOS Process has
released its address space, handles, tasks, and compatibility I/O state. Keeping
the whole Process alive for `wait4` would retain scarce kernel resources. Freeing
the compatibility PID at exit would let another process reuse an identity that
the parent still needs to reap.

Native supervision cannot represent the relationship. PID 1 launches Linux
roots, while Linux descendants will come from an authority-preserving fork path.
Those descendants remain ordinary GoOS Processes, but PID 1 must not treat them
as independently supervised jobs.

## Decision

The Linux compatibility layer owns a fixed-size family registry. Each record
stores the compatibility PID, generation-safe GoOS Process ID, parent PID,
family ID, lifecycle state, exit status, and terminating signal. The lifecycle
states are running, zombie, and reaped.

A launch root starts a family with a kernel-owned identifier and names
conceptual PID 1 as its parent. A child inherits its parent's family ID. The
family identifier remains distinct if the root exits and another process reuses
its PID. The allocator reserves a PID for every running or zombie record and
permits reuse after reap.

Process creation reserves its record before the child becomes runnable. A
failed creation rolls back a running record that has no children, so no PID or
family slot survives the failed attempt.

Exit closes descriptors and removes directory and VFS state before the record
becomes a zombie. Reap returns a copy marked reaped and clears the registry
slot. If a parent exits, the registry assigns its running children to PID 1 and
clears its orphan zombies. Records parented by PID 1 clear at exit because no
Linux caller can wait on them.

The native process registry continues to own tasks, address spaces, quotas,
handles, diagnostics, and resource reclamation. The family registry grants no
authority and stores no handles.

## Consequences

Linux wait state survives full GoOS Process teardown without retaining heavy
resources. Parent and family relationships remain bounded and independent of
native supervision. Zombie records consume registry and PID capacity until the
parent reaps them, which supplies a fixed upper bound for abandoned children.
