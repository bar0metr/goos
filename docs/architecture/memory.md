# Physical memory

Early boot allocates a fixed 256 KiB UEFI memory-map buffer, captures the final
map, normalizes firmware types into architecture-neutral regions, and calls
`ExitBootServices` with the matching map key. A stale key is retried without
allocating between map capture and exit. After success, firmware boot services
must never be called again.

The first physical allocator uses a fixed bitmap and manages physical addresses
below 1 GiB. This covers the supported 512 MiB QEMU configuration without
requiring dynamic metadata during allocator bootstrap. Page zero through the
first MiB stays reserved. Only UEFI conventional memory is initially free;
loader, runtime, MMIO, ACPI, and firmware regions stay reserved. Boot-services
memory is tagged reclaimable but remains reserved until later code proves no
firmware-owned structures reference it.

Allocation and release operate on 4 KiB pages. Unaligned, unmanaged, low-memory,
and double-free requests are rejected. The boot path allocates and releases one
page as a live self-test before emitting `PMM_OK`.
