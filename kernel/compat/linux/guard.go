package linux

import "goos/kernel/lock"

// guard is the lock the kernel installs to guard the Linux personality's
// tables: process states, records, threads, open descriptions, directories,
// and VFS sessions.
var guard lock.Guard

// Guard installs the lock that guards the Linux personality's tables.
func Guard(spin *lock.Spin) { guard.Set(spin) }
