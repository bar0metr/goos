# Native byte streams

GoOS native byte streams are bounded unidirectional kernel objects intended for
shell pipes and standard-stream redirection. They are not POSIX file
descriptors.

Creating a stream returns two opaque process-local handles:

- a reader endpoint with `Read|Transfer` rights;
- a writer endpoint with `Write|Transfer` rights.

The endpoints encode object identity and direction independently of handle slot
numbers. The kernel validates handle type, direction, and rights for every
operation. Each stream owns a fixed 4096-byte ring buffer. One operation moves
at most 256 bytes and reports the exact transferred size through a validated
descriptor. A write of at most 256 bytes — every write a native call can make —
lands whole or not at all, so writers sharing an end never interleave inside
one call. A full write reports bounded backpressure; an empty read reports
that it would block while the writer remains open. `StreamWait` registers the
current task in a bounded wait set and removes it from the runnable set until
data, peer closure, timeout, or, for a writer, room for a whole 256-byte write:
a writer is not woken for less, because its write would be refused again.

## Shared endpoints

Each end counts the handles, in every process, that name it (ADR 0089). The
end closes only when the last of them does:

| Path | Count |
| --- | --- |
| `StreamCreate` | each end starts at one |
| IPC attachment | +1 for the receiver's copy, −1 for the sender's handle, which the send closes: a move |
| launch grant | +1 for the child's copy; −1 more if the grant is consumed |
| `HandleClose`, process teardown, a failed launch's unwinding | −1 per handle |

So an end can be shared: PID 1 grants the terminal's writing end to the shell
and to every stage of every pipeline, and a stage exiting closes only its own
copies. Counts are 16-bit and bounded by the handles that exist (64 processes ×
256 handles); the bound is checked anyway, before anything is installed, and a
count is never decremented below zero. The counting is in `kernel/stream`
(`Retain`, `Drop`, `Placed`, `CloseHandle`) and host-tested with real handle
tables and IPC channels.

Closing the writer — its last handle — preserves buffered bytes. After the
reader drains them, the next read reports peer closure as EOF. Closing the
reader discards buffered bytes and makes later writes fail with peer closure.
Process teardown drops every owned stream handle and releases the object after
both ends close.

The registry holds up to 256 streams. Each is one page, a 4080-byte ring and
its counters, taken from the page allocator when the stream is created and
returned when it is released (ADR 0111). An ordinary process may hold sixteen
and PID 1 sixty-four (ADR 0072, ADR 0089, ADR 0111).

The userspace integration probe creates a stream, writes and reads data, closes
the writer, observes EOF, and closes the reader. The shell uses the same object
for pipelines of up to four concurrent processes. PID 1 creates one object
per adjacent pair and substitutes directional endpoints for the children's
named standard-stream roles. Retained endpoints are closed before reaping
children after a partial launch failure.

Stdout file redirection uses the same pipeline path. The shell adds a bounded
`write` consumer as the final stage. PID 1 grants it a read-only stream endpoint
and a private VFS session; the utility creates one resolved path, then either
truncates it for `>` or seeks to its current size for `>>`, and writes bytes
until peer closure. Validation markers use the separate stderr capability and
therefore do not contaminate redirected data.

Input file redirection uses the symmetric composition. The shell prepends the
existing `cat` utility as a producer with a private VFS session. PID 1 only sees
an ordinary pipeline and connects the producer's stdout endpoint to the first
requested stage's read-only `in` role.

Stderr redirection reuses the same consumer but stays outside the stdout chain.
The versioned exec request names the stage whose diagnostics are diverted; PID 1
creates one private stream for that stage and the sink, launches the sink first,
and grants the source a write-only endpoint as its `stderr` role. Stdout of the
redirected stage still reaches its normal destination, and stages without a
diverted role keep the terminal.

## The terminal's output stream

Output that is not piped or redirected goes to one stream PID 1 creates for the
terminal service (ADR 0089). PID 1 keeps both of its ends and grants copies: the
reading end to the terminal service, the writing end to the shell and, as
`stdout` and `stderr`, to every stage that prints to the terminal. Because PID 1
keeps a copy of each, neither end ever closes; output written while the terminal
service restarts waits for its replacement.
