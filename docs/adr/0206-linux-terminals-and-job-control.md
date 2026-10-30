# ADR 0206: Linux terminal compatibility, process groups, and job control

## Status

Accepted and implemented.

## Context

GNU nano 9.2 exits with "Standard output is not a terminal" unless
`isatty(1)` holds, and musl builds `isatty` on `TIOCGWINSZ`. Before this
change every `ioctl` answered `ENOTTY`, `/dev/tty` answered `ENXIO`, Linux
processes had no SID or PGID, and the signal layer knew no stop signals. An
interactive BusyBox `sh` needs all of those as well: it opens `/dev/tty`,
moves each foreground child into a group of its own, and waits with
`WUNTRACED`.

The traces in [linux-terminal-traces.md](../development/linux-terminal-traces.md)
fix the set the two programs use. nano asks `TCGETS`, `TCSETS`, `TCSETSW`,
`TIOCGWINSZ`, and `VT_GETSTATE`, blocks in `read(0, buf, 1)`, peeks with
`poll`, saves with `open`, `write`, `fsync`, and `close`, and suspends with
`kill(0, SIGSTOP)`. ash adds `TIOCGPGRP`, `TIOCSPGRP`, `setpgid`, `getpgid`,
`wait4(WUNTRACED)`, `kill(-pgid, SIGCONT)`, and `SIGTTIN` for a background
reader.

GoOS already has terminal sessions with raw and line modes, geometry,
resize propagation, and SSH PTYs. A Linux process holds no native terminal
handle and must not get one.

## Decision

### The native terminal is the transport

GoOS adds no `/dev/pts`, no PTY pair allocation, and no device
filesystem. The native terminal service moves bytes; the Linux personality
interprets them.

When the only stage of a foreground job is a Linux program, PID 1 grants it
a fifth startup role, `linux-tty`: the read end of a stream whose writer PID
1 keeps. `validLinuxLaunch` accepts four roles or exactly these five, the
fifth with `Read|Wait` on a stream. At `InstallProcess` the kernel consumes
the job's input, the session's output and error output, and the control
stream into one terminal record. The process gets descriptors 0, 1, and 2
as one read-write description of kind `FDTerminal`, as a login does. The
description names the record by a generation-safe identity, never by a
handle or stream endpoint.

PID 1 marks the `Started` notice of such a job (exec protocol version 12).
The native shell then puts its session in transport mode, a raw mode the
terminal service gained for this, and writes every key to the job's input.
Ctrl-C, which the terminal service reports out of band in raw mode, becomes
byte `0x03`; a Ctrl-C typed right behind other keys can overtake them. The
shell returns the session to line mode when the job ends. Unlike the end of
raw mode, which clears the screen for a native full-screen command, the end
of transport mode keeps it: the program's output stays, and only after
output with escape sequences does the service reset the colours, show the
cursor, and end an unfinished line before the prompt.

A pipeline stage, a background job, a redirected job, and a remote exec
session keep the stream descriptors they had: their `isatty` stays false.

### Geometry

The control stream carries 16-byte records from `internal/terminalwire`:
magic, version, rows, columns, and a sequence. PID 1 writes the session's
geometry before the program starts and again on each change; the kernel
keeps the newest record. A change sends `SIGWINCH` to the foreground group.
`TIOCGWINSZ` returns the latest size with zero pixel fields. PID 1 sets
neither `COLUMNS` nor `LINES` for such a program: ncurses prefers them to
`TIOCGWINSZ`, and they would pin it to its starting size.

An SSH PTY and the framebuffer report their size. A terminal on the serial
console does not, and a Linux program expects the kernel to know it. Once a
program in a serial transport session has written an escape sequence, the
terminal service asks once a second; plain output stays free of questions. It
saves the cursor, sends `ESC [ 999 ; 999 H ESC [ 6 n`, and restores the
cursor, always between two of the program's sequences. The answer never
reaches the program, and a size that differs takes the path of any other
change: a notice to the shell, the shell's notice to PID 1, a record on the
control stream, `SIGWINCH`. Input is held back only while a question is out,
half a second at most, so an Escape key typed then waits for the rest of that
time on a terminal that does not answer. After three unanswered questions the
service stops asking until the next job. A program that uses the terminal's
one saved cursor loses it to the question.

### The terminal record

`kernel/compat/linux` keeps at most `MaxTerminals` (8) records. A record
holds the four stream ends, termios, the size, the controlling session, the
foreground group, a hangup flag, the line being edited (4095 bytes), and
4 KiB of input waiting for readers. References come from descriptions and
one from the controlling session; the last release drops the four stream
ends. Record fields change under the personality's table lock. The calls
that read or write a terminal hold its family's lock and the stream lock,
in the order ADR 0184 gives.

### Input is processed when it arrives

The line discipline runs when a reader, a poller, or an `ioctl` reaches the
terminal, and on every kernel loop pass for terminals with unread input.
The second path lets Ctrl-C reach a foreground group that never reads, such
as `sleep 30`, within one tick. A reader that finds no input waits on the
input stream, the control stream, and a `KindLinuxTerminal` key the kernel
loop publishes after it processes input.

### Termios

The kernel copies the 36-byte kernel `struct termios` field by field. A new
terminal starts as a Linux PTY does (`iflag` ICRNL, `oflag` OPOST|ONLCR,
`cflag` B38400|CS8|CREAD, `lflag` ISIG|ICANON|ECHO|ECHOE|ECHOK|ECHOCTL|
ECHOKE|IEXTEN, the usual control characters), with IXON clear and IUTF8 set.
Settings keep every bit the program writes; the discipline acts on these:

- input: ISTRIP, IGNCR, ICRNL, INLCR, IUTF8 for erase;
- local: ISIG with VINTR, VQUIT, and VSUSP, which flush input unless NOFLSH;
  ICANON with VERASE, VKILL, VWERASE, VEOF, VEOL, and VEOL2; ECHO, ECHOE,
  ECHOK, ECHOKE, ECHOCTL, ECHONL;
- non-canonical reads complete with VMIN bytes or at once when VMIN is
  zero; VTIME is kept and not acted on;
- output: OPOST with ONLCR turns LF into CR LF, and nothing else changes.

`TCSETS`, `TCSETSW`, and `TCSETSF` apply at once; `TCSETSF` first discards
input. GoOS sees bytes leave for the terminal service and cannot tell when
the terminal shows them, so there is no output queue to drain. The kernel
serves `TCGETS`, the three sets, `TIOCGWINSZ`, `TIOCSWINSZ`, `TIOCGPGRP`,
`TIOCSPGRP`, `TIOCGSID`, `FIONREAD`, `TCFLSH`, and `TCSBRK`. Every other
request, `VT_GETSTATE` among them, is `ENOTTY`, and the last one refused is
kept for diagnostics. None reaches the console, the keyboard, or the native
terminal service.

### /dev/tty

`/dev/tty` keeps its synthetic status. Opening it gives a new description of
the caller's controlling terminal, the one whose session is the caller's
SID, or `ENXIO` without one. A process that called `setsid` has none.

### Sessions and process groups

Each Linux process record has a SID and a PGID, apart from `FamilyID`, which
still bounds what a process may reach. A process PID 1 launches leads its
session and its group; fork copies both, exec keeps them, and a clone
thread shares its process's record. The kernel implements `setsid`,
`setpgid`, `getpgrp`, `getpgid`, and `getsid` with the Linux errors the
traces and `tty-probe` show: a session leader cannot change group, a parent
may move only a child that has not called `execve` (`EACCES` after), and a
group joined must exist in the same session. `TIOCSPGRP` accepts a group of
the terminal's session (`ESRCH` for none, `EPERM` for another session).

`kill(0, sig)` and `kill(-pgid, sig)` reach the running members of the
group in the caller's family and session. A PGID from another family names
nothing. `kill(-1, sig)` stays `EINVAL`.

### Job-control signals

`SIGCONT`, `SIGSTOP`, `SIGTSTP`, `SIGTTIN`, `SIGTTOU`, and `SIGWINCH` join
the deliverable set. The stop signals stop by default; SIGWINCH and SIGCONT
are ignored by default. Sending SIGCONT continues a stopped process before
anything else and discards pending stops; sending a stop discards a pending
SIGCONT. A stop signal the receiver neither blocks nor catches stops it
when it is sent; a blocked one stops it when it is delivered. As on Linux,
SIGTSTP, SIGTTIN, and SIGTTOU sent to an orphaned group are discarded. GoOS
also discards any stop of a launch root: its parent is PID 1, which never
sends SIGCONT, so the stop would hold the terminal session for good. The
root gets SIGCONT instead, as if it had stopped and continued at once, so
nano launched from the native shell redraws after ^T^Z.

A read of the controlling terminal from outside the foreground group sends
SIGTTIN to the reader's group and restarts after it continues; a reader
that ignores or blocks SIGTTIN, or whose group is orphaned, gets `EIO`.
SIGTTOU for background writes is not generated: neither program sets
TOSTOP, and ash ignores SIGTTOU.

### Stopped processes

The scheduler has a `stopped` mask beside `held`. A stopped task keeps its
address space, descriptors, continuation, and pending signals; no processor
picks it, and a wake that arrives meanwhile is kept for when it continues.
A task may stop itself, as `kill(0, SIGSTOP)` does: it leaves the processor
at its next scheduling point. SIGKILL retires a stopped task. The kernel's
own holds and a stop do not clear each other.

`wait4` takes group targets, `WUNTRACED`, and `WCONTINUED`. Each stop and
each continuation is reported once with the Linux status words
(`sig << 8 | 0x7f`, `0xffff`), and raises SIGCHLD with `CLD_STOPPED` or
`CLD_CONTINUED` unless the parent set SA_NOCLDSTOP.

### Session loss

When the native session's input stream closes, the terminal hangs up: reads
return end of file and the foreground group gets SIGHUP. The native job
teardown still ends the whole Linux family when its root ends.

### Processing input in the reader's own call

A reader, poller, or `ioctl` caller that processes input can send a signal
to its own group. The signal then becomes pending for the caller, and the
call returns at once (`EINTR`, or a restart after a handler with
SA_RESTART), so the signal acts at the return.

### Diagnostics

A `DIAG=1` dump prints one `DIAG linux-tty` line per terminal: identity,
family, session, foreground group, size, the termios flags, the hangup
flag, counts of interrupts, suspends, quits, and resizes, and the last
refused ioctl.

### fsync

`fsync` and `fdatasync` on a regular file send the VFS session operation
`OperationSync`, which flushes the persistent volume the way PID 1's sync
control does, and succeed only when the flush did. The flush covers the
volume, not one file. A directory succeeds; a pipe, a terminal, or a device
is `EINVAL`.

### terminfo

nano's ncurses has no compiled fallbacks. The system image carries
`/system/linux/terminfo` with `goos`, `xterm`, `xterm-256color`, and
`vt100`, and PID 1 sets `TERMINFO` to it when the request names none. A
local session gets `TERM=goos`; a remote one keeps the TERM its client
negotiated. The `goos` entry lists only what the framebuffer and serial
console implement, including reverse video, underline, and cursor
visibility, which this change added to the framebuffer terminal.

The two outputs differ after the last column of a row. The framebuffer wraps
at once; the terminal on COM1 keeps the cursor on the row until the next
character. An entry with `am` and no `xenl` tells ncurses the cursor is on the
next row, and on COM1 its next relative move lands one row too high: ncdu's
first row after the full-width selection bar overwrote that bar. The entry has
`xenl`, so ncurses treats the cursor as unknown and moves it absolutely, which
is right on both. The framebuffer ignores a newline right behind a wrap, which
is what `xenl` promises, and a carriage return there goes back to the full row,
so a row of exact width followed by CR LF ends on the same row on both outputs.
Other controls after a full row still differ: the native line editor relies on
`\b` and `ESC[A` starting from the next row. On the bottom row the framebuffer
has already scrolled. ncurses moves absolutely there and meets neither case.
`rmam` and `smam` (DECAWM, also new on the framebuffer)
let ncurses fill the bottom-right cell, which it otherwise leaves blank. The
service turns autowrap back on when transport mode ends.

## Consequences

`isatty` is true for an interactive Linux launch and false for pipes,
files, and devices, through the `ioctl` path alone. nano runs without
patches, launched from the native shell or from BusyBox `sh`.

Programs that read their standard input to the end now see a terminal when
they run alone in the foreground. `abi-probe` checks `/dev/tty` and its own
group against that.

Deferred until a program needs them: PTY pairs, `TIOCSCTTY`, `TIOCNOTTY`,
output flow control (IXON), VTIME timeouts, VLNEXT and VREPRINT, TOSTOP and
SIGTTOU, `TIOCLINUX` and the VT ioctls, and the alternate screen.
