# Roadmap

The implementation follows the specification milestones. UEFI boot information,
`ExitBootServices`, the bootstrap physical page allocator, kernel-owned page
tables, NX, W^X enforcement, the GDT/IDT, the initial CPU exception path, a
100 Hz bootstrap timer, and timer-preempted kernel tasks are complete. The next
slice adds task lifecycle and blocking primitives, isolated userspace processes,
versioned IPC and capability transfer,
then userspace TTY, RAMFS, VFS, init, and a native shell.

Linux ELF compatibility begins only after the native security and process
model is stable. It is not part of the native ABI.
