package ramfs

// PageSize is the unit file data is stored in.
const PageSize = 4096

// PageRef names one page of a Store. Zero names none: a file page that was
// never written, which reads as zeros.
type PageRef uint32

// Store supplies the memory a filesystem keeps: pages for file data and for
// the indexes that locate them, and chunks of the node table. A store hands
// out only zeroed memory. The filesystem returns every page it no longer
// needs; chunks are kept, since the node table only grows to its bound.
//
// The VFS backs its RAMFS with pages of a reserved region of its address space
// whose released pages go back to the kernel (ADR 0105); the default store,
// used by tests and the kernel's validation self-test, allocates from the Go
// heap and keeps released pages for reuse.
type Store interface {
	// Allocate returns a zeroed page, or false when the store has none.
	Allocate() (PageRef, bool)
	// Page returns the page ref names. ref must have come from Allocate and
	// not been freed.
	Page(ref PageRef) *[PageSize]byte
	// Free gives a page back.
	Free(ref PageRef)
	// Chunk returns a zeroed chunk of the node table, or nil.
	Chunk() *NodeChunk
}

// heapStore is the default Store.
type heapStore struct {
	pages []*[PageSize]byte
	free  []PageRef
}

func (store *heapStore) Allocate() (PageRef, bool) {
	if count := len(store.free); count != 0 {
		ref := store.free[count-1]
		store.free = store.free[:count-1]
		*store.pages[ref-1] = [PageSize]byte{}
		return ref, true
	}
	if uint64(len(store.pages)) >= uint64(^PageRef(0)) {
		return 0, false
	}
	store.pages = append(store.pages, new([PageSize]byte))
	return PageRef(len(store.pages)), true
}

func (store *heapStore) Page(ref PageRef) *[PageSize]byte { return store.pages[ref-1] }

func (store *heapStore) Free(ref PageRef) { store.free = append(store.free, ref) }

func (store *heapStore) Chunk() *NodeChunk { return new(NodeChunk) }
