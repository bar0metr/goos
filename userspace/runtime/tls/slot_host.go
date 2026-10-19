//go:build !goos_userspace

package tls

// Slot returns the caller's thread slot. A host build runs as the main
// thread.
func Slot() int { return 0 }
