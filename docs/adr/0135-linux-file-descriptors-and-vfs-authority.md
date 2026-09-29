# ADR 0135: Linux file descriptors and VFS authority

## Status

Accepted design for GoOS 0.2.0.

## Context

Linux programs name I/O objects with small file descriptors. GoOS uses typed,
generation-safe capabilities and explicit VFS sessions. Exposing native handle
values as file descriptors would leak the native ABI and its authority model.

## Decision

Each Linux Process owns a bounded compatibility fd table. An fd is an index
into compatibility state, never a GoOS handle. Entries refer to supplied stream
capabilities or to open files reached through exactly one process-owned VFS
session, with bounded flags and offset state. Descriptors 0, 1, and 2 bind only
to the streams supplied by launch policy.

Path operations issue VFS IPC through that session. They never call a backend
or filesystem implementation directly. Teardown closes every fd and the VFS
session; peer death produces a Linux error and cannot trigger a privileged
fallback. Duplication, when implemented, shares an open-file description and
offset while retaining per-fd flags such as close-on-exec.

## Consequences

Linux descriptors cannot be guessed into native capabilities. Filesystem
authority remains process-scoped, restart behavior remains explicit, and the
table has a fixed resource bound.
