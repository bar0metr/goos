# Roadmap

The implementation follows the specification milestones. UEFI boot information,
`ExitBootServices`, the bootstrap physical page allocator, kernel-owned page
tables, NX, and W^X enforcement are complete. The next slice adds a GDT/IDT,
exceptions, and a timer. Later milestones add timer-preempted
tasks, isolated userspace processes, versioned IPC and capability transfer,
then userspace TTY, RAMFS, VFS, init, and a native shell.

Linux ELF compatibility begins only after the native security and process
model is stable. It is not part of the native ABI.
