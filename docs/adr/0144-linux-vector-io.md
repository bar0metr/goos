# ADR 0144: Linux vector I/O

## Status

Accepted.

## Context

musl stdio writes with `writev` and reads requests larger than its buffer with
`readv`, so both are on the path of ordinary programs. A vector is caller
memory that describes more caller memory: up to 1024 elements on Linux, each
with its own address and length. The kernel must bound what it copies and must
never assemble a vector's data in storage that grows with the request.

## Decision

A vector has at most 16 elements; more is `EINVAL`. The kernel copies the
element array into fixed storage and checks all of it before any byte moves:

| Condition | Result |
| --- | --- |
| element count over 16, or negative | `EINVAL` |
| a length negative as `ssize_t` | `EINVAL` |
| a non-empty element outside user memory | `EFAULT` |
| unreadable element array | `EFAULT` |

Empty elements are legal anywhere and transfer nothing.

`writev` to a stream writes the elements in order in atomic chunks. When the
stream fills after some bytes went out, the call returns the count so far; it
blocks only when it has written nothing. `writev` to a file gathers the
elements into the process's 64 KiB I/O memory object and is one VFS write.

`readv` from a stream delivers what the stream holds, up to the vector's
total, and blocks only while it has nothing to return. `readv` from a file is
one VFS read into the I/O object. Both fill the elements in order. Every
element is checked for writability before a byte leaves its source, so a bad
pointer costs the caller no data.

One call transfers at most 64 KiB. A longer vector is a short transfer. Data
moves through the 256-byte stream chunk buffer or the I/O object; neither
grows.

`read` and `write` follow the same rules with one element.

## Consequences

Programs that loop on short transfers, which POSIX requires of them, work
unchanged. A program that passes more than 16 elements gets `EINVAL` where
Linux would accept up to 1024; musl passes two. `preadv` and `pwritev` are not
implemented.
