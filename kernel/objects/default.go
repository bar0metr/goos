//go:build !uefi

package objects

// Heap takes entries from the Go heap, for host tests.
type Heap[T any] struct{}

func (Heap[T]) New() (*T, bool) { return new(T), true }
func (Heap[T]) Free(*T)         {}

func defaultStorage[T any]() Storage[T] { return Heap[T]{} }
