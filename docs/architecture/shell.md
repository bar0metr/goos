# Native shell

PID 1 launches `/system/shell` as a normal supervised native image after the
crash-policy boot probe completes. Its capability manifest contains one device
handle derived from PID 1 with `Read|Write` rights and without `Transfer`.

The shell resolves its `console`, `vfs`, and `exec` handles from the versioned
startup block, then reads one serial byte at a time through the
capability-checked console session ABI. The bounded TTY line editor supplies printable input, backspace,
delete, CR/LF completion, and a 128-byte maximum command line. Runtime
allocation remains confined to the fixed userspace arena.

The built-ins are `help`, `clear`, `cd`, `services`, `handles`, `caps`, `log`,
`reboot`, `diag`, and `time`. `services` and `reboot` are requests to PID 1
over the control protocol on the exec channel. `handles` and `caps` list the
shell's own handle table, raw and under its startup names. `log` prints the
system log (ADRs 0059 and 0061). `clear` emits ANSI terminal control. `time` prefixes a whole command line, runs it through
the normal path, and prints the elapsed time from the kernel's nanosecond
clock as `time: <N> us` (ADR 0054). Commands are split into at most eight arguments with bounded single
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
`stdout` and `stderr` handles. Normal output uses `stdout`; diagnostics and
validation markers use `stderr`. Streams target the serial console unless
pipeline or redirection composition substitutes an endpoint. The bounded
`readline` utility reads
one line from `in`, rejects input beyond 128 bytes, and writes accepted bytes to
`stdout`. This validates the inherited read-only input capability independently
of the shell's own console handle.
Up to three unquoted `|` operators split a command into at most four bounded
invocations. PID 1 creates private streams, starts all children before waiting,
and gives each stage only its adjacent directional endpoints. It returns the
first failing exit code in left-to-right order. Quoted and escaped `|` bytes
remain ordinary argument data. One unquoted `>` or `>>` redirects stdout to a
single bounded VFS path. The shell adds an isolated `write` consumer to the
pipeline; it receives only the upstream reader endpoint and a fresh VFS session,
then truncates the target for `>` or seeks to its current size for `>>` before
draining the stream and exiting on EOF. This reuses typed
handles and does not add POSIX file descriptors. A redirected pipeline may
contain at most three user stages because the sink occupies the fourth bounded
exec slot. One unquoted `2>` or `2>>` target adds a second `write` consumer for the last
requested stage's diagnostics. It stays outside the stdout chain: the shell names
the diverted stage in the exec request, and PID 1 connects that stage's `stderr`
role to the sink through a private stream. The `2` is an operator only at the
start of a word, so arguments ending in `2` remain data. One unquoted `<` target prepends the existing `cat` utility as a file
producer with its own VFS session; its stdout stream becomes the first requested
stage's stdin. Input and output redirection may be combined while staying within
the four-stage bound. Quoted or escaped operator bytes remain argument data.
Stream merging such as `2>&1`, stopping jobs, and a persistent userspace TTY
server remain later work.

A line ending in an unquoted `&` runs as a background job with an empty
standard input. The shell prints `[N] started`, returns to the prompt, and
reports `[N] done`, `[N] exit status: C`, or `[N] launch failed: S` when PID 1
replies, redrawing the line being typed. Up to three background jobs may run,
and one exec slot stays free for the foreground. Builtins and `;` sequences
cannot run in the background. A foreground command has no reply time limit
(ADR 0064).

Up to eight commands or bounded pipelines may be separated by unquoted `;`
within the existing 128-byte line limit. A trailing separator is accepted; empty
intermediate commands are rejected. The entire line is checked for balanced
quotes, escaping, argument bounds, and pipeline syntax before any command runs.
Quoted or escaped semicolons remain data. Builtins cannot be pipeline stages.
Each command completes before the next starts; a runtime failure is reported
and does not stop the sequence. `cd` accepts a quoted path and changes the cwd
inherited by subsequent commands. The shell resets its arena after each command.
This syntax does not implement conditional execution (`&&` or `||`).
