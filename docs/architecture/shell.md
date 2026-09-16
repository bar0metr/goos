# Native shell

PID 1 launches `/system/shell` as a normal supervised native image after the
crash-policy boot probe completes. Its capability manifest contains one device
handle derived from PID 1 with `Read|Write` rights and without `Transfer`.

The shell reads one serial byte at a time through the capability-checked console
session ABI. The bounded TTY line editor supplies printable input, backspace,
delete, CR/LF completion, and a 128-byte maximum command line. Runtime
allocation remains confined to the fixed userspace arena.

The initial built-ins are `help`, `echo`, `clear`, and `uptime`. `clear` emits
ANSI terminal control, while `uptime` reads the kernel's monotonic 100 Hz tick
counter. Unknown commands return a bounded diagnostic and the prompt remains
active.

The QEMU boot test writes real commands to serial stdin and requires the shell's
responses in serial output. Filesystem commands, process launch from the shell,
pipes, job control, and a persistent userspace TTY server remain future work.
