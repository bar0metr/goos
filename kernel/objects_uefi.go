//go:build uefi

package main

import (
	"sync/atomic"

	"goos/kernel/mm/vm"
)

// pageObjects keeps a registry's entries in pages from the page allocator,
// taken when an entry is created and returned when it is released (ADR 0111).
type pageObjects[T any] struct{}

func (pageObjects[T]) New() (*T, bool) { return vm.NewObject[T](&pageAllocator) }

func (pageObjects[T]) Free(object *T) {
	if !vm.FreeObject(&pageAllocator, object) {
		objectFreeFailures.Add(1)
	}
}

// objectFreeFailures counts registry entries whose pages the allocator
// refused to take back: an accounting bug, reported by the diagnostics.
var objectFreeFailures atomic.Uint64
