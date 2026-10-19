//go:build !goos_userspace

package thread

import "goos/kernel/kcall"

// A host build has no threads.

// Start answers Unsupported on the host.
func Start(fn func(uintptr), argument uintptr) (ID, Status) { return 0, kcall.StatusUnsupported }

// Join answers Unsupported on the host.
func Join(id ID) (uint64, Status) { return 0, kcall.StatusUnsupported }

// Self returns zero on the host.
func Self() ID { return 0 }

// Yield does nothing on the host.
func Yield() {}

func yield() {}
