//go:build goos_userspace

package sys

import "unsafe"

// window turns a range the kernel has just mapped into the caller into a
// slice. It is the only place userspace makes memory out of an address.
//
// SAFETY: callers pass a range the kernel reported as mapped into this
// address space, readable, and of exactly size bytes. Mappings made through
// these calls are never removed while the process runs (ADR 0063, 0070), so
// the slice stays valid for the life of the process.
func window(base uint64, size uint64) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(base))), int(size))
}
