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
persisted monotonic counters. The store refuses to allocate at `math.MaxUint64`.
Deletion never reuses an internal ID. The UID/GID are separate `uint32`
numbers that an administrator may reuse explicitly, but an old internal ID
never aliases a new account or group.

Reserved Unix numbers are `0` and `0xFFFFFFFF`; allocation and explicit
assignment reject both. Deterministic allocation picks the lowest unused value
at or above `1000`, for UID and GID independently.

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

Encoding is explicit fixed-endian, deterministic, and re-encodes byte-for-byte.
The file is:

```
magic  "GOOS-IDENTITY-V1\n" (17 bytes)
version uint16 = 1
sequence uint64 (monotonic, increments each committed mutation)
payload canonical encoding of all accounts, groups, nextAccountID, nextGroupID
digest  sha256(magic || version || sequence || payload) (32 bytes)
```

`sequence` is the durability/linearization marker, not a session token.

Writes go through `safesave.Save` (write `.<name>.~save` beside the target,
close, `Replace`). On FAT32 `Replace` is the durability barrier. A `Replace`
that fails late may have committed: identityd reconciles with a forward retry
from storage, never by assuming the old snapshot survived. The store never
truncates a snapshot in place and never rewrites a committed generation.

Load validates magic, version, lengths, counts, digest, then the payload
(bounds, duplicates, dangling references). Any failure is corruption.

### 4. Availability and recovery

identityd reports one of three states:

- `Available`: loaded and valid.
- `Unprovisioned`: the directory has no snapshot and no stale temp file.
  identityd serves the admin endpoint; lookup and data operations deny.
- `Unavailable`: the snapshot is corrupt, or a write ended with uncertain
  durability and reconciliation did not produce a validated snapshot. Lookup
  denies; data operations return unavailable.

Provisioning is explicit: `AdminProvision`, or the first successful account or
group creation from `Unprovisioned`, writes an empty valid snapshot. identityd
never initializes a database on its own at boot. Corrupt data is never
regenerated over; recovery is `AdminReset` (destructive, requires an explicit
administrator action) after which `AdminProvision` applies. Local console
administration and this recovery path do not depend on UID 0, on the account
database being valid, or on remote login.

### 5. IPC

`userspace/identityproto` is the versioned codec, one shared package imported by
identityd, the administration commands, and PID 1. `ProtocolID =
0x4749444e545931` ("GIDNTY1"), `Version = 1`. Payloads fit the 256-byte inline
IPC limit; list replies are multipart with a sequence index and a `More` flag,
mirroring the remoted admin multipart pattern.

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

PID 1 grants identityd exactly: a `control` channel (PID 1 keeps the client
end), a `vfs` session confined to `/state/identity`, and the `log` device.
The VFS confinement follows the remoted `/state/security/ssh` pattern:
`vfs.InstallScope` on `/state/identity` (subtree, denied `Read|Write`) plus
`vfs.AddSessionWithAuthorityRequest` with `Confined: true` and that slot
granted `Read|Write`.

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
user list | show NAME | add NAME | disable NAME | enable NAME | delete NAME
user uid NAME UID | gid NAME GID | home NAME PATH | groups NAME ...
user key add NAME KEY | remove NAME FINGERPRINT
group list | add NAME | delete NAME | members NAME ...
```

Both declare a new command grant, `commands.GrantIdentityAdmin` (next free
bit), bumping `IndexFormat` to 11 and adding the corresponding case in
`tools/commands`. PID 1 maps the grant to an identity administration session
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
and availability policy. M5 must also bind the verified account identity to a
supervisor-controlled session record; M4 deliberately does not mint account
sessions.
