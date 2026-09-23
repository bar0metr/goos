# Terminal

The terminal service, `/system/tty`, owns the console's input (ADR 0082). It is
the only process that holds the right to read the console, so nothing else can
see what is typed. It edits the line, echoes it, recognises the control keys,
and hands finished lines to its client, the shell, over a typed session.

```text
keyboard ──UART──▶ kernel ring ──ConsoleRead──▶ tty ──GTTY lines──▶ shell
                                                                      │
                                    command input stream ◀────────────┘
processes ──ConsoleWrite (write-only)──▶ console
```

## Line discipline

`userspace/tty.Discipline` edits one line of at most 128 bytes and allocates
nothing:

| Key | Effect |
| --- | --- |
| printable ASCII | inserted and echoed |
| Backspace, Delete | erase the last character |
| Enter (CR or LF) | complete the line |
| Ctrl-C | discard the line, echo `^C`, answer the read with an interrupt |
| Ctrl-D on an empty line | answer the read with end of input |
| Ctrl-U | erase the whole line |
| Ctrl-L | clear the screen and show the prompt and line again |
| ESC sequences | skipped, so arrow and function keys type no text |

Other control bytes and bytes above 0x7e are ignored. Ctrl-C reaches the service
from the kernel as an interrupt rather than a byte (ADR 0075); the console is
readable while one is pending, so the service wakes for it.

## Session protocol

`GTTY` version 1 runs over a channel PID 1 creates between the service and the
shell. A client sends:

- `ReadLine(prompt)`: show the prompt and answer with the next line, an
  interrupt, or end of input. One read may be outstanding; a second is refused.
- `Redraw(prompt)`: replace the outstanding read's prompt and show it again with
  what has been typed. The shell sends it after output has moved the cursor —
  when a background job is reported, or when a foreground command ends while a
  line was being typed ahead. The line is erased first only if typed text is on
  it.

Prompts are at most 64 bytes of printable ASCII, so a client cannot use the
service to send control sequences. Replies carry at most 128 bytes. A line
completed while no read is outstanding waits in the discipline; an interrupt or
end of input typed then is kept for the next read.

## Command input

The shell keeps a read outstanding while a foreground command runs. PID 1 gives
each command's first stage a stream of its own for standard input and sends the
writing end to the shell in an exec `Started` message before the job's reply
(exec protocol version 8). The shell writes each typed line to it, closes it on
Ctrl-D, and stops the job on Ctrl-C. A background job's input stream is already
at end of input. No command reads the console.

## Output

Output does not pass through the service. Every process that prints holds a
write-only console capability, and the kernel writes each call whole. Routing
output through the service would let it redraw the line under any output, but
it needs many-writer streams the kernel does not have: closing a shared writing
end today ends the stream for every holder.

## Supervision

The service and the shell form one interactive session over a channel created
once, so the service is not restarted on its own, and neither is the shell;
recovering the session as a unit is later work.

## The bootstrap TTY demonstration

The validation build still runs the ADR 0016 demonstration in the kernel self
tests, with `kernel/tty`: two bootstrap processes exchange a TTY write request.
It exercises IPC waits and device rights and is not the terminal service.
