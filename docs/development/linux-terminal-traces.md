# Linux terminal traces: GNU nano and BusyBox sh

This is the Linux behaviour a GoOS terminal layer (termios, `/dev/tty`,
process groups, job control) has to reproduce for a static GNU nano and an
interactive BusyBox `sh`. It was recorded from the real binaries on a real
Linux PTY with scripted keystrokes.

## Binaries

| Binary | SHA-256 | Version |
|---|---|---|
| `nano` (static x86_64) | `fee997775e4c63ebc6ab687ddf1d51175f7d5523b1c1f7075901db99dc318d2d` | `GNU nano, version 9.2`; compiled options `--disable-libmagic --disable-nls --enable-utf8`. `--version` does not report the ncurses version. Its ncurses reads both the legacy and the 32-bit terminfo format, so it is ncurses 6.1 or later. |
| `busybox` (static x86_64) | `9209cc9efa5840f06e0e61f78c1ca4b9a374eb6508ac0bd831cf53181a86f2db` | `BusyBox v1.38.0 (2026-09-01 23:40:50 UTC)`. `ash` has job control on: `jobs`, `fg`, `^Z` and the `Stopped (tty input)` / `Stopped (tty output)` reports all work. |

## How the traces were made

`userspace/linux/trace-tty.sh NANO BUSYBOX OUTDIR [scenario ...]` runs every
scenario in an Alpine x86_64 container. The driver
(`userspace/linux/tty-trace/driver.py`) gives the program a fresh PTY
(`setsid`, `TIOCSCTTY`, 24x80), types the keys, resizes with `TIOCSWINSZ` on
the master (the kernel then sends `SIGWINCH` to the foreground group, as a
terminal emulator's resize does), and snapshots the slave termios and the
foreground process group. `userspace/linux/tty-trace/summarize.py OUTDIR`
prints per-scenario syscall counts, ioctl requests, signal deliveries and,
with `DETAIL=1`, the full lines of the calls that matter here.

The PTY, termios, process groups and signals are the container host's real
Linux kernel. The x86_64 programs run under qemu-user, which writes every
guest syscall when `QEMU_STRACE` is set. Limits of that log:

- `rt_sigaction` shows only pointers, so a handler cannot be told from
  `SIG_IGN`. What a call installs below comes from the program's behaviour.
- `execve` is not logged. Signals that stop a process (`SIGTSTP`, `SIGTTIN`,
  `SIGTTOU`, `SIGSTOP`) are not logged either; the parent's `SIGCHLD` with
  `CLD_STOPPED` shows them.
- `c_cc` is not decoded. The driver's snapshots supply `VMIN`, `VTIME` and
  the control characters.
- Call names are the x86_64 syscall numbers: `open` is syscall 2, not
  `openat`; `stat` is 4; `fork` 57; `vfork` 58.

Raw traces are not in the repository. The run this page describes is under
`/Volumes/Data/tmp/linux-tty-traces/qemu-strace/` (one directory per scenario
with `strace.<pid>.log`, `screen.raw`, `events.json`), with the summaries in
`summary.txt` and `detail.txt` beside it.

| Scenario | What it does | Result |
|---|---|---|
| `nano-start-exit` | `nano -I`, `^X` | exit 0 |
| `nano-open-existing` | `nano -I existing.txt`, `^X` | exit 0 |
| `nano-edit-save` | type text, `^O`, Enter, `^X` | file rewritten in place, mode 0640 kept |
| `nano-new-save` | new file, type, `^O`, Enter, `^X` | file created 0644 (0666 and umask 022) |
| `nano-resize` | 24x80 to 30x100 to 20x60 | redraws at each size |
| `nano-suspend` | `^Z`, then `^T^Z`, then `SIGCONT` from the driver | `^Z` alone only shows `[ To suspend, type ^T^Z ]`; `^T^Z` stops nano |
| `nano-stdout-file` | stdout is a file | `Standard output is not a terminal`, exit 1 |
| `nano-stdin-file` | `nano -I -` with stdin a file | reads stdin, then reopens `/dev/tty` |
| `nano-stdin-devnull` | `nano -I existing.txt < /dev/null` | `Standard input is not a terminal`, exit 1 |
| `nano-term-{linux,xterm,vt100}-nodb` | no terminfo directory present | `Error opening terminal: <name>.`, exit 1 |
| `nano-term-goos`, `nano-term-db-*` | `TERM=goos`, `xterm-256color`, `vt100` from the GoOS tree only | start and quit normally |
| `nano-keys-xterm`, `nano-keys-goos` | Down, End, `X`, Home, Delete, save | both write `first line\necond lineX\n` |
| `busybox-sh-jobs` | `sh -i`: echo, `ls`, `^C` of `sleep`, `^Z`/`jobs`/`fg`, `sleep 1 &` + `wait`, `cat &`, `stty tostop` + `echo bgout &`, a pipeline, `exit` | see below |
| `busybox-sh-nano` | `sh -i` runs nano, `^T^Z`, `jobs`, `fg`, `^X`, `exit` | nano stops and resumes cleanly |

## Distinct syscalls

| Program | Syscalls |
|---|---|
| nano | `access arch_prctl brk clock_gettime close dup2 exit_group fcntl fsync getcwd getegid geteuid getgid getuid ioctl kill mmap munmap open poll read readlink readv rt_sigaction rt_sigprocmask rt_sigreturn set_tid_address setfsgid setfsuid stat write writev` |
| BusyBox (`sh` and the `sleep`, `cat`, `ls`, `stty` applets) | `access arch_prctl brk clock_gettime close dup2 exit_group fcntl fork getcwd getdents64 getegid geteuid getgid getpgid getpid getppid getuid ioctl kill lstat mmap munmap nanosleep open pipe ppoll read readlink readv rt_sigaction rt_sigprocmask rt_sigreturn sendfile set_tid_address setfsgid setfsuid setpgid stat tkill uname vfork wait4 write writev` |

Not called by either program in any scenario: `setsid`, `getsid`,
`select`, `pselect6`, `sigaltstack`, `fdatasync`, `flock`, `fchmod`,
`fchown`, `rename*`, `link*`, `unlink*`, `ftruncate`, `openat`, `waitid`.

## ioctl requests

| Request | Number | Caller and use | Result needed |
|---|---|---|---|
| `TCGETS` | `0x5401` | nano and ncurses on fd 0 and 1; ash line editing on fd 0; `stty` | 0 on a tty, `ENOTTY` (25) otherwise |
| `TCSETS` | `0x5402` | nano on fd 0; ash on fd 0 around every line read | 0 |
| `TCSETSW` | `0x5403` | ncurses on fd 1 (`def_prog_mode`, `endwin`, `reset_prog_mode`); `stty` | 0, after output drains |
| `TIOCGPGRP` | `0x540f` | ash once at start, on its `/dev/tty` descriptor (fd 10) | the foreground group |
| `TIOCSPGRP` | `0x5410` | ash (fd 10) and each foreground child before `execve` | 0 |
| `TIOCGWINSZ` | `0x5413` | ncurses and nano for the size; musl `isatty()` (stdio on every new stream, and nano's checks of fd 0 and 1) | 0 on a tty, `ENOTTY` on a file |
| `VT_GETSTATE` | `0x5603` | nano at start on fd 1, to learn whether it runs on a Linux virtual console | `ENOTTY` on a PTY |

`TIOCSWINSZ` (`0x5414`) is issued only by the driver, on the master. Not
seen at all: `TCSETSF` (`0x5404`), `TCFLSH` (`0x540b`), `TCXONC`
(`0x540a`), `TIOCSCTTY` (`0x540e`), `FIONREAD` (`0x541b`), `TIOCLINUX`
(`0x541c`), `TIOCGSID` (`0x5429`), `KDGKBTYPE` (`0x4b33`), `KDGETMODE`.
In nano's source, `TIOCLINUX` (shift state) is asked only when `VT_GETSTATE`
succeeds,
so a GoOS console that answers `VT_GETSTATE` would also need `TIOCLINUX`;
answering `ENOTTY` keeps nano on the plain terminal path.

## termios

Linux values: `c_iflag` `ICRNL 0x100`, `IXON 0x400`; `c_oflag` `OPOST 0x1`,
`ONLCR 0x4`; `c_lflag` `ISIG 0x1`, `ICANON 0x2`, `ECHO 0x8`, `ECHOE 0x10`,
`ECHOK 0x20`, `TOSTOP 0x100`, `ECHOCTL 0x200`, `ECHOKE 0x800`,
`IEXTEN 0x8000`; `c_cflag` `B38400|CS8|CREAD = 0xbf`. `c_cc` indices:
`VINTR 0`, `VQUIT 1`, `VERASE 2`, `VKILL 3`, `VEOF 4`, `VTIME 5`, `VMIN 6`,
`VSTART 8`, `VSTOP 9`, `VSUSP 10`, `VREPRINT 12`, `VDISCARD 13`,
`VWERASE 14`, `VLNEXT 15`.

| State | `c_iflag` | `c_oflag` | `c_cflag` | `c_lflag` | `VMIN` | `VTIME` |
|---|---|---|---|---|---|---|
| PTY default (cooked) | `0x500` | `0x5` | `0xbf` | `0x8a3b` | 1 | 0 |
| ash reading a line | `0x500` | `0x5` | `0xbf` | `0x8a30` (no `ISIG`, `ICANON`, `ECHO`) | 1 | 0 |
| nano editing | `0x0` | `0x0` | `0xbf` | `0xa30` (`ECHOE ECHOK ECHOCTL ECHOKE`) | 1 | 0 |
| nano reading or writing a file | `0x0` | `0x0` | `0xbf` | `0xa31` (adds `ISIG` so `^C` can cancel) | 1 | 0 |

The control characters stay at the PTY defaults throughout:
`c_cc = 3 28 127 21 4 0 1 0 17 19 26 0 18 15 23 22`.

nano reaches its editing state in steps: ncurses `TCSETSW` on fd 1
clearing `ICRNL` and `ICANON` (`cbreak`), then `ECHO` and `ONLCR`
(`noecho`, `nonl`), then `ISIG`, `IXON` and `IEXTEN` (`raw`); nano then
`TCGETS`/`TCSETS` on fd 0 clearing `OPOST`, and once more with the same
values. On exit ncurses restores
the saved state with `TCSETSW` on fd 1 and nano with `TCSETS` on fd 0.
`VMIN` and `VTIME` are never changed; reads are single-byte and blocking.

## Input waiting and keys

- nano blocks in `read(0, buf, 1)`. ncurses calls `poll(fds, 1, 0)` to check
  for more input before a refresh.
- After an ESC byte ncurses waits for the rest of the sequence with
  `poll(fds, 1, 50)`: `ESCDELAY` is 50 ms. A key sequence must arrive
  within 50 ms of its ESC to decode as a key.
- BusyBox line editing blocks in `ppoll(fds, 1, NULL, ...)`, then reads.
  `ppoll` returns `EINTR` when `SIGCHLD` or `SIGWINCH` arrives.
- No `select`, `pselect6`, `FIONREAD` or non-blocking reads.
- nano clears `O_NONBLOCK` on fd 0 at start (`fcntl(0, F_GETFL)`,
  `fcntl(0, F_SETFL, flags)`).

## Signals

nano installs `SIGWINCH` (28) at start, then `SIGINT`, `SIGQUIT`, `SIGHUP`,
`SIGTERM`, `SIGTSTP` (20), `SIGCONT`, `SIGSEGV`, `SIGABRT`. ncurses queries
and sets `SIGINT`, `SIGTERM` and `SIGWINCH`, and sets and restores
`SIGTSTP` around every screen update, so `SIGTSTP` changes dozens of times
per session. `SIGWINCH` interrupts the blocking `read` with `EINTR` (the
handler has no `SA_RESTART`); nano then calls `TIOCGWINSZ` on fd 1 and
redraws at the new size.

## Suspend

In nano 9.2 `^Z` does not suspend by default. It shows
`[ To suspend, type ^T^Z ]`; `^T^Z` suspends. With `ISIG` off the kernel
never sees `^Z`, so nano suspends itself:

1. ncurses `endwin`: `TCSETSW` on fd 1 and `TCSETS` on fd 0 back to `0x8a3b`.
2. `kill(0, SIGSTOP)`: the whole process group stops.
3. On `SIGCONT` nano's handler re-enters curses mode (`TIOCGWINSZ`,
   `TCSETSW`, `TCSETS` as at start) and redraws.

## Saving a file

| Step | nano 9.2 with `-I` |
|---|---|
| Resolve | `getcwd`, `readlink(path)` (`EINVAL` for a regular file), `stat` |
| Check | `access(path, W_OK)` |
| Write | `open(path, O_WRONLY|O_CREAT|O_TRUNC, 0666)`, `TIOCGWINSZ` on the new fd (`ENOTTY`, from musl stdio), `write` |
| Flush | `fsync(fd)` |
| Close | `close`, `stat` |

The file is rewritten in place; its mode and owner are untouched. There is no
backup, lock file, temporary file, `rename`, `fchmod`, `fchown`, `flock` or
`fdatasync`. Reading a file is `open(O_RDONLY)`, `read`, `close`.

## Terminal checks and `/dev/tty`

- stdout not a terminal: `TIOCGWINSZ(1)` fails with `ENOTTY`, nano prints
  `Standard output is not a terminal` and exits 1 before reading terminfo.
- stdin not a terminal and a file named: nano starts curses, reads the file,
  then prints `Standard input is not a terminal` and exits 1.
- `nano -` with stdin a file: `open("/dev/stdin", O_RDONLY)`, read it, then
  `open("/dev/tty", O_RDONLY)`, `dup2(fd, 0)`, `close(fd)`, and go on with
  fd 0 as the terminal. GoOS needs `/dev/stdin` and `/dev/tty` for this.
- ash at start: `open("/dev/tty", O_RDWR)`, `fcntl(F_DUPFD_CLOEXEC, 10)`;
  fd 10 is its job-control descriptor.

## terminfo lookup

ncurses `stat`s each directory in order: `$TERMINFO`, `$HOME/.terminfo`,
`/etc/terminfo`, `/usr/share/terminfo`, `/lib/terminfo`,
`/usr/lib/terminfo`; then `access(<dir>/<c>/<name>, R_OK)`, `open`, and two
`readv` calls. There is no compiled-in fallback: with no database, even
`TERM=xterm` and `TERM=linux` fail with `Error opening terminal`. GoOS has
to ship a database; `userspace/linux/terminfo/` builds one.

## The GoOS terminfo tree

`userspace/linux/terminfo/goos.src` describes `TERM=goos`: the CSI subset
of the framebuffer and serial console (cursor moves, `CUP`, `CHA`, `ED`,
`EL`, SGR 0 1 4 7 24 27 and 8 colours, `?25l`/`?25h`), `am bce` without
`xenl`, and the key bytes of `kernel/keyboard/decoder.go`.
`userspace/linux/terminfo/build.sh` compiles it with `tic` in an Alpine
container, together with the stock `xterm`, `xterm-256color` and `vt100`,
into `userspace/linux/terminfo/db/` (about 7 KB):

```text
db/g/goos
db/v/vt100
db/x/xterm
db/x/xterm-256color
```

All four use the legacy format (magic 0432); `xterm-256color` is cut to
`pairs#32767` to fit it. Alias links such as `vt100-am` are dropped. Two
runs of `build.sh` give byte-identical files. nano loads the tree with `TERMINFO=<dir>`, or
without any variable when the tree is installed at `/etc/terminfo` or
`/usr/share/terminfo`. With `TERM=goos` and only this tree present, nano
starts, draws with `CUP`, relative moves, `EL`/`ED` and SGR 7, and the
Down/End/Home/Delete test edits the file as expected. nano still writes
`ESC [ ? 2004 h` / `l` (bracketed paste) whatever the entry says; the
console must ignore private modes it does not implement.

## BusyBox ash job control

Start-up as a session leader on the PTY (pid = pgid = sid = 53):

| Call | Result |
|---|---|
| `rt_sigaction(SIGCHLD)`, `SIGHUP`, `SIGINT`, `SIGQUIT`, `SIGTERM` | installed |
| `open("/dev/tty", O_RDWR)`, `fcntl(3, F_DUPFD_CLOEXEC, 10)` | fd 10 |
| `ioctl(10, TIOCGPGRP)` | 53 |
| `getpgid(0)` | 53; equal, so no `SIGTTIN` loop |
| `rt_sigaction(SIGTSTP)`, `rt_sigaction(SIGTTOU)` set | `SIG_IGN` in ash's source; the trace shows only that they are set |
| `rt_sigaction(SIGTTIN)` | queried only; stays default |
| `setpgid(0, 53)` | `EPERM`: a session leader cannot change group; ash ignores it |
| `ioctl(10, TIOCSPGRP, 53)` | 0 |

A foreground command (`sleep 30`, `nano`, `stty`) uses `vfork`:

| Process | Calls |
|---|---|
| shell | `vfork() = C`, `setpgid(C, C)` |
| child | `setpgid(0, C)`, `ioctl(10, TIOCSPGRP, C)`, reset `SIGTSTP SIGTTOU SIGINT SIGTERM SIGQUIT` to default, `execve` |
| shell | `wait4(-1, &st, WUNTRACED, NULL)` (options 2) |
| shell | `wait4(-1, &st, WNOHANG|WUNTRACED, NULL)` (3) until `ECHILD` or 0 |
| shell | `ioctl(10, TIOCSPGRP, 53)`, then `TCGETS`/`TCSETS` for the next line |

Both the parent and the child call `setpgid`, and both the child and
(later) the parent call `TIOCSPGRP`, so whichever runs first wins and the
other's call must succeed as a no-op.

- Background command (`sleep 1 &`, `cat &`) and pipelines use `fork`.
  A background child calls `setpgid(0, C)` but not `TIOCSPGRP`. In a
  pipeline the second process joins the first one's group:
  `setpgid(C2, C1)` in the shell and `setpgid(0, C1)` in the child, and
  every member calls `TIOCSPGRP(C1)`.
- `^C` with `sleep` in front: the kernel sends `SIGINT` (`SI_KERNEL`) to
  the group, `nanosleep` returns `EINTR`, the shell sees `CLD_KILLED`,
  takes the terminal back and re-raises with `tkill(53, SIGINT)` on itself
  (its handler then prints a fresh prompt).
- `^Z`: `SIGTSTP` to the group, the shell's `wait4(WUNTRACED)` returns the
  stopped child (`CLD_STOPPED`, si_code 5) and prints `[1]+ Stopped`.
- `fg`: `ioctl(10, TIOCSPGRP, C)`, `kill(-C, SIGCONT)`, then
  `wait4(WUNTRACED)`. The child's `SIGCHLD` with `CLD_CONTINUED` (6) can
  interrupt that `wait4` with `EINTR`; ash retries.
- `cat &` reading the terminal: `cat` first tries `sendfile(1, 0, ...)`,
  which fails with `EINVAL` between two ttys, then `read`s; the kernel stops it
  with `SIGTTIN`. The shell finds it with `wait4(WNOHANG|WUNTRACED)` and prints
  `Stopped (tty input)`.
- `stty tostop` (`TCSETSW` adding `TOSTOP 0x100`) and `echo bgout &`: the
  background write stops with `SIGTTOU`; ash prints `Stopped (tty output)`.
- `kill -9 %1`: `kill(-C, SIGKILL)`.
- `exit`: `ioctl(10, TIOCSPGRP, 53)`, `setpgid(0, 53)` (`EPERM` again),
  `exit_group(0)`.

`WCONTINUED` (8) is never passed. `SIGTTOU` and `SIGTTIN` are never
delivered to the shell itself, since it is always in the foreground when it
touches the terminal.

## Line editing

ash prints the prompt and echoes typed characters itself; the kernel echo is
off (`c_lflag 0x8a30`). It installs `SIGWINCH` only while a line is being
read. History goes to `$HOME/.ash_history` with
`open(O_WRONLY|O_APPEND|O_CREAT, 0600)` after each line.

## Requirements this sets for GoOS

1. `TCGETS`, `TCSETS`, `TCSETSW` with the full Linux `struct termios`
   (36 bytes: four flag words, `c_line`, 19 `c_cc` bytes in the kernel
   layout), honouring `ICANON`, `ECHO`, `ISIG`, `ICRNL`, `OPOST`/`ONLCR`,
   `TOSTOP`, `VMIN`/`VTIME` = 1/0, and the `VINTR`/`VSUSP`/`VQUIT`
   characters.
2. `TIOCGWINSZ` on every terminal descriptor; `ENOTTY` from all of these on
   regular files and pipes, since musl `isatty()` depends on it.
3. `VT_GETSTATE` answered with `ENOTTY`.
4. `/dev/tty` (`O_RDWR` and `O_RDONLY`) and `/dev/stdin`.
5. Process groups and sessions: `setpgid` (with `EPERM` for a session
   leader), `getpgid`, `TIOCGPGRP`, `TIOCSPGRP`, `kill` with negative pids
   and with 0, `tkill`.
6. `SIGINT`/`SIGTSTP`/`SIGQUIT` from the line discipline to the foreground
   group; `SIGTTIN`/`SIGTTOU` stops for background readers and writers
   (with `TOSTOP`); `SIGWINCH` to the foreground group on a size change.
7. `wait4` with `WUNTRACED` and `WNOHANG`, `SIGCHLD` with `CLD_EXITED`,
   `CLD_KILLED`, `CLD_STOPPED`, `CLD_CONTINUED`, and `EINTR` from `read`,
   `ppoll` and `wait4` when a handler without `SA_RESTART` runs.
8. `poll` with 0 and 50 ms timeouts and `ppoll` with no timeout on a
   terminal.
9. `sendfile` from a terminal failing with `EINVAL`, so `cat` falls back to
   `read`.
10. A terminfo database on nano's search path, or `TERMINFO` set.
