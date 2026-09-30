# ADR 0138: Linux VFS session bridge

## Status

Accepted.

## Context

ADR 0135 gives each Linux process one VFS session and forbids any route to a
filesystem that skips it. The kernel translates Linux file syscalls (ADR 0134),
so the kernel has to speak the VFS protocol on the process's behalf. ADR 0135
left four questions open: whose authority a request carries, how a reply finds
its request, how long a call waits, and what a process sees after the VFS dies
or restarts.

## Decision

The bridge sends each request on the channel PID 1 granted at launch. It
resolves that channel through the process's handle table on every exchange and
requires Read, Write, and Wait rights. The kernel holds no session of its own,
so a process without the handle reaches no file.

One exchange is one request and one reply; a call may run several in turn. A session numbers its requests from
one and reuses no number. The calling task's continuation records the number
before the request enters the queue, so a retried syscall entry cannot send it
twice. The task then blocks in the channel wait set. A reply, the peer's death,
or the deadline wakes it through the path native receivers use.

After a wake the bridge reads the queue and sorts each message:

- The reply with the recorded number and the requested operation completes the
  call.
- A reply with a lower number answers a call that gave up earlier. The bridge
  drops it and keeps waiting.
- Anything else fails the call with `EIO`: a higher number, another operation,
  a message that is no VFS reply, or a message with attached handles. The
  bridge closes handles such a message carried, so the process gains no native
  authority from it.

A wait lasts at most eight seconds, the bound native clients give the VFS.
Expiry returns `ETIMEDOUT`. The VFS may still finish the abandoned request, and
its late reply is the lower-number case above. An abandoned open stays open in
the session until the process exits, inside the per-session limit the VFS
enforces.

Peer death returns `EIO` and marks the session failed. A failed session refuses
every later request with `EIO`. The bridge opens no replacement session for a
running process. A restarted VFS receives sessions from PID 1, and PID 1 grants
them to new launches only.

A request carries a path of at most 216 bytes in its message and a longer one
in an attached memory object. ADR 0140 defines how a Linux path becomes the
canonical absolute path a request carries.

`openat` and `close` were the first calls on the bridge. ADR 0141 defines the
file calls built on it and how a call runs several exchanges.

## Consequences

File authority stays with the capability PID 1 granted; kernel residence adds
no filesystem reach. A process outlives a VFS restart without its files and
cannot regain them by itself. Every wait has a deadline and every table on the
path has a fixed size. File I/O, directories, and path resolution reuse the
same exchange.
