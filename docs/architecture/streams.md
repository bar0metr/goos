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
descriptor. A full write reports bounded backpressure; an empty read reports
that it would block while the writer remains open. `StreamWait` registers the
current task in a bounded wait set and removes it from the runnable set until
data, buffer space, peer closure, or timeout.

Closing the writer preserves buffered bytes. After the reader drains them, the
next read reports peer closure as EOF. Closing the reader discards buffered
bytes and makes later writes fail with peer closure. Process teardown closes
all owned stream endpoints and releases the object after both sides close.

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
