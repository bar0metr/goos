# Configuration

Configuration is a typed service, not a directory of text files. A key is
declared with a type before it can be set, a write is checked against that
declaration, and a client reaches the store over a session whose scope and
rights decide what it may see and change (ADR 0074).

## Keys and types

A key is dotted, lower case, and has at least two components, so it always names
a namespace and a setting within it. Values are one of three types:

| Type | Values |
| --- | --- |
| `bool` | `true` or `false` |
| `uint` | an unsigned decimal |
| `text` | up to 64 printable ASCII bytes |

The running image declares the schema at startup. A key it does not declare is
not a setting: setting one answers "no such setting" rather than creating it, so
a typo fails instead of becoming a value nothing reads.

Some entries are published rather than configured — the name, release and
machine of the running image. They are declared read-only, nothing may write
them, and they are not part of the stored file, because a stored copy would be a
second source of truth about what is executing.

## Sessions

A session carries a **scope**, the namespace prefix it is confined to, and
**rights** to read and to write. The service applies them to every request; a
message carries no authority of its own. PID 1 creates the session and decides
what it may do.

A key outside the scope reads as absent rather than denied, so a session
confined to `user.` learns nothing about what exists elsewhere. Scopes match
whole components: `user.` covers `user.name` and never `username.x`.

Today the deployment grants one session, to the shell, with the whole namespace
and both rights. Narrower sessions need a control endpoint like the VFS one.

## Protocol

`GCF1` version 1 carries an operation, a key and a typed value:

| Operation | Reply |
| --- | --- |
| `Get` | the value and whether it is read-only |
| `Set` | a status |
| `List` | one entry and a cursor, within the session's scope |

Refusals are typed: `NotFound`, `ReadOnly`, `TypeMismatch`, `PermissionDenied`,
`InvalidArgument`. There are no string commands and no untyped blobs, so a
malformed message is rejected at the boundary rather than interpreted.

## Storage

Writable entries live at `/state/config` in the exported form, one per line:

```text
system.log.persist bool true
system.shell.prompt text goos>
system.storage.volume text /persist
user.name text goos
```

This is the import format as well, and it is what a person reads or edits from
the host. Naming the type on each line is what lets a value read back be checked
against its declaration rather than guessed from its shape.

The whole store is rewritten after an accepted write, which is simpler than
tracking which entry changed and leaves the file consistent with the store. A
line that does not parse, names an undeclared key, or carries the wrong type is
skipped and counted at startup, and the system comes up with its declared
defaults.

Transactional update is future work: a single-writer store with a bounded
rewrite has nowhere to put a half-applied change today.

## Shell

`config` is a builtin, in the family of `services`, `handles`, `caps` and `log`,
because it administers the shell's own session:

```text
goos> config
system.build.release 0.1.0-dev (read-only)
system.log.persist true
system.shell.prompt goos>
goos> config get system.shell.prompt
goos>
goos> config set system.log.persist false
```

`config set` resolves the key's declared type first and parses the argument
against it, so a value is converted by what the key is rather than by how the
text looks.
