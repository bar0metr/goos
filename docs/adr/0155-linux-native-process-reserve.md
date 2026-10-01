# ADR 0155: Linux processes cannot consume the native process reserve

## Status

Accepted.

## Context

Every Linux process is an ordinary GoOS Process and takes one of the 64
process and scheduler slots that PID 1, the VFS, the terminal, and native
programs also need. Per-process page and task quotas do not stop many Linux
processes from filling the registry. Two paths create Linux processes: PID 1
launching a Linux image, and a Linux program calling `fork`, `vfork`, or a
process form of `clone`. A limit on one path alone leaves the other open.

The Linux family registry keeps zombie records until they are reaped. A limit
that counted only running processes would let unreaped children hold every
compatibility PID.

## Decision

No Linux process-creation path may reduce the number of free native process
slots below `NativeProcessReserve`.

This applies both to PID 1 Linux launches and to compatibility-created
children from fork, vfork, and supported process-form clone.

Three fixed bounds follow:

- `NativeProcessReserve` is 16;
- `LinuxGlobalMaxProcesses` is 48, the 64-entry registry minus the reserve;
- `LinuxFamilyMaxProcesses` is 16, a separate bound for one family.

One function, `LinuxProcessCreationAllowed`, decides the global rule from the
native registry's process count, native and Linux alike. Every Linux creation
path asks it, and native creation never does. A PID 1 launch asks once it has
classified the image as Linux and before it builds anything, and a refusal is
`LaunchResourceExhausted`. Child registration asks it, then checks that the
compatibility records stay below 48, and then the family limit. Running and
zombie records count against both compatibility limits. A refused fork-like
call is `EAGAIN`; a failure later in the copy is `ENOMEM`, and the caller rolls
back the reserved record.

## Consequences

Linux processes cannot take the last 16 process slots, however they are
started. No family holds more than 16 running and zombie identities, so one
tree cannot use the whole Linux allowance. Reaping a zombie returns one family
and one global slot. The bounds are compile-time constants tied to the
registry capacity, not settings a user can change.
