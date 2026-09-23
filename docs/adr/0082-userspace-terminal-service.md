# ADR 0082: Userspace terminal service

## Status

Accepted. Supersedes the console-session input of ADR 0025 and extends ADR 0016,
ADR 0059 and ADR 0075.

## Context

The specification asks for a userspace TTY service with line editing and a
stdin/stdout abstraction (§23), and lists it in the 0.1.0 release (§56). What
existed was a demonstration from ADR 0016 that ran only in the validation build.
The shell read the console itself, one byte at a time, and edited its own line;
a foreground command read the console directly too, through the device
capability it was given as standard input.

The console's read right also authorised more than input. `LogRead` and
`LogBounds` accepted the same console object with the same right, so the log
service, which drains the kernel log, could equally read every key typed.

## Decision

**A terminal service owns the console's input.** `/system/tty` is supervised by
PID 1 and is the only process given the console with read rights. It runs the
line discipline — editing, echo, Ctrl-C, Ctrl-D, Ctrl-U, Ctrl-L, and skipping
escape sequences so arrow keys type no text — and answers typed `GTTY`
requests over a session channel: `ReadLine(prompt)` returns a line, an
interrupt or end of input, and `Redraw(prompt)` shows the prompt and the line
again after output has moved the cursor.

**Commands read a stream, not the console.** PID 1 gives every job's first stage
a standard-input stream of its own. For a foreground job it sends the writing
end to the shell in an exec `Started` message ahead of the job's reply; the
shell writes the lines the terminal gives it, closes the stream on Ctrl-D, and
stops the job on Ctrl-C. A background job's stream is at end of input from the
start.

**The shell reads lines.** It keeps one read outstanding at the terminal, also
while a command runs, so a line typed ahead is kept and shown again with the
next prompt. It no longer holds the console's read right; its console
capability is write-only.

**Reading the log is its own capability.** A new device object authorises
`LogRead` and `LogBounds` and nothing else. PID 1 receives it at boot and gives
it, read-only, to the log service and the shell. The console object keeps
authorising writes, records and console input.

**Output stays direct.** Processes write through a write-only console
capability. The terminal service redraws the line when told output has moved
the cursor.

## Alternatives

**Routing all output through the terminal.** It would give the service the
whole screen and let it redraw under any output. Every process's standard
output and error would become a stream read by the service. Kernel streams have
one reading and one writing end with no reference count, so an end shared by
the stages of a pipeline closes for all of them when the first exits, and the
registry holds sixteen streams. Doing it needs reference-counted endpoints and a
larger registry, which is kernel work beyond this change.

**Letting a foreground command read the terminal service directly.** Giving
each command a session would remove the shell from the input path. It also
needs the service to know which command is in the foreground, which is the
shell's knowledge, and it would have to be told on every command.

**Moving the UART driver out of the kernel with it.** The kernel keeps the
serial port because it is also the early console, the panic path and the
warning echo. The terminal service takes the parts that are policy — who may
read, how a line is edited — and leaves the device to the kernel.

## Consequences

Only PID 1 and the terminal service can read what is typed. The shell, the
commands and the log service cannot; the boot test checks that the shell's
console capability is write-only and that it holds the terminal session and the
log capability instead.

The shell's interactive behaviour is unchanged for someone typing at it. New are
Ctrl-U, Ctrl-L, Ctrl-D ending a command's input, arrow keys that no longer type
`[A`, and a line typed during a command that is kept for the prompt after it.
The boot test types an arrow key, a line erased with Ctrl-U, and Ctrl-D into
`readline`, and checks each outcome.

A foreground command now costs one more stream and one more message. The stream
is charged to PID 1 and released when the command ends.

Waiting for a foreground command no longer polls for Ctrl-C every tenth of a
second: the shell blocks on the terminal and PID 1 together. A reply to a
foreground job that arrives while the shell waits for another answer from
PID 1, such as the acknowledgement of a kill, is kept for the wait it belongs
to instead of being dropped.

The terminal service and the shell are one session over a channel created once,
so neither is restarted on its own.
