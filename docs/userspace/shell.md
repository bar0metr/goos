# Native shell

PID 1 launches `/system/shell` as a normal supervised native image after the
terminal service. The shell holds no console capability (ADR 0089): what is
typed reaches it as lines from the terminal service over its `tty` session
(ADR 0082), and what it prints goes back through the same service on `stdout`,
a write-only copy of the terminal's output stream. It assembles its output into
lines and sends each as one write, flushing before it asks for a line or waits.
Reading the system log is a separate `log` capability.

The shell resolves its `stdout`, `tty`, `log`, `vfs`, and `exec` handles from
the versioned startup block and asks the terminal service for each line with
its prompt. Editing, echo and the control keys belong to the terminal service
([tty.md](tty.md)); a command line is at most 128 bytes. Runtime allocation
remains confined to the fixed userspace arena.

The built-ins are `help`, `clear`, `cd`, `services`, `handles`, `caps`,
`config`, `log`, `jobs`, `kill`, `reboot`, `diag`, and `time`. Every command,
builtin or executable, is documented in one table in `userspace/shell`: `help`
lists them with a summary, `help NAME` and `NAME --help` print the synopsis, and
a command given the wrong number of arguments prints `usage: ...` instead of
running (ADR 0079). `services` and `reboot` are requests to PID 1
over the control protocol on the exec channel. `handles` and `caps` list the
shell's own handle table, raw and under its startup names. `log` prints the
system log (ADRs 0059 and 0061). `diag`, in a `goos_diag` build, asks PID 1 to
have the kernel dump its state to the console, since the shell cannot write the
console itself. `clear` emits ANSI terminal control. `time` prefixes a whole command line, runs it through
the normal path, and prints the elapsed time from the kernel's nanosecond
clock as `time: <N> us` (ADR 0054). Commands are split into at most eight arguments with bounded single
quotes, double quotes, and backslash escaping. The shell owns a fixed 192-byte working-directory buffer matching the
inline `GVF1` path limit and resolves
relative paths without heap allocation. `cd` commits a new directory only after
VFS `Stat` confirms it. Other command names are resolved beneath the fixed
`/system/bin/` search root; a command word with a slash is a program path,
absolute or relative to the working directory, such as `/home/demo/args` or
`./args` (ADR 0097). The shell sends the executable path, structured bounded
argument and environment vectors, and current directory to PID 1 and waits
through a process handle. PID 1 independently validates the path: under
`/system` it looks in the image, elsewhere it reads the file into an
executable object, and the kernel validates the executable image either way.
The shell similarly restores its post-startup arena mark after every completed
command, so repeated IPC commands cannot exhaust the non-freeing TinyGo heap.

The QEMU boot test writes real commands to serial stdin and requires the shell's
responses in serial output. The shell receives non-transferable VFS and exec
endpoints and cannot delegate them. Every external command whose manifest asks
for filesystem access receives a fresh VFS session instead of sharing the shell
reply queue. Fast exit notifications that
arrive before a launch reply are retained in a bounded PID 1 queue. The exec
reply distinguishes launch status from the final child exit code. The native
base utilities are separate images under `userspace/bin`, each with a manifest
the build collects into `/system/commands` (ADR 0080). They receive structured
`argv`, `PATH=/system/bin`, a VFS session when their manifest asks for one, a
read-only standard-input handle named `in`, and distinct write-only
`stdout` and `stderr` handles. Normal output uses `stdout`; diagnostics and
validation markers use `stderr`. Both are write-only copies of the terminal's
output stream, which the terminal service relays to the console, unless pipeline
or redirection composition substitutes an endpoint (ADR 0089). No command holds a
console capability. Standard input is always a
stream: a foreground command's is fed by the shell with the lines typed while it
runs, and a background command's is at end of input. The bounded `readline`
utility reads one line from `in`, rejects input beyond 128 bytes, and writes
accepted bytes to `stdout`.
`stat [-b|-k|-m|-g] PATH` prints a path's type and size in KiB by default, or
in bytes, MiB or GiB, with two decimals and the exact byte count:
`file 2.46 KiB (2519 bytes)`. A directory's size is the total of the files
under it at any depth, followed by how many files and directories it holds;
mounts inside it are counted like the directories they appear as. The walk
keeps one cursor per level in fixed arrays, so its memory does not grow with
the tree, and costs one VFS request per entry.
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
Stream merging such as `2>&1` and a persistent userspace TTY server remain
later work.

A line ending in an unquoted `&` runs as a background job with an empty
standard input. The shell prints `[N] started`, returns to the prompt, and
reports `[N] done`, `[N] exit status: C`, or the launch failure when PID 1
replies. The terminal service takes the line being typed off the screen, writes
the report, and shows the line again (ADR 0089). Up to three background jobs may run,
and one exec slot stays free for the foreground. Builtins and `;` sequences
cannot run in the background. A foreground command has no reply time limit
(ADR 0064).

`jobs` lists the running background jobs with their command lines, and
`kill %N` (or `kill N`) stops one; the job is then reported as `[N] killed`.
Ctrl-C stops the foreground command and prints `^C`, and at the prompt it
clears the line being typed (ADR 0067). A line typed while a command runs is
that command's input; Ctrl-D ends it. Typing ahead of a command that does not
read keeps the text for the next prompt (ADR 0082).

The shell is `restart: always` and restarts independently of the terminal
service (ADR 0089). When the terminal service dies, the shell keeps running —
its jobs, its working directory, its output, which waits in the terminal's
stream — and takes the new terminal's session from PID 1 as a notice on its exec
channel, then asks for its next line through it. When the shell itself dies, its
running jobs are stopped with it, and the new shell starts with a fresh
terminal session, exec channel, VFS session and configuration session, and no
jobs.

Up to eight commands or bounded pipelines may be separated by unquoted `;`
within the existing 128-byte line limit. A trailing separator is accepted; empty
intermediate commands are rejected. The entire line is checked for balanced
quotes, escaping, argument bounds, and pipeline syntax before any command runs.
Quoted or escaped semicolons remain data. Builtins cannot be pipeline stages.
Each command completes before the next starts; a runtime failure is reported
and does not stop the sequence. `cd` accepts a quoted path and changes the cwd
inherited by subsequent commands. The shell resets its arena after each command.
This syntax does not implement conditional execution (`&&` or `||`).

Diagnostics name what failed. A utility reports `cmd: path: reason` on stderr,
with the reason taken from the VFS status — `no such file or directory`, `not a
directory`, `directory not empty`, `read-only filesystem`, `a mount depends on
it` — and exits with status 2. A command PID 1 cannot start is reported by the
name of the stage that failed, `NAME: command not found`, even inside a pipeline
(ADR 0079). `uptime` prints `up H:MM:SS` and `mem` prints total, used and free
memory in binary units.
