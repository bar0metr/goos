# ADR 0044: Bounded stdout file redirection

## Status

Accepted.

## Context

Priority 3 requires file redirection without making POSIX file descriptors part
of the native ABI. ADRs 0040 through 0043 already provide directional byte
streams, explicit startup roles, and bounded multistage pipelines. PID 1 should
not gain filesystem parsing or direct file-write policy merely to implement a
shell operator.

## Decision

Support one unquoted, unescaped `>` operator with exactly one target argument.
Reject append syntax, multiple targets, builtin redirection, and commands that
would exceed the four-stage pipeline bound. Quoted and escaped `>` bytes remain
ordinary argument data.

Represent stdout redirection as an additional `/system/bin/write` pipeline
stage. The utility receives a read-only `in` stream endpoint and its normal
private VFS session. It resolves the target against the inherited cwd, creates
or truncates the file, writes bytes until stream EOF, and reports failures on
its separate stderr capability. PID 1 continues to use the existing version 4
exec protocol, downstream-first launch, capability attenuation, and cleanup.
The startup and kernel-call ABIs are unchanged.

Move native success markers used by validation away from redirected stdout when
they would otherwise become file data. User data remains on stdout; diagnostics
and validation markers use stderr.

## Alternatives

A VFS-backed kernel stream would put filesystem policy in the kernel and couple
unrelated object types. Extending the exec protocol with file paths would make
PID 1 responsible for filesystem semantics. POSIX descriptor duplication would
violate the native ABI direction. A userspace adapter preserves existing
boundaries and capability checks.

## Consequences

Redirection consumes one of four bounded pipeline stages, so at most three user
stages may precede `>`. Writes are intentionally byte-oriented in this first
correctness slice and are not a performance fast path. Input, stderr, and append
redirection remain future work. Host tests cover lexical bounds; the QEMU gate
executes `echo ... > file` and reads the file back through VFS.
