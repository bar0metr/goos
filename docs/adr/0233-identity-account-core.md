# ADR 0233: Identity and account core

## Status

Accepted.

## Context

GoOS 0.4.0 adds stable account identities so that later milestones can bind
Unix metadata, Linux credentials, and remote sessions to a real principal.
This card (M4) delivers only the identity core: a bounded account/group/key
store, durable single-snapshot persistence, generation-safe IDs, a supervised
identity service, administration commands, and an account-aware SSH
key-authorization seam. It does not yet install principals into VFS sessions,
mint Linux credentials, mutate filesystem ownership, or switch production SSH
admission to per-account logins. Those arrive in M5.

The security model does not change. Identity is data, not authority. A UID,
group, key, or account name never widens a native capability, a VFS scope, or
an administrative grant.

## Decision

### 1. Bounded data model

The domain types and all bounds live in one host-testable package,
`userspace/identity`. It imports only the standard library and `goos/internal/limits`.

| Constant | Value | Meaning |
| --- | ---: | --- |
| `MaxAccounts` | 64 | active accounts |
| `MaxGroups` | 64 | active groups |
| `MaxSupplementaryGroups` | 16 | supplementary groups per account |
| `MaxName` | 31 | account and group name bytes |
| `MaxHome` | 64 | home path bytes (matches the VFS root-path limit) |
| `MaxKeysPerAccount` | 8 | public keys per account |
| `MaxTotalKeys` | 128 | public keys across all accounts |
| `KeySize` | 32 | Ed25519 public key bytes |
| `FingerprintSize` | 32 | SHA-256 digest bytes for key removal |
| `MaxSnapshotBytes` | 65536 | hard cap on the encoded snapshot |

Account name and group name grammars are `[A-Za-z0-9_-]{1,31}`, matching
`remotepolicy.ValidUser`. Account names and group names occupy separate
namespaces.

An account holds: a stable `AccountID uint64`, a name, a `uint32` UID, a
primary `GroupID uint64`, up to 16 supplementary `GroupID`s, a home path, an
enabled flag, and up to 8 Ed25519 public keys. A group holds a stable
`GroupID uint64`, a name, and a `uint32` GID.

Internal IDs are distinct from Unix numbers. `AccountID` and `GroupID` are
persisted monotonic counters that start at 1 and never wrap or get reused.
The store refuses to allocate at `math.MaxUint64`. The UID/GID are separate
`uint32` numbers that an administrator may reuse explicitly, but an old
internal ID never aliases a new account or group.

Reserved Unix numbers are `0` and `0xFFFFFFFF`; allocation and explicit
assignment reject both. Allocation uses persistent `nextUID` and `nextGID`
high-water marks: each starts at `AllocBase` (1000) and only increases, so an
automatically issued number is never reused. A mark is a `uint64` bounded by
the uint32 domain; `0xFFFFFFFF` is the exhausted sentinel. Explicit assignment
of a free non-reserved number raises the mark when it crosses it. Deletion and
reassignment never lower a mark.

### 2. Store operations

`identity.Store` exposes, in one place, the mutations the admin commands need
and the queries the SSH adapter needs. Mutation targets are named; the single
owner resolves names to internal IDs at call time, so no stale-name race
exists.

- `CreateAccount(name)`: the user-private-group model. It auto-allocates a UID
  and GID, creates the account, and creates a private primary group named
  after the account.
- `DeleteAccount(name)`: removes the account and, when its primary group is
  the auto-created private group referenced by no other account, removes that
  group in the same update. A reassigned or shared primary group is left.
- `SetEnabled`, `SetUID`, `SetPrimaryGID`, `SetHome`, `SetGroups`, `AddKey`,
  `RemoveKey`.
- `CreateGroup`, `DeleteGroup`, `SetMembers`.
- `AuthorizeKey(username, key) (bool, error)`: true only when an enabled
  account with that exact name holds that key. Disabled, missing, and
  unavailable stores deny.
- `AccountByName`, `GroupByName`, `AccountByUID`, `GroupByGID`, `Accounts`,
  `Groups` in deterministic order (account/group ID ascending, supplementary
  groups ascending, keys sorted by fingerprint).

Cross-references in mutations use Unix numbers: `SetPrimaryGID(name, gid)`
and `SetGroups(name, gids...)` take GIDs, `SetMembers(name, uids...)` takes
UIDs, and the store resolves them to internal IDs. `RemoveKey` identifies the
key by its SHA-256 fingerprint.

Validation rejects malformed names, duplicate active names, duplicate active
numeric IDs, duplicate keys within a record, overflows, and dangling group
references. Deleting a group that any account references as primary or
supplementary fails with a conflict; the administrator reassigns first.

### 3. Persistence

One versioned snapshot file, `/state/identity/db`. No per-account or per-group
files. Public keys are not a second authority source.

Encoding is explicit fixed little-endian, deterministic, and re-encodes
byte-for-byte. The file is:

```text
magic   "GOOS-IDENTITY-V2\n" (17 bytes)
version uint16 = 2
sequence uint64 (monotonic, increments each committed mutation)
payload epoch [16]byte, nextAccountID, nextGroupID, nextUID, nextGID, accounts, groups
digest  sha256(magic || version || sequence || payload) (32 bytes)
```

The payload leads with the 128-bit database epoch, then the four counters
`nextAccountID`, `nextGroupID`, `nextUID`, `nextGID`, then the account and
group records. The snapshot format version (2) is independent of the IPC
protocol version. The pre-release V1 format (`GOOS-IDENTITY-V1\n`, version 1)
is refused with `ErrOldFormat`; M4 never silently upgrades it or invents
high-water history. Recover a V1 file by backup and re-provision, or by an
explicit tested migration.

The canonical identity reference for an account is `(DB epoch, AccountID)`,
and for a group `(DB epoch, GroupID)`. After corrupt recovery mints a fresh
epoch, stale internal references stay distinguishable even when the new
numeric IDs restart at 1.

`sequence` marks durability and linearization, not a session token.

A mutation is a real commit boundary. The single-owner coordinator clones the
committed store, applies the change, validates and encodes the candidate, then
persists it before publishing:

```text
clone committed store -> apply -> validate -> encode
  -> exclusive temp create -> full write -> Sync(temp handle) -> checked Close
  -> Replace(temp, db) -> Sync(containing directory)
  -> publish candidate
```

`Close` is not fsync. A required unsupported or failed `Sync`/`SyncDir` is not
success. On FAT32 `Replace` is a sector-level old-or-new switch, not ext4
journal crash atomicity, so a torn operation can leave the target
indeterminate; identityd treats that as Unavailable and never assumes the old
snapshot survived. A definite failure before the rename leaves the old
committed store in place. An ambiguous rename or a failed final directory sync
fails closed into Unavailable and reconciles only after a fresh lifetime and a
verified durability barrier.

The temporary file uses one reserved name, `.<name>.~save` beside the target,
created exclusively, with no counter. A stale temp left by a prior writer is
reconciled at startup against a bounded directory listing: with the db present
the temp is removed and its removal verified; with the db absent the state is
Unavailable (interrupted provisioning), never silently promoted.

Load validates magic, version, lengths, counts, digest, then the payload
(epoch nonzero, bounds, high-water marks, duplicates, dangling references).
Any failure is corruption.

### 4. Availability and recovery

identityd reports one of three states:

- `Available`: loaded and valid.
- `Unprovisioned`: the directory has no snapshot and no stale temp file.
  The admin endpoint serves; lookup and data operations deny.
- `Unavailable`: the snapshot is corrupt, or a write ended with uncertain
  durability and reconciliation produced no validated snapshot. Lookup denies;
  data operations return unavailable.

Provisioning and reset are explicit and enforce their preconditions inside
identityd, not only in the CLI:

| Operation | Unprovisioned | Available | Unavailable |
| --- | --- | --- | --- |
| Status | report state | report state and counts | report state |
| Provision | commit empty DB with a fresh epoch | Conflict, unchanged | denied, unchanged |
| Reset | Conflict; use provision | commit empty DB, preserve epoch and high-water marks, advance revision | commit empty DB with a fresh epoch and fresh counters |
| Data mutation | denied | validated durable update | denied |
| Key authorization / lookup | denied | current committed data only | denied |

A valid store is never reset by provisioning, and corrupt data is never
regenerated over. Reset destroys identity records, not home directories or
disk ownership metadata. Local console administration and recovery do not
depend on UID 0, on a valid account database, or on remote login.

### 5. IPC

`userspace/identityproto` is the versioned codec, one shared package imported by
identityd, the administration commands, and PID 1. `ProtocolID =
0x4749444e545931` ("GIDNTY1"), `Version = 2`. Version 2 adds the database
epoch, the committed revision, and the account's supplementary group Unix GIDs
to each record so a lookup reply is one coherent committed principal view.
Payloads fit the 256-byte inline IPC limit; list replies are multipart with a
sequence index and a `More` flag, mirroring the remoted admin multipart
pattern.

The control channel carries two operations, each with one attached session
handle, sent by PID 1 only:

- `OpAddAdmin`: open an administration session.
- `OpAddLookup`: open a lookup/authorization session.

Administration operations: `AdminStatus`, `AdminProvision`, `AdminReset`,
`AdminListAccounts`, `AdminListGroups`, `AdminGetAccount`, `AdminGetGroup`,
`AdminAddAccount`, `AdminDeleteAccount`, `AdminSetEnabled`, `AdminSetUID`,
`AdminSetGID`, `AdminSetHome`, `AdminSetGroups`, `AdminAddKey`,
`AdminRemoveKey`, `AdminAddGroup`, `AdminDeleteGroup`, `AdminSetMembers`.

Lookup operations: `LookupAuthorizeKey`, `LookupGetAccount`.

Each get and list record carries the stable internal ID, the Unix UID and GID,
name, home, enabled flag, key count, the 128-bit database epoch, the committed
revision, and, for accounts, the supplementary group Unix GIDs sorted
ascending. The epoch and revision name one committed view for key
authorization and M5 admission.

Result codes distinguish `ResultOK`, `ResultInvalid`, `ResultNotFound`,
`ResultExists`, `ResultExhausted`, `ResultUnavailable`, `ResultUnprovisioned`,
`ResultConflict`, `ResultDenied`.

The codec follows the repository rules: explicit little-endian field-by-field
encoding, zero reserved bytes on encode and rejection of nonzero reserved bytes
on decode, exact version match, unknown-opcode rejection, bounded counts and
lengths, and hostile-input tests.

### 6. Service supervision

`identityd` is a supervised installed-volume service (`userspace/cmd/identityd`),
registered in `userspace/cmd/init/services.go` after the persistent volume
mounts, with dependency `vfs` and `RestartOnFailure`. It runs in normal mode
and is stopped in recovery. Its image joins `sessionServices` so boot validates
it.

PID 1 reserves one of the eight VFS scope slots for the identity database:
`identityScopeSlot = vfs.MaxScopes - 1` (slot 7). remoted keeps its existing
host-key scopes on the prefix 0..6, and its install and reload paths refuse to
cross into slot 7. PID 1 creates and validates `/state/identity` through its
trusted setup session before installing the subtree scope that denies
`Read|Write` to every session without the grant, and it installs that scope
before ordinary sessions can be published.

PID 1 grants identityd exactly: a `control` channel (PID 1 keeps the client
end), a `vfs` session, and the `log` device. The VFS session carries both
canonical root confinement to `/state/identity` and the reserved slot's
`Read|Write` grant: `vfs.RootedAuthority` with `Confined: true` and
`Grants[identityScopeSlot] = Read|Write`. Root confinement alone leaves
ordinary sessions able to reach the subtree, and a grant alone would let
identityd escape its directory, so both are required.

The identity and remote protections are tracked per VFS lifetime. When the VFS
restarts, PID 1 clears the installed markers and re-installs both scopes before
handing ordinary sessions back, then reissues identityd's control and VFS
endpoints. A failed install or an ambiguous identity root fails closed: no
unprotected publication of identity state.

identityd is single-owner: one event loop, no mutation goroutines. Every
outbound VFS request uses a bounded deadline.

### 7. SSH key-authorization seam

`userspace/sshserver.Config` gains

```go
AuthorizeKey func(username string, key ed25519.PublicKey) (bool, error)
```

The backend keeps the pinned SSH library's signature verification and handshake
intact. The public-key callback invokes `AuthorizeKey`; a nil callback falls
back to the existing constant-time `Username`/`AuthorizedKeys` comparison,
which remains the production admission path. The callback returns only an
authorization decision: it grants nothing. A probe never produces a session;
the library verifies the signature after the callback, and the worker mints a
session only after a successful handshake. On callback error, authentication
fails closed.

The adapter is `identity.Store.AuthorizeKey`, host-tested through the real SSH
handshake with a signed positive and a signed negative case. Production
multi-account admission is deferred to M5.

### 8. Administration commands

`userspace/bin/user` and `userspace/bin/group` expose the §21 surface:

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

Both declare a new command grant, `commands.GrantIdentityAdmin` (`0x8000`,
next free bit), bumping `IndexFormat` to 11 and adding the corresponding case
in `tools/commands`. PID 1 maps the grant to an identity administration session
over identityd's control channel, mirroring `openRemoteAdmin`. A command
without the grant has no identity session and fails closed. Enforcement stays
at PID 1, never in identityd.

### 9. Capabilities

`identity-admin` is a command grant, not a kernel object right and not a VFS
right. No UID, mode, group, or key grants native authority. The kernel rights
bitfield and VFS rights bitfield are unchanged.

### 10. Resource bounds and invariants

Every new count, length, and allocation is bounded by §1 constants, inventoried
in `docs/architecture/resource-policy.md`. The service reuses the `normal`
resource profile. No wire- or disk-driven allocation is unbounded.

The non-negotiable invariants from the specification hold: identity is never
authority; one mutation owner per store; fail closed on corruption and
uncertain durability; no silently initialized database; protocol discipline on
the new versioned codec.

## M5 preconditions

M5 switches production SSH admission to account-aware logins and installs a
trusted principal into VFS sessions. Before M5 activates, this card must have
proven: the key-authorization API and adapter, key isolation, disabled and
missing accounts, key removal and policy changes, and the durable corruption
and availability policy. The lookup endpoint returns a bounded coherent
principal view: the stable `AccountID`, the 128-bit database epoch, the
committed revision, the current UID, the primary GID, supplementary groups,
home, and enabled state, all from one committed snapshot. M5 admission must
revalidate against that same committed view. M5 must also bind the verified
account identity to a supervisor-controlled session record; M4 deliberately
does not mint account sessions.
