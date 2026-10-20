// Package pathpage carries VFS paths and returned names too long for inline
// messages (ADR 0110). Hold covers Prepare, the entire exchange, and copying
// Name. If a sent exchange is abandoned, call Abandon before Release: a late
// server access must never reach the page of another request.
package pathpage

import (
	"goos/internal/capability"
	"goos/internal/limits"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/runtime/lock"
	"goos/userspace/runtime/sys"
)

const pages = 2 * limits.PathMax / 4096

type memoryTransport interface {
	create() (handle.ID, uint64, []byte, bool)
	duplicate(handle.ID, capability.Rights) (handle.ID, bool)
	retire(handle.ID, uint64, uint64)
}

// pageState's memory is reusable only after a completed exchange. Retiring
// the local mapping does not revoke a server mapping; the old object stays
// distinct and cannot affect the fresh object allocated by the next Prepare.
type pageState[T memoryTransport] struct {
	mu     lock.Mutex
	object handle.ID
	base   uint64
	memory []byte
	io     T
}

var page pageState[nativeMemory]

func Hold()    { page.mu.Lock() }
func Release() { page.mu.Unlock() }

// Prepare attaches the paths or space for a returned name. The caller holds
// the page, and closes the returned handle itself if the send fails.
func Prepare(request *vfs.Request) (handle.ID, bool) { return page.prepare(request) }

// Name aliases the page until Release. Copy it before releasing the page.
func Name(size int) []byte { return page.name(size) }

// Abandon permanently detaches this object from future requests. The caller
// holds the page and must not subsequently access its previous Name slice.
func Abandon() { page.abandon() }

func (p *pageState[T]) prepare(request *vfs.Request) (handle.ID, bool) {
	inline := vfs.FitsInline(*request)
	if inline && !request.NameReturn {
		return 0, true
	}
	if len(request.Path) > limits.PathMax || len(request.Target) > limits.PathMax || !p.ensure() {
		return 0, false
	}
	rights := capability.Read | capability.Transfer
	if request.NameReturn {
		rights |= capability.Write
	}
	if !inline {
		copy(p.memory, request.Path)
		copy(p.memory[len(request.Path):], request.Target)
		request.PathsAttached = true
	}
	return p.io.duplicate(p.object, rights)
}

func (p *pageState[T]) name(size int) []byte {
	if size < 0 || size > len(p.memory) {
		return nil
	}
	return p.memory[:size]
}

func (p *pageState[T]) ensure() bool {
	if p.object != 0 {
		return true
	}
	object, base, memory, ok := p.io.create()
	if !ok {
		return false
	}
	p.object, p.base, p.memory = object, base, memory
	return true
}

func (p *pageState[T]) abandon() {
	object, base, size := p.object, p.base, uint64(len(p.memory))
	p.object, p.base, p.memory = 0, 0, nil
	if object != 0 {
		p.io.retire(object, base, size)
	}
}

type nativeMemory struct{}

func (nativeMemory) create() (handle.ID, uint64, []byte, bool) {
	object, status := sys.MemoryCreate(pages)
	if status != kcall.StatusOK {
		return 0, 0, nil, false
	}
	base, size, status := sys.MemoryMapAnywhere(object, true)
	if status != kcall.StatusOK {
		sys.HandleClose(object)
		return 0, 0, nil, false
	}
	if size < 2*limits.PathMax {
		sys.VMUnmap(base, size)
		sys.HandleClose(object)
		return 0, 0, nil, false
	}
	return object, base, sys.Window(base, size), true
}

func (nativeMemory) duplicate(object handle.ID, rights capability.Rights) (handle.ID, bool) {
	attached, status := sys.HandleDuplicate(object, rights)
	return attached, status == kcall.StatusOK
}

func (nativeMemory) retire(object handle.ID, base, size uint64) {
	sys.VMUnmap(base, size)
	sys.HandleClose(object)
}
