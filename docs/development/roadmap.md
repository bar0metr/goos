# Roadmap

The implementation follows the specification milestones. UEFI boot information,
`ExitBootServices`, the bootstrap physical page allocator, kernel-owned page
tables, NX, W^X enforcement, the GDT/IDT, the initial CPU exception path, a
100 Hz bootstrap timer, timer-preempted kernel tasks, guarded stacks, and basic
block/wake lifecycle, the Process model, typed generation-safe handles, and
distinct process page-table roots are complete. The next slice adds ring 3
execution, followed by versioned IPC and capability transfer,
then userspace TTY, RAMFS, VFS, init, and a native shell.

Linux ELF compatibility begins only after the native security and process
model is stable. It is not part of the native ABI.
