package utility

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/vfs"
)

type bulkTransport interface {
	create(uint64) (handle.ID, uint64, []byte, bool)
	duplicate(handle.ID, capability.Rights) (handle.ID, bool)
	retire(handle.ID, uint64, uint64)
	call(vfs.Request, handle.ID) (vfs.Reply, bool)
}

// bulkBuffer separates caller-owned bytes from server-visible staging. In
// particular, returning false cannot leave a late ReadInto writing Memory or
// a late WriteFrom reading it while the caller prepares the next transfer.
// The owner must not copy a used buffer or use it from concurrent callers.
type bulkBuffer[T bulkTransport] struct {
	Memory  []byte
	object  handle.ID
	base    uint64
	staging []byte
	io      T
}

func (b *bulkBuffer[T]) ensure(size uint64) bool {
	if b.object != 0 {
		return true
	}
	object, base, memory, ok := b.io.create(size)
	if !ok {
		return false
	}
	b.object, b.base, b.staging = object, base, memory
	return true
}

func (b *bulkBuffer[T]) transfer(operation vfs.Operation, file vfs.Handle, count int) (vfs.Reply, bool) {
	if count < 0 || count > len(b.Memory) || uint64(count) > uint64(^uint32(0)) || !b.ensure(uint64(len(b.Memory))) || count > len(b.staging) {
		return vfs.Reply{}, false
	}
	rights := capability.Read | capability.Transfer
	if operation == vfs.OperationReadInto {
		rights |= capability.Write
	} else {
		copy(b.staging[:count], b.Memory[:count])
	}
	attached, ok := b.io.duplicate(b.object, rights)
	if !ok {
		return vfs.Reply{}, false
	}
	reply, ok := b.io.call(vfs.Request{Operation: operation, Handle: file, Size: uint32(count)}, attached)
	if !ok {
		// The server may retain its mapping indefinitely. Drop our mapping and
		// handle, never reuse the old object, and leave Memory unchanged.
		b.io.retire(b.object, b.base, uint64(len(b.staging)))
		b.object, b.base, b.staging = 0, 0, nil
		return vfs.Reply{}, false
	}
	if operation == vfs.OperationReadInto && reply.Status == vfs.StatusOK {
		copy(b.Memory[:int(reply.Size)], b.staging[:int(reply.Size)])
	}
	return reply, true
}
