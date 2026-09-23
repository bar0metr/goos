# ADR 0074: Typed configuration service

## Status

Accepted. Extends ADR 0018 and ADR 0071.

## Context

The system had no configuration. Every setting was a constant compiled into the
component that used it: the shell's prompt, the volume a service mounts, whether
the log is persisted. Changing one meant rebuilding the image, and nothing could
report what the running system was configured to do.

The specification is explicit about what not to build here. Configuration is not
to be a directory of text files under `/etc` that each component parses its own
way, because that makes the format a convention rather than a contract and the
parser a per-component liability. It asks instead for a typed service with
namespaces, validation, read-only namespaces, capability checks, and an
export/import form a person can read.

The pieces it needs now exist: a persistent volume, a named place for mutable
system state, and the service pattern the log service established.

## Decision

**Settings are declared, not invented.** A key carries a type, and the running
image declares the schema at startup. A write is checked against that
declaration, so a wrong type is refused and a mistyped key is a failure rather
than a new setting that nothing reads. This is what makes the store typed in a
way a string map is not.

**A key names a namespace and a setting in it.** Keys are dotted, lower case,
and need at least two components, so `system.shell.prompt` and `user.name` are
keys and `prompt` is not. That is also what makes a scope meaningful.

**Authority is a property of the session, not of the message.** A session
carries a scope — the namespace prefix it is confined to — and rights to read
and to write. The service applies them to every request; the message carries no
authority of its own and cannot ask for more. PID 1 creates the session and
decides what it may do.

**Out of scope reads as absent.** A session confined to `user.` is told a
`system.` key does not exist rather than that it is denied, so it learns nothing
about what exists outside its namespace.

**Some values are published, not configured.** What the running image is — its
name, release and machine — is declared read-only and nothing may write it. A
stored copy would be a second source of truth about what is executing.

**Scopes match whole components.** `user.` covers `user.name` and never
`username.x`, the same rule the VFS namespace uses for mount prefixes.

**The exported form is the stored form.** One entry per line, `key type value`,
which is what the specification means by export and import in a human-readable
format and also what lands on `/state/config`. Naming the type on each line is
what lets a value read back be checked against the declaration rather than
guessed from its shape. Read-only entries are not exported.

**A damaged file does not stop the boot.** A line that does not parse, names an
undeclared key, or carries the wrong type is skipped and counted, and the system
comes up with its declared defaults. The count is logged.

**The whole store is rewritten after an accepted write.** Configuration is
bounded and small, so this is simpler than tracking which entry changed and it
leaves the file consistent with the store after every write.

**`config` is a shell builtin.** It administers the shell's own session, in the
same family as `services`, `handles`, `caps` and `log`. An external utility
would need a session of its own, and the shell's is the one PID 1 grants.

**There is one session today.** The mechanism is a session with a scope and
rights, and the store enforces whatever a session is given; the deployment
grants exactly one, to the shell, with the whole namespace and both rights.
Additional sessions with narrower scopes need a control endpoint like the VFS
one, which is a known shape and is not built here.

**Transactional update is not implemented.** The specification lists it as
future work, and a single-writer store with a bounded rewrite has nowhere to put
a half-applied change today.

## Alternatives

**Files under `/etc`.** Familiar, needs no service, and every tool can read it.
It also makes each component its own parser of untrusted text, gives no types,
no validation and no way to refuse a write, and it is the model the architecture
explicitly rejects.

**An untyped key-value store.** A map of strings would accept any key and any
value and need no schema. It would also accept `system.shell.prompt` set to a
number and `system.shel.prompt` set to anything, and the first component to read
either would be the one that discovered the problem.

**Rights on the IPC endpoint instead of the session.** The channel already
carries read and write rights, so a read-only client could be given a read-only
endpoint. Those rights say whether a message may be sent, not what it may ask
for: a client that can send at all can send a write request, and the service
would still have to decide. Keeping configuration authority in the session
states it once, where it is enforced.

**Letting a client declare its own keys.** An open namespace where the first
write declares the type would let `user.` grow without a schema change. It also
reintroduces the typo-becomes-a-setting failure the declaration rule exists to
prevent, in the one namespace where a person types the keys by hand.

**A binary stored form.** Compact and unambiguous, and it would need no parser
for the text. It would also not be readable or editable from the host, which is
the point of the export format the specification asks for, and the store is far
too small for the size to matter.

## Consequences

`config` lists every setting with its value and marks the published ones,
`config get KEY` reads one and `config set KEY VALUE` writes one. A write to a
published value answers "setting is read-only", a wrongly typed value answers
"value does not match the declared type", and an undeclared key answers "no such
setting". The boot test asserts all four, and that a value set in one session is
restored in the next.

`/state/config` holds the writable settings in the exported form and can be read
with `cat` or edited from the host.

The shell resolves a key's declared type before parsing the argument, so a value
is converted by what the key is rather than by how the text looks: `config set
system.log.persist 1` is refused because the key is a bool, not silently
accepted as one.

The service starts after the filesystem and before the shell, and a failure to
start it warns rather than stopping the boot. The shell's configuration session
is optional for the same reason, and `config` reports the service as unavailable
when there is none.

Nothing reads configuration yet. The schema declares settings that describe the
system — the prompt, the storage volume, whether the log is persisted — and
making the components that own them consult it is the next step; the store,
its protocol and its persistence are what had to exist first.
