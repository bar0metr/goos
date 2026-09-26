//go:build goos_userspace

// Package pathpage carries a VFS request's paths when they are too long for
// the message, and receives a listed name too long for the reply (ADR 0110).
//
// A process that needs it creates one shared-memory object of two pages, a
// path and a target of PathMax bytes each, and keeps it mapped. Each request
// that uses it attaches a new handle to it, with only the rights the VFS
// needs; the VFS copies the paths out before it looks at them.
package pathpage

import (
	"goos/internal/capability"
	"goos/internal/limits"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/runtime/sys"
)

const pages = 2 * limits.PathMax / 4096

var (
	object handle.ID
	memory []byte
)

// Prepare readies request to be sent. When its paths fit the message and it
// asks for no name, there is nothing to attach and the handle is zero.
// Otherwise the paths are written to the page, the request is marked, and the
// handle returned is to be attached; it moves to the VFS with the request.
func Prepare(request *vfs.Request) (handle.ID, bool) {
	inline := vfs.FitsInline(*request)
	if inline && !request.NameReturn {
		return 0, true
	}
	if len(request.Path)+len(request.Target) > 2*limits.PathMax || !ensure() {
		return 0, false
	}
	rights := capability.Read | capability.Transfer
	if request.NameReturn {
		rights |= capability.Write
	}
	if !inline {
		copy(memory, request.Path)
		copy(memory[len(request.Path):], request.Target)
		request.PathsAttached = true
	}
	attached, status := sys.HandleDuplicate(object, rights)
	if status != kcall.StatusOK {
		return 0, false
	}
	return attached, true
}

// Name returns the listed name the VFS wrote to the page, size bytes long.
func Name(size int) []byte {
	if size > len(memory) {
		return nil
	}
	return memory[:size]
}

func ensure() bool {
	if object != 0 {
		return true
	}
	created, status := sys.MemoryCreate(pages)
	if status != kcall.StatusOK {
		return false
	}
	base, size, status := sys.MemoryMapAnywhere(created, true)
	if status != kcall.StatusOK {
		sys.HandleClose(created)
		return false
	}
	object, memory = created, sys.MappedAtStart(base, size)
	return true
}
