# ADR 0162: Linux pipes are native streams with a 4096-byte PIPE_BUF

## Status

Accepted.

## Context

A shell pipeline connects processes through `pipe`, `fork`, `dup2`, and
`execve`. GoOS already carries Linux standard streams on native byte streams,
and a Linux open-file description already owns a stream endpoint reference
that the last descriptor drops (ADR 0156).

Linux promises that a pipe write of at most `PIPE_BUF` bytes lands whole,
never interleaved with another writer's bytes. musl compiles `PIPE_BUF` as
4096 into every program. A native stream held a 4080-byte ring in one page,
so a 4096-byte write could never fit whole.

## Decision

`pipe` and `pipe2` create a native stream charged to the caller's stream
quota. The read end becomes an `FDStreamReader` description and the write end
an `FDStreamWriter` description on the two lowest free descriptors. Either
both descriptors exist afterwards or the stream is released. A full stream
quota is `EMFILE`, a full stream registry `ENFILE`, and an unwritable result
array `EFAULT` before anything is created.

`pipe2` accepts `O_CLOEXEC`. `O_NONBLOCK` and `O_DIRECT` need nonblocking and
packet streams, which GoOS does not have, and are `EINVAL` like every other
flag.

The existing stream rules supply the rest. The read end refuses writes and
the write end refuses reads with `EBADF`. `lseek` is `ESPIPE` and `ioctl` is
`ENOTTY`. `fstat` reports a FIFO whose inode is the stream identity, the same
for both ends. A read after the last writer closes returns 0. A write after
the last reader closes is `EPIPE`; `SIGPIPE` waits for signal delivery.

A stream ring grows to 8176 bytes, two pages with its counters. `PIPE_BUF` is
4096. A `write`, or a `writev` whose elements are all readable, of at most
`PIPE_BUF` bytes waits until the ring has room for all of it and then copies
it in one kernel pass that nothing can preempt. Larger writes keep the
existing behaviour: they move what fits in 256-byte pieces and return a short
count when the ring fills after some bytes moved. The rule covers every Linux
stream writer, standard streams included.

## Consequences

Two writers of records up to 4096 bytes never interleave inside a record. A
stream takes two pages instead of one, so 256 streams can hold 2 MiB of
kernel memory. Native stream calls keep their 256-byte atomic unit. A Linux
program sees a pipe capacity of 8176 bytes rather than 65536, which only
changes how soon a writer blocks.
