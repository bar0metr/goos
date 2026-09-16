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
generation-safe RAMFS with its versioned service protocol, and a namespace-
scoped VFS with mount routing, path resolution, open-file objects, caches, and
its versioned IPC protocol are complete. The bounded init supervision policy
core, UEFI initramfs discovery, ring-3 PID 1 launch, and task-to-process death
notifications are also complete. PID 1 now receives lifecycle events and drives
a real service restart through correlated, capability-protected IPC. Launch
requests carry a validated absolute executable path; the kernel uses a
bounded process pool, assigns fresh process identifiers, validates the selected
native image, and creates its address space and initial task. A freestanding
amd64 TinyGo runtime now executes the complete supervision policy loop in PID 1;
the QEMU path exercises three crash/restart cycles and userspace rate limiting.
Declared capability manifests are serialized into launch requests and installed
as non-escalating derived handles before a child Task is created. PID 1 now
launches a native interactive shell with capability-checked serial input/output,
bounded line editing, and the `help`, `echo`, `clear`, and `uptime` built-ins.
The shell is now connected to a supervised userspace VFS service through an
explicit pair of channel endpoints. It maintains a bounded working directory
and implements `pwd`, `ls`, `cd`, and `cat` over correlated `GVF1` requests.
The next shell slice adds native executable launch without introducing ambient
filesystem or process authority.

Linux ELF compatibility begins only after the native security and process
model is stable. It is not part of the native ABI.
