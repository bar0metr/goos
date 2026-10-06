package stream

import "goos/kernel/lock"

// guard is the lock the kernel installs to guard the stream registry and every stream.
var guard lock.Guard

// Guard installs the lock that guards the stream registry and every stream.
func Guard(spin *lock.Spin) { guard.Set(spin) }
