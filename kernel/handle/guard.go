package handle

import "goos/kernel/lock"

// guard is the lock the kernel installs to guard every handle table.
var guard lock.Guard

// Guard installs the lock that guards every handle table.
func Guard(rw *lock.RW) { guard.SetRW(rw) }
