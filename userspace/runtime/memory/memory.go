// Package memory holds the raw address operations that copy checks, their
// tests and validation probes need, so those callers stay outside the unsafe
// boundary (ADR 0095).
package memory

import "unsafe"

// SliceAddress returns the address of data's array, zero for a nil array.
func SliceAddress(data []byte) uintptr { return uintptr(unsafe.Pointer(unsafe.SliceData(data))) }

// Bytes returns a slice of length bytes at address. Tests use it to build
// the torn headers safe Go cannot produce; length must not exceed what is
// mapped there.
func Bytes(address uintptr, length int) []byte {
	header := struct {
		data     unsafe.Pointer
		len, cap int
	}{*(*unsafe.Pointer)(unsafe.Pointer(&address)), length, length}
	return *(*[]byte)(unsafe.Pointer(&header))
}

// Word reads the 8-byte word at address, which must be mapped. A probe uses
// it to read heap memory a collection may already have freed.
//
//go:noinline
func Word(address uintptr) uint64 {
	return *(*uint64)(*(*unsafe.Pointer)(unsafe.Pointer(&address)))
}
