# ADR 0155: Linux descendants have bounded process budgets

## Status

Accepted.

## Context

Linux descendants will allocate ordinary GoOS process and scheduler slots.
Those registries contain 64 entries and also serve PID 1, VFS, the terminal,
and native programs. Per-process page and task quotas do not stop a process
tree from exhausting the registries.

The Linux family registry retains zombie records until reap. A limit that
counts only running processes would let unreaped children consume every
compatibility PID and family slot.

## Decision

The compatibility layer applies two fixed limits before it reserves a child:

- `LinuxFamilyMaxProcesses` is 16;
- `LinuxGlobalMaxProcesses` is 48;
- `NativeProcessReserve` is 16.

The global limit is the 64-entry native process capacity minus the native
reserve. The family limit prevents one process tree from consuming the whole
compatibility allowance. Running and zombie records count against both limits.

Child registration also receives the current native process count. It refuses
creation once that count reaches 48, even when compatibility records remain.
The process-creation syscall maps a budget refusal to `EAGAIN`. Allocation or
page-copy failure remains `ENOMEM`. A caller must roll back a reserved child
record if later preparation fails.

## Consequences

Linux descendants cannot consume the last 16 process slots. No family can
hold more than 16 running and zombie identities. Reaping a zombie returns one
family and global slot. The limits are compile-time constants tied to the
native registry capacity, not user-controlled resource settings.
