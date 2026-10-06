package shmem

import "goos/kernel/lock"

// guard is the lock the kernel installs to guard the shared memory table and its objects.
var guard lock.Guard

// Guard installs the lock that guards the shared memory table and its objects.
func Guard(spin *lock.Spin) { guard.Set(spin) }
