package linux

import (
	"goos/kernel/objects"
	"goos/kernel/process"
)

// lazyTable keeps one record of type T per process slot. A slot takes its
// record's pages from storage the first time a process needs it and keeps
// them for the processes that later reuse the slot. Kernel code holds
// pointers to a live record past the lock it found it under, as the per-task
// caller cache does, so a record's pages never go back to the allocator: a
// pointer that outlived its process reads a reset record, never a page
// another owner took. A slot nobody used costs one pointer.
type lazyTable[T any] struct {
	entries [process.MaxProcesses]*T
	storage objects.Storage[T]
}

// at returns the record of slot index, or nil when the slot never had one.
func (table *lazyTable[T]) at(index int) *T { return table.entries[index] }

// claim returns the record of slot index, taking its pages first when the
// slot has none. It fails only when storage cannot supply them.
func (table *lazyTable[T]) claim(index int) (*T, bool) {
	if entry := table.entries[index]; entry != nil {
		return entry, true
	}
	entry, ok := objects.Or(table.storage).New()
	if !ok || entry == nil {
		return nil, false
	}
	table.entries[index] = entry
	return entry, true
}

// allocated counts the slots that hold pages.
func (table *lazyTable[T]) allocated() int {
	count := 0
	for index := range table.entries {
		if table.entries[index] != nil {
			count++
		}
	}
	return count
}
