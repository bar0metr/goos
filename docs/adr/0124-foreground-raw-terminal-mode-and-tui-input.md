# ADR 0124: Foreground raw terminal mode and TUI input

## Status

Accepted.

## Context

GoOS 0.1.6 adds its first full-screen program, the `gfm` file manager. Until
now every program read standard input a line at a time: the terminal service
(ADR 0082, ADR 0089) owned the console, edited and echoed what was typed, and
the shell wrote each finished line to the foreground command's input stream.
A full-screen program has to react to each key as it is pressed — arrows, Tab,
Escape, function keys — and draw every cell itself, and nothing in the stack
supported that:

- the keyboard decoder produced at most three bytes per key, enough for
  ESC [ A but not for an xterm function key such as ESC [ 1 1 ~, and the IRQ1
  path packed those three bytes into one return register;
- the framebuffer terminal parsed one CSI parameter and knew no colours, so it
  could not position the cursor absolutely (ESC [ row ; col H) or render SGR;
- the terminal service had only a line discipline, and around every write it
  relayed it took the edited line off the screen and put it back, which would
  corrupt a program that addresses the screen itself;
- a command had no way to learn the terminal's size, since `ConsoleInfo`
  requires the console capability, which also allows reading the console.

Giving the program the console, the keyboard or the framebuffer would break
the isolation the previous releases built. Drawing through a special kernel
call would make the program depend on the framebuffer and fail on COM1.

## Decision

### The terminal contract

A full-screen program is an ordinary terminal program: it reads key sequences
on standard input and writes a bounded ANSI subset on standard output. The
same bytes drive the framebuffer and a COM1 host terminal.

Input keys are xterm sequences, and they are part of the GoOS terminal input
contract:

| Key | Sequence | Key | Sequence |
| --- | --- | --- | --- |
| Up, Down, Right, Left | ESC [ A/B/C/D | F1 | ESC [ 1 1 ~ |
| Home | ESC [ H | F2 | ESC [ 1 2 ~ |
| End | ESC [ F | F3 | ESC [ 1 3 ~ |
| Insert | ESC [ 2 ~ | F4 | ESC [ 1 4 ~ |
| Delete | ESC [ 3 ~ | F5 | ESC [ 1 5 ~ |
| PageUp | ESC [ 5 ~ | F6 | ESC [ 1 7 ~ |
| PageDown | ESC [ 6 ~ | F7 | ESC [ 1 8 ~ |
| | | F8 | ESC [ 1 9 ~ |
| | | F9 | ESC [ 2 0 ~ |
| | | F10 | ESC [ 2 1 ~ |
| | | F11 | ESC [ 2 3 ~ |
| | | F12 | ESC [ 2 4 ~ |

Modifiers do not change these sequences. The decoder's output holds up to eight
bytes. IRQ1 passes the decoder a pointer to an eight-byte buffer instead of
packing bytes into its return value, and it enqueues a key's sequence whole or
not at all: a ring without room for every byte drops the key, so a prefix such
as ESC [ 1 never reaches a reader as other keys.

The framebuffer terminal accepts up to eight CSI parameters of up to three
digits; anything longer abandons the sequence. Beyond the existing controls it
renders CUP (`ESC[row;colH`, `f`), CHA (`ESC[nG`), ED 0/1/2, EL 0/1/2, and SGR
0, 1, 22, 30-37, 39, 40-47, 49, 90-97 and 100-107. The operands of 38 and 48
are skipped, other attributes and private sequences such as `ESC[?25l` are
ignored. Colours are the 16 ANSI numbers; the framebuffer maps them through one
fixed palette in the terminal package. Erasing uses the current background.
The RGB values are an implementation detail; no application colour reaches the
kernel.

`userspace/terminal` is the userspace side of this contract: an output builder
for cursor, erase and colour sequences over a caller's buffer, a byte-at-a-time
key parser that also accepts the common alternates COM1 host terminals send,
and display-width helpers. It allocates nothing.

### Raw mode belongs to the session

The terminal service session protocol, `GTTY` version 3, gains three requests
that carry no prompt:

- `RawMode` puts the session in raw mode. A read is then answered with
  `ResultRaw` carrying 1 to 255 typed bytes as soon as there are any. Nothing
  is echoed or edited, and output is relayed as it arrives, without the edited
  line being taken off and put back around it.
- `LineMode` returns the session to line mode and writes the reset sequence
  `ESC[0m ESC[?25h ESC[2J ESC[H`: default colours, a visible cursor, a clear
  screen.
- `Geometry` is answered with `ResultGeometry`: the terminal's columns and rows.

Raw mode ends with `LineMode`, and also when the session is replaced or closed.
A restarted service starts in line mode. Whichever way it ends, the reset is
written, so a program that crashed or was stopped cannot leave the terminal
with its colours or a hidden cursor.

A raw read's bytes are taken so that a part that is not the last never ends in
ESC. Every key reaches the service whole, so a reader can take an ESC that ends
what it read to be the Escape key, without a timeout.

### The shell grants it

Only the shell holds a session, so only the shell can ask for raw mode. It
does so for one case: a foreground job of one stage, with no redirection, whose
command is named without a path and whose manifest in the system image's
command index declares the new `commands.GrantTerminal`. It switches when PID 1
reports the job started, writes each `ResultRaw` to the job's input stream
unchanged, and sends `LineMode` as soon as the job's reply arrives — normal
exit, crash, or kill alike. A program loaded from a path never gets raw mode,
as it never gets manifest grants from PID 1.

Ctrl-C keeps its meaning in raw mode: the kernel still delivers it as an
interrupt, the service answers the read with `ResultInterrupt`, and the shell
stops the job. That is the way out of a full-screen program that has stopped
reading, and it needs no key reserved from the program. A program therefore
cannot use Ctrl-C itself; Escape is its cancel key.

The shell asks for the geometry when it gets a session and, when it is the
framebuffer's, passes it to every command as `COLUMNS` and `LINES`;
`utility.TerminalSize` reads them. A serial-only console has no size of its
own, so the shell passes none. No console capability reaches a command.

### The terminal on COM1 has a size of its own

The framebuffer and COM1 show one byte stream, but a host terminal on COM1 can
be any size and is resized without anything reaching the serial line. A
full-screen program therefore asks it: `terminal.Output.RequestSize` moves the
cursor to `ESC[999;999H`, where every terminal clamps it to its bottom-right
corner, and sends DSR 6 (`ESC[6n`). A host terminal answers with a cursor
position report, `ESC[rows;columnsR`, which arrives as input like a key; the
parser returns it as `KeyPosition`. The framebuffer terminal does not answer.

`gfm` asks when it starts, on F2, and whenever a second passes without input,
so it follows a resized terminal within about a second. It lays itself out on
the framebuffer's size when only that is known, on the host terminal's when
only that is, and on the smaller of the two in each dimension when both are:
then neither console wraps or scrolls. A host terminal larger than the
framebuffer does not enlarge the layout while the framebuffer shows the same
screen. `ESC [ 1 ; m R` stays a modified F3; a report is taken only for a row
above 1, which a corner of a usable screen always is.

`GrantTerminal` carries no handle. A command that has it still holds only its
standard streams and its declared grants; it cannot read the console, reach
the keyboard, map the framebuffer or take raw mode from another job.

## Consequences

- `gfm` renders the same screen on the framebuffer and on an ANSI terminal on
  COM1, and needs no authority beyond its VFS session.
- Raw mode is scoped by construction: it exists only while the shell has a
  raw foreground job, and every path that ends the job ends it.
- A crashed full-screen program leaves a cleared screen and a prompt rather
  than its last frame; a message it printed just before exiting is cleared too.
- Typed bytes pass through the terminal service and the shell before the
  program sees them: one extra IPC hop compared with reading the console.
- Output that other processes write while a program owns the screen, such as
  a background job's report, lands on its display; `gfm`'s F2 repaints.
- Host terminals on COM1 may send sequences GoOS does not generate; the key
  parser accepts the common ones (ESC O, ESC [ 1 ~, ESC [ [ A, modifier
  parameters), but it is not a terminfo implementation.
- The kernel terminal wraps as soon as the last column is written, so a
  full-screen program never writes the bottom-right cell.
- An idle full-screen program writes a size query once a second; with a host
  terminal attached, its answer comes back once a second too.
