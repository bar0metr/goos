# ADR 0089: Terminal output and independent session restart

## Status

Accepted. Supersedes the "Output stays direct" decision of ADR 0082 and its
consequence that the terminal service and the shell are one session restarted
together. Extends ADR 0040 (streams), ADR 0072 (quotas) and ADR 0078
(restarting a service on a fresh channel).

## Context

ADR 0082 gave the console's input to a terminal service and left two
limitations on record.

**Output bypassed the terminal.** The shell and every command wrote to the
console through a write-only console capability, so the service could not know
when output had moved the cursor. The shell told it after the fact with a
`Redraw` request, which covered the shell's own reports and nothing else: a
background command printing while someone typed wrote into the middle of their
line. Routing output through the service was rejected then because kernel
streams had exactly one reading and one writing end with no count of the
handles naming them. Closing any handle closed the end, so an end shared by the
stages of a pipeline — every stage needs somewhere to print diagnostics — would
have closed for all of them when the first stage exited. The registry also held
only sixteen streams.

**The terminal and the shell were one session.** They talk over a channel
created once at boot, and a channel dies with either holder. Neither could be
restarted without the other, so both were registered `restart: never`, and a
crash of either left a machine no one could type at.

Building this found a defect in ADR 0082 itself. The exec `Started` notice,
which hands a foreground command's input stream to the shell, carried the job's
correlation in the message header. The IPC layer refuses a correlation on a
notification, so the notice was never sent: PID 1 closed the stream instead, and
every foreground command read end of input at once. What was typed "into" a
command ran as a shell command. The boot test missed it because it only looked
for the typed text in the log, where the echo alone satisfied it.

## Decision

### Stream endpoints are reference counted

Each stream end counts the handles, across every process's handle table, that
name it. The invariant is that the count equals the number of open handles to
the end:

- `StreamCreate` installs one handle per end, and a stream starts at one and one.
- Every other path that places a stream handle in a table counts it: an IPC
  attachment (in the receiver) and a launch grant (in the new process).
- Every path that removes one forgets it: `HandleClose`, the sender's side of an
  IPC move, a consumed launch grant, the unwinding of a launch that failed, and
  process teardown.
- The end closes — with the existing peer-closed wakeups, and the slot released
  once both ends are closed — only when its count reaches zero.

An IPC transfer is a move: the receiver's copy is counted before the sender's
handle is closed, so the count is unchanged net and the end never closes in
between. A launch grant that is not consumed is a copy and adds one. A handle
attached twice to one message is two copies and one close, which the same rule
gets right.

The counting lives in `kernel/stream` (`Retain`, `Drop`, `Placed`,
`CloseHandle`) where host tests exercise it with real handle tables and IPC
channels; `kernel/main.go` calls it from each path above.

Bounds. Counts are 16-bit. No end can have more handles than exist:
32 processes × 128 handles = 4,096, which a test holds below the 65,535 bound.
The bound is still checked: a count at it refuses another handle, and IPC sends
and launches check the headroom they need *before* anything is installed, so a
refusal is a clean error with nothing to unwind. A drop with nothing counted is
refused rather than wrapping, which would have kept an end open for ever.

Two stream semantics change with sharing:

- **A write of at most 256 bytes lands whole or not at all.** With several
  writers on one end, a partial write would let another writer's bytes land in
  the middle of a line. 256 bytes is the largest native stream call, so every
  call is atomic.
- **A writer is woken only when an atomic write fits.** A stream is writable
  when 256 bytes are free, and a read wakes writers only then. Otherwise a writer
  woken for a few free bytes would have its write refused and wait again, which
  is a spin paced by the reader.

### Larger tables, sized from what is needed

- **Streams: 64.** Each stream embeds its 4 KiB ring, so the registry is 256 KiB
  of static kernel memory; it is a fixed array because the kernel heap never
  frees (ADR 0065). PID 1 needs at most 17 at once: four per job for its four
  job slots (an input stream and up to three pipes, or two pipes and a stderr
  stream) plus the terminal's output stream. Its quota goes from 16 to 24; the
  remaining 40 are ten ordinary processes at their full share of four.
- **Channels: 24.** A channel embeds two 16-message queues, about 10.5 KiB, so
  the registry grows from about 170 KiB to about 250 KiB. PID 1, the only process
  that can create channels, held 14 of 16 in a running system, leaving two for
  the VFS sessions of commands. The terminal's control channel adds one, and
  restarting the shell or the terminal briefly needs up to four more while the
  old endpoints are closed. PID 1's channel quota is the whole table, 24, since
  no other process can create one.

### Output goes through the terminal service

PID 1 creates one stream for the terminal's output when it starts the service,
and keeps both ends for as long as it runs. It grants copies:

- the terminal service gets the reading end, `output`, read-only;
- the shell gets a writing end as `stdout`, write-only;
- every command stage gets writing ends as `stdout` and `stderr`, wherever they
  are not a pipe or a redirection. Intermediate stages keep their pipes; only the
  final stage's output and every stage's diagnostics go to the terminal.
  Background jobs are the same.

Because PID 1 holds a copy of each end, neither end ever closes: output written
while the terminal service restarts waits in the stream for its replacement, and
no writer ever sees the terminal "go away". Redirection to files is unchanged.

The service relays what arrives to the console. It knows where the cursor is and
whether a line is being edited, and keeps a small model of the screen
(`userspace/tty.Screen`): before relaying output it takes the line being typed
off the screen, and after the batch it puts the prompt and the typed text back —
on a new row if the output ended without one, so a prompt never trails a
program's unfinished line. Output of a foreground command, whose read has no
prompt and usually nothing typed, is written exactly as it arrives. One pass
relays at most sixteen reads, 4 KiB, so a program printing without pause cannot
keep the service from the keyboard, and Ctrl-C still gets through.

A line typed and finished while no read was outstanding — typing that beat the
shell's request for its next line — was echoed with no prompt. When the read
arrives, the service shows it again after the prompt before delivering it, as
a shell's line editor does with typed-ahead input, so the screen always reads as
a prompt followed by the command it ran, however a burst of typing and a job's
report interleave.

The model is only as good as the service's knowledge of the cursor, so writes
it does not make are kept rare. The validation build printed a marker on every
process exit straight from the kernel, which could move the cursor under a
prompt the service believed was still in place; it now prints it once.

`Redraw` stays, with a narrower meaning: it changes the prompt of a read the
client left outstanding, which the shell does when a foreground command ends.
Redrawing after output is the service's own business now; the shell no longer
redraws after reporting a job.

The shell assembles its output into lines of up to 256 bytes and sends each as
one write, flushing before it asks for a line or waits, so its reports never
arrive in pieces with a command's output between them.

### Who still writes the console directly

| Writer | Why |
| --- | --- |
| The kernel | early console, panics, the warning echo of the log (ADR 0059), and the `goos_diag` dump. They must work when nothing else does. |
| PID 1 (`DebugWrite`) | the boot banner's `[ OK ]` lines, before the terminal exists; `Rebooting...`, which must be written before the reset, not queued behind it; validation markers. After boot, a restarted service is recorded in the log instead of printed, so PID 1 does not write into a line being typed. |
| The terminal service (console read and write) | it is the console's owner. |
| VFS, block, virtio-blk, config, log, and the validation `fault-probe` (console write only) | `LogWrite` is authorised by console write, and their fatal paths report through it. They start before the terminal and underneath it: a storage failure has to be reportable when the terminal cannot be. |

The shell and every command hold no console capability at all. The shell's
`diag` builtin, which needed the console to ask the kernel for its dump, now
asks PID 1 (exec control protocol version 3, `ControlDiagnostics`).

### Backpressure

Nothing in the output path can wedge the terminal or the shell:

- The terminal service only reads the stream and writes the console, which is a
  bounded synchronous call; it never waits on a writer.
- A writer facing a full stream waits for room with a deadline. The shell waits
  at most two seconds per flush and then drops that output rather than stop
  responding; a command waits up to five seconds and then exits with status 2,
  as it did for any stream. They block in `StreamWait`; nothing spins.
- PID 1 never writes to the stream, so a stalled terminal cannot stall the
  supervisor.

### The terminal service and the shell restart independently

Both become `restart: always`. Neither has a reason to exit: a terminal or a
shell that exits, even with status zero, leaves a console no one can use, so it
is brought back. The supervisor's restart limit (ADR 0019) still ends a crash
loop.

Whichever restarts is launched with fresh endpoints and the survivor is handed
the other end of the new session. The session channel is the one thing both
share; it dies with either.

- **The terminal service restarts.** PID 1 creates a new session channel and a
  new control channel, launches the service with their server ends, and sends
  the session's client end to the shell as a typed `GTTY` session notice over
  the shell's exec channel. The shell, which saw its session close and kept
  running without one, installs the new end and asks for its next line through
  it. The output stream needs nothing: its reading end is a copy PID 1 kept.
- **The shell restarts.** PID 1 creates a new session channel, a new exec
  channel, a new VFS session and a new configuration session, launches the shell
  with them, keeps the exec channel's server end, and sends the session's server
  end to the terminal service as a session notice over the service's control
  channel. The service, which kept editing input with no one to give it to,
  installs the new end and serves the new shell. What had been typed of the line
  is kept.
- A peer that is itself down gets nothing: PID 1 closes the end it would have
  sent, and the peer's own restart brings a session of its own.

The new endpoints go into the launch about to happen and into the stored
definition, as ADR 0078 did for the storage driver, and a replaced handle that
no launch consumed — one renewed for a launch that then failed — is closed, so
repeated failures cannot fill PID 1's table.

The terminal's control channel carries only the session notice, and only PID 1
holds its client end. Session notices are notifications with exactly one handle
and no correlation; anything else arriving on the control channel is dropped and
its handles closed.

### Jobs end with the shell's session

When the shell exits, PID 1 stops every stage of every job it had running, the
same way `kill` does. The jobs are reaped through the normal path but answered to
no one; their exec slots come free as they finish. Nothing leaks: a foreground
job's input writer was the shell's and closed with it, pipes close with the
stages that hold them, and each stage's copies of the terminal's writing end go
with its teardown. The new shell starts with no jobs.

### Validation

The validation build keeps the process handles of the terminal service and the
shell, and accepts `ControlFault` from the shell's validation-only `fault tty`
and `fault shell`. The boot test uses them to kill each once and require that the
other keeps working through the replacement.

### The Started notice

It carries the job's correlation in its payload rather than its header (exec
protocol version 9), so the IPC layer accepts it. A host test encodes it through
the IPC wire, and the boot test now requires the line typed into `readline` to
be printed by `readline` and never run as a command.

## Alternatives

**A stream per job handed to the terminal service.** PID 1 could give each job
its own output stream and pass the reading end to the service, which would read
many streams. It needs no kernel change, but the service would multiplex a
changing set of readers, and a job's stages would still have to share one
writing end for their diagnostics — which is the problem reference counts solve.

**A handle-duplication call.** Letting a process duplicate its own stream handle
would give several handles too, but closing any of them would still close the
end. The count is what is needed, and with it duplication is just another path
that places a handle.

**Counting in the handle table.** A generic count per kernel object, kept by
`handle.Table` on every insert and close, would cover channels too. Channels do
not need it — a channel side has one holder by construction — and every object
type would pay for a stream's need. The count lives with the stream.

**Restarting both as a unit.** Tearing both down when either dies avoids handing
over a session. It also loses the shell's state and its jobs when only the
terminal failed, and the terminal is the component most exposed to input.

**Letting the survivor ask PID 1 for a session.** The shell or the terminal
could request a new session when it notices the old one gone. That makes both
clients of process supervision and races the restart; PID 1 already knows when a
restart happens and holds the authority to create channels.

**Keeping a shell's jobs across its restart.** The replacement does not know
them: their correlations, command lines and input streams were the old shell's.
Handing them over would need a job-transfer protocol for a case — the shell
crashing under running jobs — where ending them is the expected outcome.

## Consequences

The shell and every command hold no console capability; the boot test checks
the shell's capability list and that the kernel refuses a read on the shell's
output endpoint. The terminal service is the only process that reads the
terminal's output, as it is the only one that reads what is typed.

A background job's report no longer lands inside a line being typed: the service
erases the prompt line, writes the report, and shows the line again. In the
serial log this appears as the report on the same row as the erased prompt,
after `\r\x1b[K`, which the boot test's job checks allow for.

Command latency, `make test-latency` at 100 Hz under QEMU TCG, ten runs per
case, median microseconds measured by the shell's `time` builtin before and
after this change:

| Case | Before | After | Change |
| --- | ---: | ---: | ---: |
| `help cd` | 212 | 50 | −76% |
| `uname` | 1697 | 1855 | +9% |
| `echo x` | 1548 | 1738 | +12% |
| `ls /system` | 5355 | 6274 | +17% |
| `cat /system/motd` | 2884 | 3299 | +14% |
| `cat /persist/latency.txt` | 2923 | 3176 | +9% |
| `echo y > /persist/latency.txt` | 10244 | 10729 | +5% |
| `echo a \| readline \| readline` | 3006 | 3285 | +9% |
| `missing-stage` | 1241 | 1365 | +10% |

A line printed now costs a stream write, a switch to the terminal service, its
console write, and a switch back, instead of one console write: roughly 150 µs
per line under emulation, which is why `ls /system`, twelve lines, pays most.
The shell's own output is the exception: it used to write the console inside
the measured time and now hands whole lines to the stream and returns, so a
builtin that only prints, such as `help cd`, got faster. The in-guest VFS
benchmarks are unchanged (186 → 194 µs per round trip, within run-to-run
variation). Batching a command's lines into fewer writes would recover most of
the difference if it matters; it is not done here because a command's output
would then appear later than it is written.

A pipeline's first stage exiting early no longer affects the others' output,
and the boot test runs one.

Output written while the terminal service is down waits in its stream, up to
4 KiB; beyond that writers wait and then give up as described. What had been
typed but not finished when the terminal service died is lost with it.

The terminal's model of the screen assumes the line being edited fits one row.
A prompt and typed text longer than the console is wide are erased only from
their last row. The kernel's warning echo still writes directly and can land in
a line being typed; it is the one ordinary event that does.

Stream memory grows by 192 KiB and channel memory by about 80 KiB of static
kernel data.
