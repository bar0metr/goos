# ADR 0045: Bounded stdout append redirection

## Status

Accepted.

## Context

ADR 0044 routes stdout redirection through an isolated `write` consumer with a
private VFS session. Priority 3 also requires append redirection without adding
file paths or filesystem policy to the exec protocol, startup ABI, or kernel.

## Decision

Accept one unquoted, unescaped `>>` operator under the same parsing, path, and
four-stage bounds as `>`. The shell starts the existing `/system/bin/write`
consumer with an append mode argument. After creating the target if necessary,
the consumer reads its current size through VFS, opens it for writing, seeks to
that size with the existing VFS `Seek` operation, and drains stdin to the file.

The kernel, native startup ABI, exec IPC protocol, capability manifests, and
VFS protocol remain unchanged.

## Consequences

Append redirection preserves existing bytes and consumes the same final pipeline
stage as truncate redirection. It has the existing bounded userspace VFS
semantics rather than POSIX atomic append semantics. The QEMU gate writes a file
with `>`, appends with `>>`, and verifies both lines through the VFS path.
