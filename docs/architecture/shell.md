# Native shell

PID 1 launches `/system/shell` as a normal supervised native image after the
crash-policy boot probe completes. Its capability manifest contains one device
handle derived from PID 1 with `Read|Write` rights and without `Transfer`.

The shell resolves its `console`, `vfs`, and `exec` handles from the versioned
startup block, then reads one serial byte at a time through the
capability-checked console session ABI. The bounded TTY line editor supplies printable input, backspace,
delete, CR/LF completion, and a 128-byte maximum command line. Runtime
allocation remains confined to the fixed userspace arena.

The built-ins are `help`, `clear`, and `cd`. `clear` emits ANSI terminal
control. Commands are split into at most eight arguments with bounded single
quotes, double quotes, and backslash escaping. The shell owns a fixed 192-byte working-directory buffer matching the
inline `GVF1` path limit and resolves
relative paths without heap allocation. `cd` commits a new directory only after
VFS `Stat` confirms it. Other command names are resolved beneath the fixed
`/system/bin/` search root; shell sends the executable path, structured bounded
argument and environment vectors, and current directory to PID 1 and waits through a process
handle. PID 1 independently validates the system path and executable image.
The shell similarly restores its post-startup arena mark after every completed
command, so repeated IPC commands cannot exhaust the non-freeing TinyGo heap.

The QEMU boot test writes real commands to serial stdin and requires the shell's
responses in serial output. The shell receives non-transferable VFS and exec
endpoints and cannot delegate them. Every external utility receives a fresh VFS
session instead of sharing the shell reply queue. Fast exit notifications that
arrive before a launch reply are retained in a bounded PID 1 queue. The exec
reply distinguishes launch status from the final child exit code. The native
base utilities receive structured `argv`, `PATH=/system/bin`, a fresh VFS
session, a read-only standard-input handle named `in`, and distinct write-only
`stdout` and `stderr` handles. Normal output uses `stdout`; non-zero-exit
diagnostics use `stderr`. All three currently target the serial console, so
observable terminal behavior is unchanged. The bounded `readline` utility reads
one line from `in`, rejects input beyond 128 bytes, and writes accepted bytes to
`stdout`. This validates the inherited read-only input capability independently
of the shell's own console handle.
One unquoted `|` splits a command into two bounded invocations. PID 1 creates a
private stream, launches both children before waiting, gives only the writer to
the producer and only the reader to the consumer, and returns the first failing
exit code. Quoted and escaped `|` bytes remain ordinary argument data. File
redirection, longer pipelines, job control, and a persistent userspace TTY
server remain later work.
