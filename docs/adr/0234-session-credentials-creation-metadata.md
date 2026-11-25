# ADR 0234: Session credentials and creation metadata

## Status

Accepted.

## Context

GoOS 0.4.0 introduces stable account identities (ADR 0233). This card delivers
the next step: a trusted immutable principal on VFS sessions, per-process Linux
credentials, creation metadata, and account-aware remote admission. The
security model does not change. Identity is data, not authority. A UID, GID,
group, account name, or key never widens a native capability, a VFS scope, or
an administrative grant.

## Decision

### 1. Trusted session identity

`kernel/vfs.SessionIdentity` holds the trusted principal a session carries:
the stable `AccountID`, the Unix `UID`, the `PrimaryGID`, and the 128-bit
database `Epoch`. It is separate from `Authority` and from native handles.

PID 1 installs it over the trusted control path. `GVFSCTL1` version 12 adds an
identity block to the `AddSession` control request
(`AddIdentitySessionRequest`); the ordinary data protocol has no field that can
set it. A zero identity is the identity-free system session: creation keeps the
legacy fixed ownership of UID 0 and GID 0 and the caller's documented default
mode. The account's supplementary groups, home, and name travel elsewhere, in
the root confinement, the environment, and the Linux credential snapshot.

The namespace stores identities per owner, parallel to authorities, and the
kernel VFS derives a creation context from the owner's identity when a session
creates an object.

### 2. Creation metadata

Creation carries a resolved context: the mode's low 12 bits and the owning UID
and primary GID. The Linux process computes the requested permission bits after
its umask; native callers use a documented default (0644 files, 0755
directories) or an explicit mode. Only trusted backend IPC carries the resolved
context; ordinary client wire never accepts UID or GID.

The context flows through four layers:

- `GVF1` version 17 adds a `Mode` field to open, create, and mkdir requests,
  with a flag that marks it authoritative.
- The kernel `Backend` gains an optional `CreationBackend` interface whose
  create, mkdir, and symlink methods take a `ramfs.CreateContext`. Backends that
  persist Unix metadata (ext4) implement it; FAT32 and RAMFS keep the legacy
  methods and present synthesized modes, never claiming persisted ownership.
- `GFSPRT01` version 15 carries the resolved context in create, mkdir, and
  symlink requests.
- ext4 computes final GID and directory SGID inheritance inside the same
  mutation: a child of a set-group-ID directory inherits the parent's GID, and
  a new directory also inherits the set-group-ID bit. No extra stat-then-create
  race exists.

Hard links and rename preserve existing ownership. `O_CREAT` on an existing
file never reassigns metadata; open, create, exclusive, and truncate stay one
atomic intent.

### 3. Linux credentials

`userspace/startup.Credentials` is the bounded credential snapshot: the
real/effective/saved UID/GID (equal at launch), the supplementary GIDs, the
stable account ID, and the database epoch. GST4 version 5 carries this block
after the string area. The kernel reads it at launch and stores it
in `ProcessState`, inherited across fork and exec and shared across threads.

`getuid`, `geteuid`, `getgid`, `getegid` report the snapshot.
`getgroups(0, NULL)` returns the required count, a short buffer fails EINVAL
without writing, and the supplementary GIDs copy out with validated pointers.
`setuid` and `setgid` are self no-ops and fail EPERM otherwise. The auxv
`AT_UID`, `AT_EUID`, `AT_GID`, `AT_EGID` carry the same numbers. An
identity-free legacy launch reports the explicit legacy identity, 1000:1000,
matching the pre-account behavior.

`umask` now applies at creation: the Linux bridge masks the requested mode
before it reaches the VFS. No double application happens because native
callers pass a resolved mode directly.

### 4. Account-aware remote admission

PID 1 holds a persistent identity lookup session. A worker authorizes a key
during the SSH handshake by asking PID 1 over the worker control channel; the
worker receives no lookup session, no identity-admin, and no wider grant.
Proof of possession stays in the pinned SSH library, which verifies the
signature after the callback.

At admission PID 1 revalidates the key and fetches the account from the current
committed snapshot, then binds the principal to the supervisor-controlled
session record. A disabled, missing, recreated, or revoked account fails
closed. A worker-supplied username or account ID never chooses grants.

Shell, exec, and SFTP share one account principal. SFTP roots at the validated
account home. Shell and exec receive a root-confined VFS authority carrying the
account's trusted `SessionIdentity`: the home is read-write, the
additional-volume root is read-write as the shared writable storage grant, and
the program tree is read-only so the shell can read the command index and
resolve programs. One account therefore cannot name another account's files
through the shell, its launched commands, or SFTP. Program execution is
delegated to PID 1 through the exec channel. Legacy `system.remote.user`,
`authorized-keys`, and `sftp-root` import into one account idempotently; after a
committed migration the account database is the sole account and key authority,
with no fallback to legacy keys on a missing or corrupt database.

### 5. Invariants

Identity is never authority. One mutation owner per writable filesystem holds.
Protocol changes bump versions, update history, and reject the previous
version in codec tests. Every new count, length, and allocation is bounded.

## Consequences

Account-backed creation writes real ownership and mode into ext4 and reports
them through `stat`; FAT32 and RAMFS keep presenting synthesized metadata.
Linux processes see credentials derived from their launch identity. Remote
sessions admit only accounts the identity database currently authorizes, and
their filesystem access stays scoped to the account home plus the bounded
positive grants (shared writable storage read-write, program tree read-only).
