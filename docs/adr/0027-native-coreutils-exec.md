# ADR 0027: Native core utilities and per-process VFS sessions

## Status

Accepted

## Decision

Keep `cd` inside shell because a child cannot mutate its parent's working
directory. Execute `pwd`, `ls`, and `cat` as isolated native images under
`/system/bin`. The three archive entries are validated USTAR hardlinks to one
bounded multicall image; startup data selects the command and carries the
current directory plus one raw argument.

Shell sends a typed exec request to PID 1 and waits for completion. PID 1
enforces an executable allowlist, creates a fresh channel pair, moves the server
endpoint to VFS through its control protocol, consumes the client endpoint into
the child launch manifest, and grants console write authority. Utilities close
their VFS endpoint before exiting.

Channel endpoint identity is part of the capability object. Native IPC handle
attachments use move semantics and remove `Transfer` from the received handle.
When both endpoints close, the bounded channel registry slot is released.

## Consequences

Utilities cannot consume shell VFS replies, retain session-creation authority,
or delegate their handles. PID 1 remains the only process allowed to create
channel pairs. Startup data is fixed at 128 bytes and the exec protocol remains
bounded by the 256-byte IPC payload.

Exited process registry entries and architecture scheduler slots are reused.
The QEMU test launches 66 utilities in one session, exceeding the unreclaimed
registry and scheduler capacities that existed before this decision.

Address-space pages and guarded kernel-stack pages are not reclaimed yet. Long
sessions therefore remain bounded by physical memory rather than metadata-table
exhaustion; full memory reclamation is the next process-lifecycle iteration.
