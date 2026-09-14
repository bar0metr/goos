# Processes and address spaces

GoOS separates resource ownership from execution. A Process owns an address
space, a process-local handle table, metadata, and a bounded collection of Task
identities. A Task is the schedulable CPU context. This avoids treating a
process and a thread as interchangeable kernel objects.

Process IDs and Task IDs reject zero. Each Process currently supports up to 32
Tasks and rejects duplicate ownership. Dynamic process teardown and ID reuse
are deferred until object lifetime and wait semantics are implemented.

## Address-space isolation

Each process receives a distinct PML4 and PDPT. Supervisor-only kernel mappings
are shared, while user mappings use process-owned lower-level tables. The user
bit is present only along the user mapping path; shared kernel leaves remain
supervisor-only even though both address spaces can execute kernel code.

The live isolation test allocates two physical pages, writes different sentinel
bytes, and maps each page at virtual address `0x40000000` in a different process
root. With interrupts disabled, assembly switches CR3 to process A, reads the
byte, switches to process B, reads again, and restores the kernel CR3. Boot
requires both distinct values before emitting `PROCESS_ISOLATION_OK`.

The mapped page is RW+NX and is not executable userspace. A real ring 3 process
requires user code and stack mappings, a TSS/RSP0 path, and validated transition
frames; those are deliberately not claimed by this stage.
