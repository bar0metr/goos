# Roadmap

The implementation follows the specification milestones. UEFI boot information,
`ExitBootServices`, and the bootstrap physical page allocator are complete. The
next slice adds page tables with NX and W^X enforcement, an IDT, exceptions,
and a timer. Later milestones add timer-preempted
tasks, isolated userspace processes, versioned IPC and capability transfer,
then userspace TTY, RAMFS, VFS, init, and a native shell.

Linux ELF compatibility begins only after the native security and process
model is stable. It is not part of the native ABI.
