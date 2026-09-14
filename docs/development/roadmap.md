# Roadmap

The implementation follows the specification milestones. The next milestone
adds boot information capture, a physical page allocator, page tables with NX
and W^X enforcement, an IDT, and a timer. Later milestones add timer-preempted
tasks, isolated userspace processes, versioned IPC and capability transfer,
then userspace TTY, RAMFS, VFS, init, and a native shell.

Linux ELF compatibility begins only after the native security and process
model is stable. It is not part of the native ABI.
