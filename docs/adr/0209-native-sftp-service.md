# ADR 0209: Native SFTP service

## Status

Accepted and implemented. Amends ADR 0199 and ADR 0200, which excluded SFTP.

## Context

Administrators need to move files to and from GoOS with a standard OpenSSH client.
`remoted-worker` holds the SSH connection, its session keys and the host-signing
endpoint, and it has no filesystem authority. Giving it a VFS session would let a
bug in the SSH code reach files.

## Decision

### A separate process per session

An SSH `sftp` subsystem request makes the worker ask PID 1 to open the session in
`ModeSFTP`. PID 1 launches the fixed image `/system/services/sftpd` and grants it:

- a stdin stream and a stdout stream, whose other ends the worker receives;
- a VFS session rooted at the configured directory;
- the log device, write only.

`sftpd` has no terminal, launcher, configuration, network or signing authority. The
worker relays bytes and receives no VFS session. The request names no image and no
root. PID 1 refuses `ModeSFTP` for a connection whose policy had SFTP disabled.

We rejected SCP and shell emulation. Both run a command interpreter on the peer's
behalf, and both would need the shell's launch authority for a file copy.

A connection enters one mode: terminal, exec or SFTP. The SSH pump refuses a later
mode request, a PTY with SFTP, and any other subsystem name, with a channel failure
that leaves the connection up.

### The session root

`vfs.Authority` gains `Root`, a canonical path of at most 64 bytes. `AuthorizePath`
canonicalizes the path a rooted session names, the form the namespace then resolves,
and refuses anything other than the root and paths below it. A rename, replacement
or removal must name a path strictly below the root, so a rooted session cannot move
its own root. A rooted session also cannot read the mount table.

The check belongs to the VFS, so a parser bug in `sftpd` reaches nothing outside the
root. `sftpd` translates client paths for the sake of the client's view; security
does not depend on that translation.

A path spelled differently from the root, such as `/HOME/USER` on FAT32, fails the
comparison and gets permission denied. Inside the root, exact and subtree scopes
apply by node identity as they do for any session: the configured `authorized_keys`
stays read only under any alias, and moving its parent directory stays refused.

We rejected a subtree scope slot for the root. A slot records the nodes of its
subtree in a walk bounded at 256, and a check on a node outside that record answers
permission denied for every session on the backend. Three hundred uploads would then
have broken the persistent volume for the whole system. Slots also last for the
life of the namespace, and only eight exist, while the root changes on reload.

The root identifies a path. If an administrator renames the root's directory, a
session open at that moment follows the path, not the moved directory. A cross-linked
FAT32 directory inside the root remains a limit shared with every session (ADR 0117).

The root confines namespace paths, not filesystem backends or mount IDs. A mount
reachable below the rooted path remains reachable unless another authority rule
denies it. A subtree scope works the other way: it follows node identity on one
backend and needs a walk of the nodes it covers.

### Retained handles

Root confinement applies when a session names a path. An already opened VFS file
handle remains a capability to that opened object. If another authorized session
renames the object outside the rooted path, the VFS does not revoke the handle because
its namespace path changed, and READ, WRITE and FSTAT keep working on it.

New path-based operations remain confined to the configured root. The session cannot
open or stat the object at its new path, list that directory, move anything there, or
read the mount table. Installing a scope over the object still revokes the handle or
denies its use, as for any session. FSETSTAT changes size through the path it opened,
so after such a move it fails. Authority granted through an open handle survives a
rename, and obtaining a new handle still requires path authorization.

### Settings and their lifetime

`system.remote.sftp-enabled` defaults to false, `system.remote.sftp-root` to
`/home/user`. The root must be canonical and strictly below `/home`, and a bad value
fails the whole reload. Both settings travel in the worker's policy page. PID 1 takes
the root from its own copy of the page at admission, so a connection keeps its root
across reloads and the worker cannot change it.

PID 1 checks that the root is an existing directory when it opens the session. The
SSH channel has already accepted the subsystem by then, so a missing root closes the
connection.

### Protocol and limits

`sftpd` speaks version 3 and advertises no extensions. It serves one request at a
time and copies request IDs unchanged.

| Limit | Value |
|---|---|
| Packet length field | 64 KiB |
| File data in a READ or WRITE | 32 KiB |
| Open handles per session | 32 |
| Paths and names | the VFS limits, 4096 and 1024 bytes |

It holds one input buffer and one reply buffer of the packet size, and it rejects an
oversized length field before storing anything for it. A READ asking for more than
32 KiB returns 32 KiB, and a WRITE carrying more gets `SSH_FX_BAD_MESSAGE`.

Malformed framing or an encoded length past the packet ends the session. A valid
packet of an unknown type gets `SSH_FX_OP_UNSUPPORTED`.

Transfers use the bulk VFS operations. Each states its offset; `sftpd` sends a seek
only when the handle's VFS position differs, so a sequential transfer costs one VFS
request per 32 KiB block. READDIR uses the batched listing of ADR 0208 and fills one
NAME reply with whole records. The stream relay still moves 256 bytes per system
call, the kernel's `MaxStreamIO`, and the worker takes 64 of those between SSH events.

### Handles

A wire handle holds a kind, a slot, the slot's generation and a random per-session
nonce, and no native value. Freeing a slot changes its generation. The nonce stops a
handle from another session matching a slot here. A file handle owns one VFS open
file, and a directory handle owns a path and a listing cursor.

### Unsupported operations

SYMLINK, READLINK and all extensions answer `SSH_FX_OP_UNSUPPORTED`, because GoOS has
no symlinks; LSTAT returns what STAT does. SETSTAT changes size only. A request that
also changes permissions, ownership or times changes nothing and answers
unsupported, so `chmod` and `put -p` report failure. OPEN and MKDIR accept and ignore
a permissions field, since `put` and `mkdir` send one. Reported modes, 0644 and 0755,
are fixed and grant nothing.

### Control protocol

VFS control protocol version 10 carries the root in AddSession authority, after the
scope grants. The payload is the grants alone for a session with no root. A decoder
refuses another version, a root over 64 bytes, and a root that is not canonical.

### Failure

PID 1 treats `sftpd` as the session's shell for reaping. If `sftpd` exits, the SSH
channel closes with its status.

When a worker dies, PID 1 closes that connection's slot, kills `sftpd` and waits for
it. The client sees its connection drop, because the process holding the SSH keys has
gone. The VFS closes the session's open files when its channel closes.

When the VFS dies, PID 1 kills every `sftpd`, and each worker sends the exit status and
closes its channel. PID 1 admits no new connection in the meantime. It stops the remote
service once those workers finish, or after 5 seconds if a peer does not answer. Loss
of any other remote dependency stops the service at once. The normal restart path
then brings the VFS and the remote service back. Old sessions, processes and handles
do not return, and a wire handle carries a nonce that a new `sftpd` does not share.

A VFS request that gets no answer within the client's deadline counts as a lost
session. `sftpd` reports `SSH_FX_FAILURE` for that request and exits, without using
its handles again.

`sftpd` logs one record per session with byte, request, VFS and batch counts. It does
not log file contents or paths.

## Consequences

Moving a directory with more than 256 nodes below it is refused, because the walk
that looks for protected objects inside it stops at that bound. A rooted session has
this limit in common with every other session.

Requests run sequentially, so a client's pipelined requests wait in the SSH window
and the bounded streams.
