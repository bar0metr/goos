# Terminal

The terminal service, `/system/tty`, owns the console (ADR 0082, ADR 0089). It
is the only process that holds the right to read the console, so nothing else
can see what is typed. It edits the line, echoes it, recognises the control
keys, and hands finished lines to its client, the shell, over a typed session.
It is also the only process that reads what the shell and its commands print,
and it writes that to the console around the line being typed.

```text
keyboard ──UART──▶ kernel ring ──ConsoleRead──▶ tty ──GTTY lines──▶ shell
                                                 ▲                    │
                                                 │  command input ◀───┘
shell, commands ──terminal output stream─────────┘──ConsoleWrite──▶ console
```

## Line discipline

`userspace/tty.Discipline` edits one line of at most 4096 bytes, the native
path limit, and allocates nothing:

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

`GTTY` version 2 runs over a channel PID 1 creates between the service and the
shell. A client sends:

- `ReadLine(prompt)`: show the prompt and answer with the next line, an
  interrupt, or end of input. One read may be outstanding; a second is refused.
- `Redraw(prompt)`: replace the outstanding read's prompt and show it again with
  what has been typed. The shell sends it when a foreground command ends while
  its read — which had no prompt — is still outstanding. Output does not need
  it any more: the service redraws around every write it relays.
- `Continue`: ask for the next part of a line (ADR 0109).

Prompts are at most 64 bytes of printable ASCII, so a client cannot use the
service to send control sequences. A reply carries at most 255 bytes of a
line; a longer line is answered with `ResultPart` replies, each followed by the
client's `Continue`, and ends with a `ResultLine` reply carrying the rest. A
Ctrl-C between parts discards the line and answers the next `Continue` with the
interrupt. The kernel's console input ring holds 8 KiB, so a pasted line of the
full length arrives whole. A line
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

The shell and every command print to one stream whose reading end only the
service holds (ADR 0089). PID 1 creates it, keeps both ends, and grants copies:
`output` to the service, `stdout` to the shell, and `stdout` and `stderr` to
every command stage that is not piped or redirected. Stream ends are reference
counted, so a stage exiting closes only its own copies ([streams](../kernel/streams.md)).

The service relays what arrives. It keeps a model of the screen
(`userspace/tty.Screen`): what the prompt and the typed text occupy, and whether
the cursor is at the start of a row. Before relaying a batch of output it takes
the line being typed off the screen — `\r\x1b[K` when the line began a row,
otherwise back over exactly its columns — and after the batch it shows the
prompt and the typed text again, on a new row if the output ended mid-row. A
foreground command's output, arriving while the read has no prompt and nothing
is typed, is written exactly as it came. A line finished before the shell asked
for it was echoed without a prompt; when the read arrives the service shows it
again after the prompt, then delivers it. One pass relays at most sixteen reads
(4 KiB), so output cannot keep the service from the keyboard, and Ctrl-C gets
through a command that prints without pause.

The service only reads the stream and writes the console; it never waits on a
writer. A writer facing a full stream waits for room with a deadline (the shell
two seconds per flush, then it drops that output; a command five seconds, then
it exits with status 2).

The kernel's warning echo, panics and early console, PID 1's boot lines, and the
system services' own reports still reach the console directly
([ADR 0089](../adr/0089-terminal-output-and-session-restart.md)).

## Supervision

The service and the shell restart independently, both `restart: always`
(ADR 0089). A session is a channel, which dies with either end, so whichever
restarts is launched with a fresh one and the survivor is handed the other end
as a `GTTY` session notice — a notification carrying exactly one handle:

- When the service restarts, PID 1 gives the new process a new session and a
  new control channel, and sends the session's client end to the shell over its
  exec channel. The output stream needs nothing: PID 1 held a copy of its
  reading end, so output written meanwhile is waiting.
- When the shell restarts, PID 1 sends the new session's server end to the
  service over its control channel. The service had kept editing input with no
  one to give it to; it installs the session, forgets the old client's read, and
  keeps what had been typed of the line.

The control channel carries nothing but session notices, and only PID 1 holds
its other end; anything else arriving on it is dropped and its handles closed.
The validation build kills each once (`fault tty`, `fault shell`) to prove it.

## The bootstrap TTY demonstration

The validation build still runs the ADR 0016 demonstration in the kernel self
tests, with `kernel/tty`: two bootstrap processes exchange a TTY write request.
It exercises IPC waits and device rights and is not the terminal service.
