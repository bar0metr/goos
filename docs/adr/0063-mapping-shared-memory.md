# ADR 0063: Mapping shared memory

## Status

Accepted.

## Context

Shared-memory objects (ADR 0062) could only be mapped by the kernel itself.
The launch staging page is created and mapped for PID 1 at boot, and no
process can map an object it was granted. The shell and PID 1 need a shared
page for exec requests that do not fit a 256-byte IPC payload, and the shell
receives its handles through a launch manifest, after the kernel has built its
address space.

## Decision

Native operation 30, `MemoryMap(handle, address, flags)`, maps a whole
shared-memory object into the caller's address space. Flag bit 0
(`MemoryMapWritable`) requests a writable mapping; any other bit is rejected
with `InvalidArgument`.

- The caller must be a registered process. The handle must be a
  `TypeMemory` handle with `Read`, and with `Write` when writable. Otherwise
  the call returns `PermissionDenied`.
- The kernel keeps a fixed table of the shared objects it created
  (`shmem.Table`, four entries). An object not in the table cannot be mapped.
- `address` must be page-aligned, and the whole object must lie inside the
  shared window `[vm.UserSharedAddress, vm.UserSharedLimit)`
  (`0x40180000`–`0x401c0000`), between the largest native image and the DMA
  window. A bad address or an occupied leaf returns `InvalidArgument`.
- `shmem.Object.Map` checks every leaf before installing any, so a failed call
  maps nothing. Shared pages are always non-executable.
- Mappings are not owned. `DestroyUserAddressSpace` leaves the pages
  allocated, and the object lives for the lifetime of the kernel. There is no
  unmap operation.

When the kernel creates PID 1 it also creates a one-page exec page and grants
it as handle `0x100000014` with `Read|Write|Transfer`. The kernel does not map
it. PID 1 maps it with `MemoryMap` and passes a derived `Read|Write` handle to
the shell through the launch manifest. The launch staging page is registered in
the same table. ADR 0064 describes the exec protocol that uses the page.

## Alternatives

Mapping the exec page for the shell at launch would need a kernel-side notion
of which process gets which object at which address. Mapping by handle keeps
that decision in userspace and under capability checks.

A call that creates new objects from userspace would need accounting, a
reclaim path, and unmap. Nothing needs it yet, so objects remain kernel-created.

## Consequences

PID 1 has one more fixed handle and one more shared page. Any process holding a
memory handle can map the object once per free address in its shared window;
mappings persist until the process exits. Since mapped pages are never freed,
reusing or revoking a shared object would first need mapping tracking.
