//go:build uefi

package objects

// none refuses every entry: a registry used before the kernel installed its
// storage fails rather than touching the heap.
type none[T any] struct{}

func (none[T]) New() (*T, bool) { return nil, false }
func (none[T]) Free(*T)         {}

func defaultStorage[T any]() Storage[T] { return none[T]{} }
