//go:build goos_userspace

package main

import (
	"goos/internal/logfmt"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/logpersist"
	"goos/userspace/runtime/sys"
)

// kernelRing reads the ring through LogReadSequence. The 13 KiB snapshot
// lives in BSS, and a batch's records borrow from it until the next read.
type kernelRing struct {
	snapshot [kcall.LogReadSize]byte
	batch    sys.LogBatch
}

func (ring *kernelRing) Read(cursor uint64) (logpersist.Batch, bool) {
	batch, status := sys.LogReadSequence(logHandle, cursor, &ring.snapshot)
	if status != kcall.StatusOK {
		return logpersist.Batch{}, false
	}
	ring.batch = batch
	return logpersist.Batch{Oldest: batch.Oldest, Total: batch.Total, First: batch.First, Count: batch.Count}, true
}

func (ring *kernelRing) Record(index int) (logfmt.Record, bool) { return ring.batch.Record(index) }

// vfsFiles is the persister's filesystem: requests over the service's VFS
// session, one append handle at a time.
type vfsFiles struct{ handle vfs.Handle }

func (*vfsFiles) Stat(path []byte) (uint64, bool, bool) {
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	switch {
	case !ok:
		return 0, false, false
	case reply.Status == vfs.StatusNotFound:
		return 0, false, true
	case reply.Status != vfs.StatusOK:
		return 0, false, false
	}
	return reply.Size, true, true
}

func (*vfsFiles) ReadAt(path []byte, offset uint64, target []byte) (int, bool) {
	opened, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if !ok || opened.Status != vfs.StatusOK {
		return 0, false
	}
	size := 0
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationSeek, Handle: opened.Handle, Offset: offset})
	ok = ok && reply.Status == vfs.StatusOK
	for ok && size < len(target) {
		reply, ok = callVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: uint32(min(len(target)-size, vfs.MaxReplyData))})
		ok = ok && reply.Status == vfs.StatusOK
		if !ok || len(reply.Data) == 0 {
			break
		}
		size += copy(target[size:], reply.Data)
	}
	closeFile(opened.Handle)
	return size, ok
}

func (*vfsFiles) Truncate(path []byte, size uint64) bool {
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationTruncate, Path: path, Offset: size})
	return ok && reply.Status == vfs.StatusOK
}

func (*vfsFiles) Replace(from, to []byte) bool {
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationReplace, Path: from, Target: to})
	return ok && reply.Status == vfs.StatusOK
}

// OpenAppend creates the file when absent: storage can arrive after the
// service starts, and the file can be removed from under it.
func (files *vfsFiles) OpenAppend(path []byte) (uint64, bool) {
	created, ok := callVFS(vfs.Request{Operation: vfs.OperationCreate, Path: path})
	if !ok || created.Status != vfs.StatusOK && created.Status != vfs.StatusAlreadyExists {
		return 0, false
	}
	stat, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	if !ok || stat.Status != vfs.StatusOK {
		return 0, false
	}
	opened, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: path})
	if !ok || opened.Status != vfs.StatusOK {
		return 0, false
	}
	if reply, ok := callVFS(vfs.Request{Operation: vfs.OperationSeek, Handle: opened.Handle, Offset: stat.Size}); !ok || reply.Status != vfs.StatusOK {
		closeFile(opened.Handle)
		return 0, false
	}
	files.handle = opened.Handle
	return stat.Size, true
}

// Write sends data in chunks the VFS protocol carries inline; each write
// advances the handle's offset.
func (files *vfsFiles) Write(data []byte) bool {
	for len(data) != 0 {
		chunk := min(len(data), vfs.MaxProtocolData)
		reply, ok := callVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: files.handle, Data: data[:chunk]})
		if !ok || reply.Status != vfs.StatusOK || reply.Size != uint64(chunk) {
			return false
		}
		data = data[chunk:]
	}
	return true
}

func (files *vfsFiles) Close() { closeFile(files.handle) }

func closeFile(file vfs.Handle) {
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: file})
}

func callVFS(request vfs.Request) (vfs.Reply, bool) {
	correlation++
	message, ok := vfs.EncodeRequest(request, correlation)
	if !ok {
		return vfs.Reply{}, false
	}
	size, ok := ipc.Encode(message, wire[:])
	if !ok || sys.IPCSend(vfsHandle, wire[:size]) != 0 {
		return vfs.Reply{}, false
	}
	for attempt := 0; attempt < vfsReplyAttempts; attempt++ {
		status := sys.IPCWaitReceive(vfsHandle, wire[:], vfsReplyDeadline)
		if status == ipc.StatusTimeout {
			continue
		}
		if status != 0 {
			return vfs.Reply{}, false
		}
		payloadSize := int(wire[24]) | int(wire[25])<<8
		received, valid := ipc.Decode(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8])
		if !valid || received.Correlation != correlation {
			return vfs.Reply{}, false
		}
		reply, valid := vfs.DecodeReply(received)
		return reply, valid && reply.Operation == request.Operation
	}
	return vfs.Reply{}, false
}
