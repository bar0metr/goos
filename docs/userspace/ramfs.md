# RAMFS

RAMFS is an allocation-free filesystem service core backed by 32 fixed node
slots. Each node has a generation-safe opaque ID, a 63-byte component name, and
up to one 4 KiB data page. Slot reuse increments the generation so stale IDs do
not name newly created files.

The service implements create, open, read, write, truncate, mkdir, readdir,
rename, unlink, and stat. Directories refer to parents by node ID. Path parsing,
mount traversal, namespaces, and caches remain VFS responsibilities. RAMFS
therefore never interprets POSIX path strings.

Names reject empty values, slash, NUL, `.` and `..`. A directory cannot move
under its own descendant. Nonempty directories cannot be removed. File writes
and growth are bounded to 4 KiB, and truncation clears discarded bytes so later
growth cannot expose old contents.

Protocol `GRF1` version 1 carries filesystem operations through typed IPC. Its
fixed header contains operation, node IDs, offset, size, and bounded name/data
lengths. Reserved bytes must be zero. Larger data paths will use shared-memory
objects instead of increasing the inline IPC payload.
