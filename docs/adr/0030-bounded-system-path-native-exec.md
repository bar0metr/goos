# ADR 0030: Bounded system-path native execution

## Status

Accepted

## Context

The shell and PID 1 both contained a hard-coded `pwd`, `ls`, and `cat` allowlist.
Startup data encoded those programs as numeric selectors, so adding a native
program required coordinated shell, init, and runtime changes.

## Decision

The shell splits an unrecognized command into one command token and an unparsed
bounded argument tail. It resolves only simple command names beneath the fixed
`/system/bin/` search root. PID 1 independently validates the same root, looks
up the exact initramfs entry, creates a fresh VFS channel pair, grants console
write and that one VFS client endpoint, and waits through the returned process
handle.

Startup data now carries a bounded command name, working directory, and raw
argument tail. Multicall images select behavior from the command name; ordinary
native images may ignore it. The initramfs includes an `args` hardlink to prove
that a new executable can be exposed without extending an allowlist.

Lifecycle notifications that race ahead of a correlated launch reply are kept
in a bounded PID 1 queue and processed after the reply. This prevents fast
program exit from being misclassified as launch failure.

## Consequences

Any valid native image explicitly installed in `/system/bin` can be launched by
name, but relative paths, nested paths, and traversal remain rejected. Every
launch retains its isolated VFS session and minimal capability manifest.
Quoting, multiple parsed arguments, pipelines, and redirection remain future
shell grammar work.
