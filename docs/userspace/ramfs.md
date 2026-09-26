# RAMFS

RAMFS is the volatile filesystem the VFS serves `/` and `/tmp` from. It keeps
files in pages rather than fixed buffers (ADR 0105).

## Storage

A file's bytes live in 4 KiB pages located through its node the way a classic
inode locates blocks: twelve direct page references, one page of 1024
references, and one page of 1024 pages of references — a little over 4 GiB per
file. A page is allocated only when bytes are written to it. A page never
written is a hole that reads as zeros, so a write far past the end of a file,
or a truncation that grows it, costs nothing until the gap is written.
Truncation frees every page past the new end, and the index pages that then
index nothing, and zeroes the tail of the last kept page, so later growth never
exposes old bytes. Removal frees all of a file's pages.

Pages come from a `ramfs.Store`. The VFS backs it with a pool of pages in a
reserved region of its own address space (`userspace/runtime/pages`); a page
given back is returned to the kernel at once with `VMDiscard` (ADR 0104), so
memory used by `/tmp` falls when files shrink or go. Tests and the kernel's
validation self-test use a default store on the Go heap.

## Nodes

The node table grows 64 nodes at a time up to the instance's node limit. Node
IDs are generation-safe: slot reuse increments the generation so stale IDs do
not name newly created files. A directory's children are linked in slot order,
so lookup costs the directory's size, and a `ReadDir` cursor — the slot after
the last entry returned — stays meaningful when entries are removed.

## Limits

Each instance has a node limit and a page limit, set when it is configured; a
create past the node limit or a write that would need pages past the page limit
is refused whole with "no space left on device". The VFS configures 8192 nodes
and 16 MiB of pages, within its own page quota, which bounds it in the end.

## Semantics

The service implements create, open, read, write, truncate, mkdir, readdir,
rename, unlink, and stat. Directories refer to parents by node ID. Path parsing,
mount traversal, namespaces, and caches remain VFS responsibilities. RAMFS
therefore never interprets POSIX path strings.

Names reject empty values, slash, NUL, `.` and `..`. A directory cannot move
under its own descendant. Nonempty directories cannot be removed.

Protocol `GRF1` version 1 carries filesystem operations through typed IPC. Its
fixed header contains operation, node IDs, offset, size, and bounded name/data
lengths. Reserved bytes must be zero.
