# Roadmap

The implementation follows the specification milestones. UEFI boot information,
`ExitBootServices`, the bootstrap physical page allocator, kernel-owned page
tables, NX, W^X enforcement, the GDT/IDT, the initial CPU exception path, a
100 Hz bootstrap timer, timer-preempted kernel tasks, guarded stacks, and basic
block/wake lifecycle, the Process model, typed generation-safe handles, and
distinct process page-table roots, a guarded TSS/RSP0 path, the first CPL 3
native program, a typed native kernel-call dispatcher, validated bounded user-
memory copying, process-aware CR3 scheduling, task-local RSP0 stacks, native
Exit lifecycle, isolated user-fault termination, and a validated versioned
native image loader, versioned typed IPC, bounded request/reply queues,
correlation validation, handle passing, peer-death reporting, and capability-
controlled shared-memory objects are complete. The next slice introduces IPC
kernel calls with scheduler wait/timeout integration, followed by userspace TTY,
RAMFS, VFS, init, and a native shell.

Linux ELF compatibility begins only after the native security and process
model is stable. It is not part of the native ABI.
