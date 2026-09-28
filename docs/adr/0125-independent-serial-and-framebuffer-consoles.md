# ADR 0125: Independent serial and framebuffer consoles

## Status

Accepted. Supersedes the single logical terminal of ADR 0082 and ADR 0123 for
interactive use, and the shared-size rule ADR 0124 first gave `gfm`.

## Context

GoOS 0.1.5 mirrored one byte stream to COM1 and to the framebuffer, and fed
both keyboards into one input ring. One terminal service and one shell served
both screens. That worked for line-oriented output, since each screen wraps
text at its own width.

`gfm` addresses cells. The framebuffer at 1280x800 has 106x33 of them; a host
terminal on COM1 has whatever size its window has, 136x43 or 86x43 in practice,
and it can change at any moment. With one stream, a layout that fills the host
terminal wraps and scrolls on the framebuffer, and a layout that fits the
framebuffer leaves the host window partly empty. The first fix drew on the
smaller of the two sizes, which kept both screens intact but let neither grow.

## Decision

COM1 and the framebuffer become two consoles, each with its own terminal
service and its own shell, the way a serial line and a virtual console get one
login each in Unix systems.

### Kernel

Two device objects join the logical console:

| Object | Writes reach | Input | ConsoleInfo |
| --- | --- | --- | --- |
| `ConsoleObject` (1) | COM1 and the framebuffer | none | the framebuffer's size, flags for both |
| `SerialConsoleObject` (6) | COM1 | UART ring, Ctrl-C from COM1 | 80x25, serial flag |
| `GraphicalConsoleObject` (7) | the framebuffer | PS/2 ring, Ctrl-C from the keyboard | the framebuffer's size; `NotFound` without one |

The IRQ4 handler fills ring 0 and the IRQ1 handler ring 1. Each ring keeps its
own head, tail, drop count and interrupt count, and one set of assembly
routines serves both by index. `ConsoleRead`, `ConsoleInterrupt` and `WaitAny`
accept only the two terminal objects, so each reader sees one keyboard. The
logical console keeps its role for the kernel's warnings, panics, PID 1's boot
lines and the system services' reports, which land on both screens as before.
Any of the three objects authorizes `LogWrite`.

PID 1 receives the serial console at handle `0x19`, the graphical console at
`0x1a`, and a second exec page at `0x1b`, each with Read, Write and Transfer.

### PID 1

PID 1 keeps a `terminalSession` per console: service names, console handle,
output stream, terminal control channel, exec channel and page, first-launch
flags, and the process handles the validation build uses for faults. The COM1
session keeps the names `tty` and `shell`, the exec channel the kernel created
at boot, and the first exec page, so the boot test drives it as before. The
framebuffer session runs `tty-fb` and `shell-fb` when `ConsoleInfo` on the
graphical console reports a framebuffer; PID 1 creates its exec channel and
opens its filesystem session.

Every session-scoped path now takes a session: job records name the shell that
submitted them, slots are numbered per shell, `kill` and `fault tty|shell`
reach only the caller's session, a shell's exit orphans only its own jobs, and
a restarted terminal service or shell is handed channels within its session.
After a VFS or configuration restart, every running shell receives a new
session.

### Terminal service

The same `/system/tty` binary runs once per console and does not know which
one it serves. It reports its console's geometry with the display flag set
only for the framebuffer, so the shell passes `COLUMNS` and `LINES` to the
framebuffer session's commands and none to the COM1 session's.

The QEMU tests read COM1 alone. In the validation build PID 1 grants `tty-fb`
a write-only copy of the serial console named `mirror`, and the service copies
its output there from the first key typed at the framebuffer console. A boot
that never touches the keyboard shows only the COM1 session on COM1, so the
boot test's marker counts hold.

### Full-screen programs

A program on the framebuffer console lays itself out on `COLUMNS` and `LINES`.
A program on COM1 has no size from the system: it asks the host terminal with
`ESC[999;999H ESC[6n` and reads the cursor position report as input. `gfm`
asks at start, on F2 and after each idle second, and fills whatever size comes
back.

## Consequences

- `gfm` on COM1 fills a 136x43 host window and follows it within a second of a
  resize; `gfm` in the QEMU window fills 106x33. Neither screen shows the
  other's session.
- A machine without a framebuffer runs the COM1 session alone.
- The system runs four interactive processes instead of two. `services` lists
  `tty-fb` and `shell-fb`.
- Typing in the QEMU window no longer reaches the COM1 shell, and the reverse.
- Kernel warnings and service reports still appear on both screens, through
  the logical console, and can land in a line being typed on either.
- `tools/stackframes` accepts `LEAQ n(SP), SP` as a constant adjustment: LLVM
  releases a frame that way before a conditional tail call in the terminal
  service.
