# Native shell

PID 1 launches `/system/shell` as a normal supervised native image after the
crash-policy boot probe completes. Its capability manifest contains one device
handle derived from PID 1 with `Read|Write` rights and without `Transfer`.

The shell reads one serial byte at a time through the capability-checked console
session ABI. The bounded TTY line editor supplies printable input, backspace,
delete, CR/LF completion, and a 128-byte maximum command line. Runtime
allocation remains confined to the fixed userspace arena.

The built-ins are `help`, `echo`, `clear`, `uptime`, and `cd`. `clear` emits
ANSI terminal control, while `uptime` reads the kernel's monotonic 100 Hz tick
counter. The shell owns a fixed 192-byte working-directory buffer matching the
inline `GVF1` path limit and resolves
relative paths without heap allocation. `cd` commits a new directory only after
VFS `Stat` confirms it. `pwd`, `ls`, and `cat` are external native executables
under `/system/bin`; shell sends their executable path, raw argument, and current
directory to PID 1 and waits for process completion. Unknown commands return a
bounded diagnostic and the prompt remains active.
The shell similarly restores its post-startup arena mark after every completed
command, so repeated IPC commands cannot exhaust the non-freeing TinyGo heap.

The QEMU boot test writes real commands to serial stdin and requires the shell's
responses in serial output. The shell receives non-transferable VFS and exec
endpoints and cannot delegate them. Every external utility receives a fresh VFS
session instead of sharing the shell reply queue. Pipes, job control, and a
persistent userspace TTY server remain future work.
