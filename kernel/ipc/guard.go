package ipc

import "goos/kernel/lock"

// guard is the lock the kernel installs to guard the channel registry and every channel.
var guard lock.Guard

// Guard installs the lock that guards the channel registry and every channel.
func Guard(spin *lock.Spin) { guard.Set(spin) }
