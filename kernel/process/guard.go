package process

import "goos/kernel/lock"

// guard is the lock the kernel installs to guard the process registry.
var guard lock.Guard

// Guard installs the lock that guards the process registry.
func Guard(rw *lock.RW) { guard.SetRW(rw) }
