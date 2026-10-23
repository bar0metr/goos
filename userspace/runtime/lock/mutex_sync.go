//go:build !goos_userspace || goos_goroutines

package lock

import "sync"

// Mutex is a mutual exclusion lock. The zero value is unlocked.
type Mutex struct {
	m sync.Mutex
}

// Lock acquires the lock, waiting until it is free.
func (l *Mutex) Lock() { l.m.Lock() }

// Unlock releases the lock. Unlocking a Mutex that is not locked is a fault.
func (l *Mutex) Unlock() { l.m.Unlock() }

// TryLock acquires the lock if it is free and reports whether it did.
func (l *Mutex) TryLock() bool { return l.m.TryLock() }
