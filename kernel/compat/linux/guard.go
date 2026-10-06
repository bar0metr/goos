package linux

import "goos/kernel/lock"

// tablesLock guards the personality's shared tables: process states and
// records, threads, open descriptions, directories, VFS sessions, and the
// PIDs and family IDs they hand out. Every exported function that touches
// them takes it for its own duration and no longer (ADR 0187). What a
// family's processes share beyond the tables, the fields of their states and
// descriptions, the kernel guards with the family's lock.
var tablesLock = lock.New(lock.LevelLinuxTables)

// guard checks, in validation builds, that the tables are held where they
// are used.
var guard lock.Guard

func init() { guard.Set(tablesLock) }
