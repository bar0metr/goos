# ADR 0031: Versioned native process startup

## Status

Accepted

## Context

Native services and utilities assumed that capabilities occupied fixed numeric
handle slots. That made application behavior depend on kernel insertion order
and prevented capability manifests from evolving independently of binaries.
The previous command, working-directory, and argument block also had no format
identifier or version.

## Decision

Native launches carry a bounded `GST1` startup block. Its little-endian wire
format contains a version, total size, command name, inherited working
directory, bounded raw argument data, and up to seven named handle bindings.
Names are unique, lowercase capability roles of at most 15 bytes.

PID 1 creates an unbound launch template whose binding order matches the exact
capability manifest. The kernel first derives and installs every capability,
then replaces the template's zero handle values with the actual generation-safe
process-local IDs. It rejects count mismatches, malformed templates, duplicate
names, unresolved IDs, and blocks larger than 128 bytes before making the first
task runnable.

Applications resolve handles by role such as `console`, `vfs`, `exec`,
`session`, `control`, and `stdout`. The handle table remains the authority for
type and rights checks; startup names do not grant authority.

## Consequences

Shell, VFS, and native utilities no longer depend on global numeric handle IDs
or insertion order. Capability rights remain bounded by the launch manifest.
The 128-byte startup limit intentionally constrains the combined command, cwd,
arguments, and binding names. Structured `argv`, environment entries, and
separate stdin/stdout/stderr roles remain later extensions of this versioned
contract.
