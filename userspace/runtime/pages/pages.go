// Package pages is a pool of whole pages that outlive the per-request arena
// (ADR 0105). A service that keeps data across requests — the VFS's RAMFS —
// takes pages from it and gives them back; a page given back returns its
// memory to the kernel at once with VMDiscard and reads as zeros when it is
// taken again.
//
// The collector does not scan pool pages (ADR 0201). A Go pointer stored only
// in a page keeps nothing alive, so pages hold plain data and numeric
// references, as the RAMFS node table does, never pointers to heap objects.
//
// The pool is two reservations of anonymous memory: the pages themselves and a
// stack of the numbers of pages given back. Both cost only what is touched,
// and both are bounded by the page quota of the process.
//
// A Pool may be used from several goroutines or threads: its own lock covers
// its counters and its stack of free pages, and is held only for the few
// instructions and the one VMDiscard of a call. The contents of a page belong
// to whoever took it until they give it back.
package pages

import (
	"goos/kernel/kcall"
	"goos/userspace/runtime/lock"
	"goos/userspace/runtime/sys"
	"unsafe"
)

const PageSize = 4096

// Pool hands out pages of one reservation by number, starting at one.
type Pool struct {
	mu          lock.Mutex
	base, pages uint64
	// next is the first page never handed out.
	next uint64
	// free is the stack of page numbers given back, depth entries deep.
	free  uint64
	depth uint64
}

// Init reserves bytes of address space for the pool.
func (pool *Pool) Init(bytes uint64) bool {
	pages := bytes / PageSize
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.base != 0 || pages == 0 || pages > uint64(^uint32(0)) {
		return false
	}
	base, status := sys.VMReserve(pages*PageSize, kcall.VMRead|kcall.VMWrite)
	if status != kcall.StatusOK {
		return false
	}
	free, status := sys.VMReserve((pages*4+PageSize-1)&^(PageSize-1), kcall.VMRead|kcall.VMWrite)
	if status != kcall.StatusOK {
		sys.VMUnmap(base, pages*PageSize)
		return false
	}
	pool.base, pool.pages, pool.next, pool.free = base, pages, 1, free
	return true
}

// Allocate returns the number of a zeroed page, or false when the pool is
// exhausted.
func (pool *Pool) Allocate() (uint32, bool) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.depth != 0 {
		pool.depth--
		return pool.slot(pool.depth)[0], true
	}
	if pool.base == 0 || pool.next > pool.pages {
		return 0, false
	}
	ref := uint32(pool.next)
	pool.next++
	return ref, true
}

// Page returns the memory of a page Allocate handed out. It reads only what
// Init set, so it takes no lock.
func (pool *Pool) Page(ref uint32) *[PageSize]byte {
	// SAFETY: ref names a page inside the reservation Init made, which stays
	// mapped for the life of the process; Allocate is the only source of refs.
	return (*[PageSize]byte)(pointerAt(pool.base + (uint64(ref)-1)*PageSize))
}

// Free gives a page back. Its memory goes back to the kernel now.
func (pool *Pool) Free(ref uint32) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if ref == 0 || uint64(ref) >= pool.next {
		return
	}
	if sys.VMDiscard(pool.base+(uint64(ref)-1)*PageSize, PageSize) != kcall.StatusOK {
		// The page keeps its memory, so it is cleared here: Allocate
		// promises a zeroed page either way.
		clear(pool.Page(ref)[:])
	}
	pool.slot(pool.depth)[0] = ref
	pool.depth++
}

// Used is the number of pages handed out and not given back.
func (pool *Pool) Used() uint64 {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.next - 1 - pool.depth
}

// pointerAt turns an address inside the pool's reservations into a pointer.
// It reuses the word's bits rather than converting, since an address computed
// from a reservation base is not derived from a Go pointer (as in arena).
func pointerAt(address uint64) unsafe.Pointer {
	word := uintptr(address)
	return *(*unsafe.Pointer)(unsafe.Pointer(&word))
}

func (pool *Pool) slot(index uint64) *[1]uint32 {
	// SAFETY: the stack reservation holds four bytes per page of the pool, and
	// its depth never exceeds the pages handed out.
	return (*[1]uint32)(pointerAt(pool.free + index*4))
}

// Permanent returns zeroed memory for a value of type T that is never given
// back, from pages taken from the pool's end of never-used pages. It serves
// long-lived tables that only grow, such as a filesystem's node table.
func Permanent[T any](pool *Pool) *T {
	var zero T
	size := uint64(unsafe.Sizeof(zero))
	count := (size + PageSize - 1) / PageSize
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.base == 0 || count == 0 || pool.next+count-1 > pool.pages {
		return nil
	}
	first := pool.next
	pool.next += count
	// SAFETY: the pages [first, first+count) were never handed out, lie inside
	// the reservation, are zero as fresh anonymous memory, and are never
	// given back, so the value owns them for the life of the process.
	return (*T)(pointerAt(pool.base + (first-1)*PageSize))
}
