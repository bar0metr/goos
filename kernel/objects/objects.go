// Package objects is how a kernel registry gets memory for its entries. A
// registry keeps a fixed table of pointers, sized by its system-wide limit,
// and takes the memory for an entry when the entry is created, returning it
// when the entry goes. Memory in use is then what is live, not the worst case
// the limit allows (ADR 0111).
package objects

// Storage allocates and frees one kind of registry entry. New returns a
// zeroed entry; Free takes back one New returned.
type Storage[T any] interface {
	New() (*T, bool)
	Free(*T)
}

// Or returns storage, or the default when a registry has none installed:
// the Go heap on a host, and nothing in the kernel, whose heap never frees and
// which installs page-backed storage before it creates anything.
func Or[T any](storage Storage[T]) Storage[T] {
	if storage != nil {
		return storage
	}
	return defaultStorage[T]()
}

// Limited wraps a storage with a count of what it has handed out, so a test
// can check that every entry is returned.
type Limited[T any] struct {
	Live  int
	Limit int
}

func (storage *Limited[T]) New() (*T, bool) {
	if storage.Limit != 0 && storage.Live == storage.Limit {
		return nil, false
	}
	storage.Live++
	return new(T), true
}

func (storage *Limited[T]) Free(*T) { storage.Live-- }
