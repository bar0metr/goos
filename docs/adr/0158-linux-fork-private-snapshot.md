# ADR 0158: Linux fork copies a private snapshot

## Status

Accepted.

## Context

A shell starts a program by forking and then replacing the child image. The
child must see the parent's memory at the moment of the call, must not see the
parent's later writes, and must share the parent's open-file descriptions.
Copy-on-write would need shared-frame reference counts and write-fault
handling that the VM does not have. A Linux process holds a VFS channel handle
that carries no transfer right, and closing a channel handle at exit closes the
endpoint.

## Decision

`fork` builds the whole child before its task exists:

1. The family registry reserves a PID under the budgets of ADR 0155. A
   budget refusal is `EAGAIN`.
2. The kernel takes a free Process record and a new address space. The child
   Process copies the parent's ABI, quota limits, region map, and name, and
   starts with an empty handle table and an empty account.
3. Every present user page of the parent is copied into a fresh page the child
   owns, with the same permissions. Untouched demand-zero pages stay absent
   and keep only their regions. The stack guard stays unmapped. A page the
   parent does not own, such as shared memory, makes the call fail.
4. The child's handle table receives the parent's VFS channel capability with
   the same rights. The compatibility state copies the fd table, retaining each
   open-file description, and takes a reference on the family VFS context.
   It copies cwd, brk bounds, signal dispositions, and the blocked mask.
5. The copied pages and page tables are charged to the child. The child takes
   a reference on the parent's executable object.
6. The child task starts at the instruction after `SYSCALL` with the parent's
   general-purpose registers, RSP, RFLAGS, and FS base, and RAX zero.

Any failure before step 6 removes the PID reservation, the compatibility state,
the handles, the registry entry, and the address space. Page-copy or quota
failure is `ENOMEM`. No child becomes visible.

The family VFS context decides when the shared endpoint closes. A process that
exits while other members hold the context removes its handle without closing
the channel. The last member's exit closes it. VFS replies carry no handles, so
the endpoint has no single owning process.

PID 1 holds no handle to a forked Process and receives no lifecycle message
when it exits. The child inherits no pending signal, syscall continuation,
wait registration, or `clear_child_tid` address.

## Consequences

Fork cost grows with the pages the parent has touched, including its image
pages. Two processes never share a writable frame, so no write can leak between
them. A fork can fail with `ENOMEM` when the child quota or physical memory
cannot hold the copy even though the parent runs. FPU and SSE state start from
the initial image in the child. The C calling convention treats those
registers as clobbered across the `fork` call; a changed MXCSR or x87 control
word does not survive into the child.
