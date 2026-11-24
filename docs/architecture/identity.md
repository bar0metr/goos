# Identity core

GoOS 0.4.0 adds a bounded account, group and key store so later milestones can
bind Unix metadata, Linux credentials and remote sessions to one principal.
This M4 core delivers the store, the supervised service, the administration
commands and an account-aware SSH authorization seam. It does not yet install a
principal into VFS sessions, mint Linux credentials, mutate filesystem
ownership, or switch production SSH admission to per-account logins. Those
arrive in M5 (ADR 0233).

Identity is data, not authority. A UID, group, key or account name never widens
a native capability, a VFS scope or an administrative grant.

## Packages

Three packages split the core so the domain logic runs under plain `go test` on
the host.

| Package | Holds | Host-testable |
| --- | --- | --- |
| `userspace/identity` | the domain types, every bound, the store and the snapshot codec | yes; imports only the standard library and `goos/internal/limits` |
| `userspace/identityproto` | `GIDNTY1`, the versioned IPC codec | yes |
| `userspace/identityd` | the single-owner service, availability state machine, and candidate coordinator | yes; imports only `identity` and `identityproto` |

`userspace/cmd/identityd` is the native entry point that wires IPC and the VFS
session around `userspace/identityd`. `userspace/bin/user` and
`userspace/bin/group` are the administration commands.

## Data model

An account holds a stable `AccountID uint64`, a name, a `uint32` UID, a primary
`GroupID uint64`, up to 16 supplementary `GroupID`s, a home path, an enabled
flag, and up to 8 Ed25519 public keys. A group holds a stable `GroupID uint64`,
a name, and a `uint32` GID.

| Constant | Value | Meaning |
| --- | ---: | --- |
| `MaxAccounts` | 64 | active accounts |
| `MaxGroups` | 64 | active groups |
| `MaxSupplementaryGroups` | 16 | supplementary groups per account |
| `MaxName` | 31 | account and group name bytes |
| `MaxHome` | 64 | home path bytes |
| `MaxKeysPerAccount` | 8 | public keys per account |
| `MaxTotalKeys` | 128 | public keys across all accounts |
| `KeySize` | 32 | Ed25519 public key bytes |
| `FingerprintSize` | 32 | SHA-256 digest bytes for key removal |
| `MaxSnapshotBytes` | 65536 | hard cap on the encoded snapshot |

Account and group names use the grammar `[A-Za-z0-9_-]{1,31}`, matching
`remotepolicy.ValidUser`. Account names and group names occupy separate
namespaces. The bounds stay within the shared native limits; two compile-time
checks fail the build if `MaxName` or `MaxHome` ever exceeds `limits.NameMax` or
`limits.PathMax`.

## Internal IDs and Unix numbers

`AccountID` and `GroupID` are persisted monotonic counters, distinct from Unix
numbers. The first allocated ID of each kind is 1; 0 is never a valid internal
ID. The store refuses to allocate at `math.MaxUint64`, and deletion never reuses
an internal ID. An administrator may reuse a UID or GID explicitly, but an old
internal ID never aliases a new account or group.

UID and GID are separate `uint32` numbers. Allocation and explicit assignment
reject both reserved values, `0` and `0xFFFFFFFF`. Allocation uses persistent
`nextUID` and `nextGID` high-water marks that start at `AllocBase` (1000) and
only increase, so an automatically issued number is never reused. A mark is a
`uint64` bounded by the uint32 domain; `0xFFFFFFFF` is the exhausted sentinel.
Explicit assignment of a free non-reserved number raises the mark when it
crosses it, and it rejects duplicates. Deletion and reassignment never lower a
mark.

## Store and persistence

`identity.Store` is the single mutation owner. Mutation targets are named; the
owner resolves names to internal IDs at call time, so no stale-name race exists.
Cross-references in mutations use Unix numbers: `SetPrimaryGID` and `SetGroups`
take GIDs, `SetMembers` takes UIDs, and the store resolves them to internal IDs.
`RemoveKey` identifies a key by its SHA-256 fingerprint. Queries return accounts
and groups in ID order, supplementary groups ascending, and keys sorted by
fingerprint. `CreateAccount` applies the user-private-group model: it allocates
a UID and GID, creates the account, and creates a private primary group named
after the account.

One versioned snapshot file, `/state/identity/db`, holds everything. No
per-account or per-group files exist, and public keys are not a second authority
source. The encoding is fixed little-endian and deterministic, and it re-encodes
byte for byte.

```text
magic   "GOOS-IDENTITY-V2\n" (17 bytes)
version uint16 = 2
sequence uint64 (monotonic, increments each committed mutation)
payload epoch [16]byte, nextAccountID, nextGroupID, nextUID, nextGID, accounts, groups
digest  sha256(magic || version || sequence || payload) (32 bytes)
```

`sequence` marks durability and linearization; it is not a session token. The
payload leads with the 128-bit database epoch and the four counters. The
snapshot format version (2) is independent of the IPC version. The pre-release
V1 format (`GOOS-IDENTITY-V1\n`, version 1) is refused with `ErrOldFormat`; M4
never silently upgrades it or invents high-water history.

A mutation is a real commit boundary. The single-owner coordinator clones the
committed store, applies the change, validates and encodes the candidate, then
persists it before publishing:

```text
clone committed store -> apply -> validate -> encode
  -> exclusive temp create -> full write -> Sync(temp handle) -> checked Close
  -> Replace(temp, db) -> Sync(containing directory)
  -> publish candidate
```

`Close` is not fsync, and a required unsupported or failed `Sync`/`SyncDir` is
not success. On FAT32 `Replace` is a sector-level old-or-new switch, not ext4
journal crash atomicity, so a torn operation can leave the target
indeterminate; identityd treats that as Unavailable and never assumes the old
snapshot survived. A definite failure before the rename leaves the old store in
place. The store never truncates a snapshot in place and never rewrites a
committed generation.

The temporary file uses one reserved name, `.<name>.~save` beside the target,
created exclusively, with no counter. A stale temp is reconciled at startup
against a bounded directory listing: with the db present the temp is removed
and its removal verified; with the db absent the state is Unavailable
(interrupted provisioning), never silently promoted.

Load validates magic, version, lengths, counts and digest, then the payload:
epoch nonzero, bounds, high-water marks, duplicates and dangling references.
Any failure is corruption.

## Availability

identityd reports one of three states.

| State | Condition | Behaviour |
| --- | --- | --- |
| `Available` | loaded and valid | data and lookup operations serve |
| `Unprovisioned` | no snapshot and no stale temp file | the admin endpoint serves; lookup and data operations deny |
| `Unavailable` | corrupt snapshot, or a write with uncertain durability whose reconciliation produced no validated snapshot | lookup denies; data operations return unavailable |

Provisioning and reset are explicit and enforce their preconditions inside
identityd:

| Operation | Unprovisioned | Available | Unavailable |
| --- | --- | --- | --- |
| Status | report state | report state and counts | report state |
| Provision | commit empty DB with a fresh epoch | Conflict, unchanged | denied, unchanged |
| Reset | Conflict; use provision | commit empty DB, preserve epoch and high-water marks, advance revision | commit empty DB with a fresh epoch and fresh counters |
| Data mutation | denied | validated durable update | denied |
| Key authorization / lookup | denied | current committed data only | denied |

identityd never initializes a database on its own at boot, and it never
regenerates corrupt data over. Reset destroys identity records, not home
directories or disk ownership metadata. Local console administration and
recovery do not depend on UID 0, on a valid account database, or on remote
login. The policy fails closed: any corruption or uncertain durability denies
lookups and data operations instead of serving partial or guessed state.

## IPC

`userspace/identityproto` defines `GIDNTY1`: `ProtocolID = 0x4749444e545931`,
`Version = 2`. Version 2 adds the database epoch, the committed revision, and
the account's supplementary group Unix GIDs to each record. Messages travel in
the `kernel/ipc.Message` envelope, a 256-byte
inline payload plus up to four attached handles. The codec encodes field by
field in little-endian with explicit length prefixes, rejects nonzero reserved
bytes on decode, matches the version exactly, and rejects unknown opcodes and
oversized counts.

The control channel carries two operations, each sent by PID 1 with one attached
session handle:

- `OpAddAdmin` opens an administration session.
- `OpAddLookup` opens a lookup/authorization session.

Administration operations run on the administration session: `AdminStatus`,
`AdminProvision`, `AdminReset`, `AdminListAccounts`, `AdminListGroups`,
`AdminGetAccount`, `AdminGetGroup`, `AdminAddAccount`, `AdminDeleteAccount`,
`AdminSetEnabled`, `AdminSetUID`, `AdminSetGID`, `AdminSetHome`,
`AdminSetGroups`, `AdminAddKey`, `AdminRemoveKey`, `AdminAddGroup`,
`AdminDeleteGroup`, `AdminSetMembers`.

Lookup operations run on the lookup session: `LookupAuthorizeKey`,
`LookupGetAccount`.

Each get and list record carries the stable internal ID, the Unix UID and GID,
name, home, enabled flag, key count, the 128-bit database epoch, the committed
revision, and, for accounts, the supplementary group Unix GIDs sorted
ascending. The epoch and revision name one committed view for key
authorization and M5 admission.

Result codes distinguish `ResultOK`, `ResultInvalid`, `ResultNotFound`,
`ResultExists`, `ResultExhausted`, `ResultUnavailable`, `ResultUnprovisioned`,
`ResultConflict`, `ResultDenied`.

List replies are multipart: a sequence index and a `More` flag mirror the
remoted admin pattern. Non-final chunks are notifications, so the IPC request
stays pending until the final reply.

## Supervision and grants

PID 1 registers `identityd` after the persistent volume mounts, with dependency
`vfs` and `RestartOnFailure`. It runs in normal mode and stops in recovery, and
its image joins `sessionServices` so boot validates it.

PID 1 reserves one of the eight VFS scope slots for the identity database:
`identityScopeSlot = vfs.MaxScopes - 1` (slot 7). remoted keeps its host-key
scopes on the prefix 0..6 and refuses to cross into slot 7. PID 1 creates and
validates `/state/identity` through its trusted setup session, then installs
the subtree scope that denies Read and Write to every session without the
grant, before ordinary sessions can be published.

PID 1 grants identityd three handles:

| Role | Rights | Notes |
| --- | --- | --- |
| `control` channel | Read, Write, Wait | consumed; PID 1 keeps the client end |
| `vfs` session | Read, Write, Wait on the channel | consumed; the session authority carries canonical root confinement to `/state/identity` plus Read, Write on the reserved slot |
| `log` device | Write | diagnostics |

The VFS session carries both root confinement and the reserved slot's grant:
`vfs.RootedAuthority` with `Confined: true` and
`Grants[identityScopeSlot] = Read|Write`. Root confinement alone leaves
ordinary sessions able to reach the subtree, and a grant alone would let
identityd escape its directory.

The identity and remote protections are tracked per VFS lifetime. When the VFS
restarts, PID 1 clears the installed markers and re-installs both scopes before
handing ordinary sessions back, then reissues identityd's control and VFS
endpoints. A failed install or an ambiguous identity root fails closed.

identityd is single-owner: one event loop, no mutation goroutines, and every
outbound VFS request uses a bounded deadline.

## Commands

`user` and `group` declare the `GrantIdentityAdmin` command grant
(`commands.GrantIdentityAdmin`, value `0x8000`), which bumps the command index
`IndexFormat` to 11. PID 1 maps the grant to one administration session over
identityd's control channel and hands it to the command as the `identity` role.
A command without the grant has no identity session and fails closed.
Enforcement stays in PID 1, never in identityd.

```text
user status | provision | reset --confirm
user list | show NAME | add NAME | disable NAME | enable NAME | delete NAME
user uid NAME UID | gid NAME GID | home NAME PATH | groups NAME ...
user key add NAME KEY | remove NAME FINGERPRINT
group list | add NAME | delete NAME | members NAME ...
```

`user status` prints `available accounts=N groups=N keys=N`, `unprovisioned`,
or `unavailable`. `user provision` commits an empty database from
Unprovisioned and never provisions implicitly on `user add`. `user reset
--confirm` is the explicitly destructive recovery operation: it requires
exactly `--confirm`, rejects any other argument, and destroys identity
records, not home directories or disk ownership metadata. The lifecycle
preconditions are enforced inside identityd; the CLI flag is not the
authorization boundary.

## SSH authorization seam

`userspace/sshserver.Config` gains an account-aware hook:

```go
AuthorizeKey func(username string, key ed25519.PublicKey) (bool, error)
```

The pinned SSH library keeps its signature verification and handshake. The
public-key callback invokes `AuthorizeKey`; a nil callback falls back to the
existing constant-time `Username`/`AuthorizedKeys` comparison, which remains the
production admission path. The callback returns an authorization decision only.
The library verifies proof of possession after the callback, and the worker
mints a session only after a successful handshake. On callback error,
authentication fails closed. The adapter is `identity.Store.AuthorizeKey`, true
only when an enabled account with that exact name holds the key. Production
multi-account admission is deferred to M5.

## Invariants

- Identity is data, not authority. A UID, group, key or account name never
  widens a native capability, a VFS scope or an administrative grant.
- UID 0 grants nothing. The kernel has no root user and no UID 0; Linux user and
  group numbers are presentation.
- One mutation owner per store. No write bypasses `Store`.
- One committed view per snapshot. The epoch, revision, IDs, and high-water
  marks advance together, and no mutation publishes before a durable save.
- No reuse. Internal IDs and automatically issued UID/GID numbers never get
  reused; deletion and reassignment never lower a high-water mark.
- Fail closed on corruption and uncertain durability.
- No silently initialized database. Provisioning is explicit.
- Protocol discipline on the versioned codec.
