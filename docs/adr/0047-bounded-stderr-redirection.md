# ADR 0047: Bounded stderr redirection

## Status

Accepted.

## Context

Stderr redirection was the last unfinished Priority 3 shell-composition item.
ADRs 0040 through 0046 already provide capability-protected byte streams,
explicit named standard-stream roles, bounded multistage pipelines, and stdout
and stdin file redirection through isolated utilities. Diagnostics must be
divertible without giving PID 1 filesystem policy, without POSIX descriptor
duplication semantics, and without a second privileged path for error output.

## Decision

Accept one unquoted, unescaped `2>` or `2>>` operator with exactly one path
target, recognised only when the `2` digit begins a word so that ordinary
arguments ending in `2` stay data. Input, stdout, and stderr redirection may be
combined while the producer, requested stages, and sinks fit the existing
four-stage bound.

Represent stderr redirection as an additional `/system/bin/write` stage that is
deliberately *outside* the stdout chain. The exec protocol advances to version 5
and carries `ErrorSource`: zero means no stderr sink, otherwise one plus the
index of the stage whose stderr feeds the final stage. PID 1 creates one private
stream for that pair, grants the sink a read-only endpoint plus its own VFS
session, and grants the source a write-only endpoint as its `stderr` role. The
sink is launched first so its reader exists before the source writes. Endpoint
ownership, downstream-first launch, partial-failure cleanup, and exit-code
reporting are unchanged, and the reported pid remains that of the final stdout
stage.

The startup ABI, kernel calls, VFS protocol, and capability model are unchanged.

## Alternatives

Merging stderr into the stdout chain would corrupt user data ordering and make
the sink read both roles. Teaching PID 1 to open files would move filesystem
policy into the supervisor. Descriptor duplication (`2>&1`) would import POSIX fd
semantics into the native ABI and is deliberately left out of this slice.

## Consequences

A stderr sink consumes one of the four bounded pipeline stages, so a redirected
pipeline has correspondingly fewer user stages. Only the last requested stage's
diagnostics are diverted; earlier stages keep the console. Writes are
byte-oriented rather than a performance fast path, and `2>&1`-style stream
merging remains future work.

Implementing this surfaced a latent kernel race: the architecture task-slot
allocator reused any slot marked dead, including one whose exit record had not
been delivered yet. Reuse bumped the slot generation, so the pending exit no
longer matched its owning process; the process kept a live task count and every
`ProcessWait` on it timed out forever, hanging PID 1 and the shell. It also
orphaned the dead task's guarded kernel stack. A stderr sink that fails at
startup while its upstream stage is still being launched hits that window
reliably. Slot reuse now additionally requires a reclaimed stack, which is the
architectural invariant that exit delivery precedes slot reuse. The QEMU gate
covers the failing-sink case that reproduced it.
