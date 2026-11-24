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
| `userspace/identityd` | the single-owner service and availability state machine | yes; imports only `identity` and `identityproto` |

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
reject both reserved values, `0` and `0xFFFFFFFF`. Deterministic allocation
picks the lowest unused value at or above `AllocBase` (1000), for UID and GID
independently. Explicit assignment also rejects duplicates.

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
magic   "GOOS-IDENTITY-V1\n" (17 bytes)
version uint16 = 1
sequence uint64 (monotonic, increments each committed mutation)
payload canonical encoding of accounts, groups, nextAccountID, nextGroupID
digest  sha256(magic || version || sequence || payload) (32 bytes)
```

`sequence` marks durability and linearization; it is not a session token. Writes
go through `safesave`: write `.<name>.~save` beside the target, close, then
`Replace`. On FAT32 the `Replace` is the durability barrier. A `Replace` that
fails late may have committed, so identityd reconciles forward from storage
rather than assuming the old snapshot survived. The store never truncates a
snapshot in place and never rewrites a committed generation.

Load validates magic, version, lengths, counts and digest, then the payload:
bounds, duplicates and dangling references. Any failure is corruption.

## Availability

identityd reports one of three states.

| State | Condition | Behaviour |
| --- | --- | --- |
| `Available` | loaded and valid | data and lookup operations serve |
| `Unprovisioned` | no snapshot and no stale temp file | the admin endpoint serves; lookup and data operations deny |
| `Unavailable` | corrupt snapshot, or a write with uncertain durability whose reconciliation produced no validated snapshot | lookup denies; data operations return unavailable |

Provisioning is explicit. `AdminProvision`, or the first account or group
creation from `Unprovisioned`, writes an empty valid snapshot. identityd never
initializes a database on its own at boot, and it never regenerates corrupt data
over. Recovery is `AdminReset` (destructive, explicit) followed by
`AdminProvision`. Local console administration and recovery do not depend on UID
0, on a valid account database, or on remote login. The policy fails closed:
any corruption or uncertain durability denies lookups and data operations
instead of serving partial or guessed state.

## IPC

`userspace/identityproto` defines `GIDNTY1`: `ProtocolID = 0x4749444e545931`,
`Version = 1`. Messages travel in the `kernel/ipc.Message` envelope, a 256-byte
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

PID 1 grants identityd three handles:

| Role | Rights | Notes |
| --- | --- | --- |
| `control` channel | Read, Write, Wait | consumed; PID 1 keeps the client end |
| `vfs` session | Read, Write, Wait on the channel | consumed; the session authority confines every path to `/state/identity` with Read, Write on the scope slot |
| `log` device | Write | diagnostics |

The VFS confinement follows the remoted `/state/security/ssh` pattern: PID 1
installs the `/state/identity` subtree denied Read and Write, then opens a
confined session (`Confined: true`) whose scope slot carries Read and Write.
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
user list | show NAME | add NAME | disable NAME | enable NAME | delete NAME
user uid NAME UID | gid NAME GID | home NAME PATH | groups NAME ...
user key add NAME KEY | remove NAME FINGERPRINT
group list | add NAME | delete NAME | members NAME ...
```

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
- Fail closed on corruption and uncertain durability.
- No silently initialized database. Provisioning is explicit.
- Protocol discipline on the versioned codec.
