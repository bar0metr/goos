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
controlled shared-memory objects, capability-checked IPC kernel calls, bounded
channel objects, scheduler-integrated IPC wait/deadline bookkeeping, blocking
native receive, restartable kernel-call frames, message wakeup, and timer-driven
timeout wakeup, a capability-isolated userspace TTY service, its versioned IPC
protocol, bounded line editing, basic terminal-control operations, and a bounded
generation-safe RAMFS with its versioned service protocol are complete. The
next slice introduces VFS, followed by init and a native shell.

Linux ELF compatibility begins only after the native security and process
model is stable. It is not part of the native ABI.
