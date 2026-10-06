# ADR 0183: TLB shootdown forces a kernel entry

## Status

Accepted. ADR 0184 replaced the kernel lock the initiator held with the
process's address-space lock.

## Context

Threads of one process run on several processors at once, so a processor
can hold translations of a page another processor just unmapped. If the page
goes back to the allocator first, the stale translation reaches its next
owner. Local `INVLPG` covered one processor and no longer suffices.

GoOS separates kernel and process roots (ADR 0120): every kernel entry loads
the kernel root, it maps no global pages, and it uses no PCIDs. The kernel
reaches user memory through the direct map.

## Decision

A processor holds translations of a process root only while it runs in ring
3 on that root, because the CR3 write at each kernel entry drops them. A
shootdown therefore has one job: make each processor that may be in ring 3
on the root enter the kernel once.

Each processor records the root it is about to return to before the CR3
write that loads it, and at each entry clears the record and counts the entry
after the CR3 write that leaves it. The initiator holds the process's
address space for writing, so no other change to the root and no other
shootdown of it runs alongside; it finishes its page-table changes, fences, reads which processors run the root,
sends them the shootdown vector, and waits until each has entered the kernel
or left the root. A processor spinning with interrupts disabled is in the
kernel, its record is clear, and nobody waits for it. A shootdown that does
not complete within its bound stops the machine: the protocol cannot fail.

`UnmapUserRange` clears the leaves in a first pass, keeping each owned
page's address in the cleared entry, runs the shootdown, and frees the pages
in a second pass. `ProtectUserRange` runs one after changing permissions.
A fault on a page whose entry already allows the access came from a
translation another processor has replaced, and the fault resolver retries
it, as it does when another thread mapped the page first.

Changes to the kernel root after boot are the guard pages of new task
stacks. They bump a generation, and a processor whose generation is behind
flushes its TLB at its next switch, before it can run the task.

## Consequences

A shootdown costs one interrupt per processor that runs the process, and
nothing for processors that do not. Exec and process destruction need no
shootdown of their own: they run after every task of the old root is off its
processor. PCIDs or global pages would invalidate the premise and would need
a request mailbox with explicit invalidation instead.
