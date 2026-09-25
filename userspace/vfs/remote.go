package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/userspace/fsproto"
)

// Transport carries one request to a filesystem service and brings back the
// reply with the same correlation. It reports false when the service could not
// be reached, did not answer within its deadline, or answered with something
// that does not decode; it never waits without bound.
type Transport interface {
	Exchange(request fsproto.Request) (fsproto.Reply, bool)
}

// Remote is a namespace backend served by another process (ADR 0096). The VFS
// keeps its node IDs in open files and in its mount table; the filesystem
// service owns what they mean. A transport failure is an I/O error on this
// backend's mounts and nothing else: the VFS itself, and every other mount,
// keep working.
type Remote struct {
	transport Transport
	name      [ramfs.MaxName]byte
}

func (remote *Remote) call(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	if remote.transport == nil {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	reply, ok := remote.transport.Exchange(request)
	if !ok || reply.Operation != request.Operation {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	return reply, reply.Status
}

func (remote *Remote) named(operation fsproto.Operation, node, parent ramfs.NodeID, name ramfs.Name) (fsproto.Reply, ramfs.Status) {
	size := name.CopyTo(remote.name[:])
	if size == 0 {
		return fsproto.Reply{}, ramfs.StatusInvalidArgument
	}
	return remote.call(fsproto.Request{Operation: operation, Node: node, Parent: parent, Name: remote.name[:size]})
}

// Root asks the service for the root of the volume it has mounted.
func (remote *Remote) Root() (ramfs.NodeID, ramfs.Status) {
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationMount})
	return reply.Node, status
}

// Flush asks the service to make completed writes durable.
func (remote *Remote) Flush() bool {
	_, status := remote.call(fsproto.Request{Operation: fsproto.OperationFlush})
	return status == ramfs.StatusOK
}

func (remote *Remote) OpenName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	reply, status := remote.named(fsproto.OperationOpen, 0, parent, name)
	return reply.Node, status
}

func (remote *Remote) CreateName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	reply, status := remote.named(fsproto.OperationCreate, 0, parent, name)
	return reply.Node, status
}

func (remote *Remote) MkdirName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	reply, status := remote.named(fsproto.OperationMkdir, 0, parent, name)
	return reply.Node, status
}

func (remote *Remote) RenameName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	_, status := remote.named(fsproto.OperationRename, id, parent, name)
	return status
}

func (remote *Remote) UnlinkName(parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	_, status := remote.named(fsproto.OperationUnlink, 0, parent, name)
	return status
}

// Read fills target in as many exchanges as it needs. A short chunk is the end
// of the file.
func (remote *Remote) Read(id ramfs.NodeID, offset uint64, target []byte) (int, ramfs.Status) {
	total := 0
	for total < len(target) {
		chunk := len(target) - total
		if chunk > fsproto.MaxReplyData {
			chunk = fsproto.MaxReplyData
		}
		reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationRead, Node: id, Offset: offset + uint64(total), Size: uint32(chunk)})
		if status != ramfs.StatusOK {
			return total, status
		}
		if len(reply.Data) > chunk {
			return total, ramfs.StatusIOError
		}
		total += copy(target[total:], reply.Data)
		if len(reply.Data) < chunk {
			break
		}
	}
	return total, ramfs.StatusOK
}

// Write sends data in as many exchanges as it needs. A short write ends it.
func (remote *Remote) Write(id ramfs.NodeID, offset uint64, data []byte) (int, ramfs.Status) {
	total := 0
	for total < len(data) {
		chunk := len(data) - total
		if chunk > fsproto.MaxRequestData {
			chunk = fsproto.MaxRequestData
		}
		reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationWrite, Node: id, Offset: offset + uint64(total), Data: data[total : total+chunk]})
		if status != ramfs.StatusOK {
			return total, status
		}
		if reply.Value > uint64(chunk) {
			return total, ramfs.StatusIOError
		}
		total += int(reply.Value)
		if reply.Value < uint64(chunk) {
			break
		}
	}
	return total, ramfs.StatusOK
}

func (remote *Remote) Truncate(id ramfs.NodeID, size uint64) ramfs.Status {
	_, status := remote.call(fsproto.Request{Operation: fsproto.OperationTruncate, Node: id, Offset: size})
	return status
}

func (remote *Remote) Stat(id ramfs.NodeID) (ramfs.Stat, ramfs.Status) {
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationStat, Node: id})
	if status != ramfs.StatusOK {
		return ramfs.Stat{}, status
	}
	return ramfs.Stat{ID: reply.Node, Kind: reply.Kind, Size: reply.Value}, ramfs.StatusOK
}

func (remote *Remote) ReadDir(parent ramfs.NodeID, cursor int) (ramfs.Entry, int, ramfs.Status) {
	if cursor < 0 || uint64(cursor) > uint64(^uint32(0)) {
		return ramfs.Entry{}, 0, ramfs.StatusInvalidArgument
	}
	reply, status := remote.call(fsproto.Request{Operation: fsproto.OperationReadDir, Parent: parent, Size: uint32(cursor)})
	if status != ramfs.StatusOK {
		return ramfs.Entry{}, 0, status
	}
	var entry ramfs.Entry
	if len(reply.Name) == 0 || len(reply.Name) > len(entry.Name) {
		return ramfs.Entry{}, 0, ramfs.StatusIOError
	}
	entry.Stat = ramfs.Stat{ID: reply.Node, Kind: reply.Kind, Size: reply.Value}
	entry.Size = uint8(copy(entry.Name[:], reply.Name))
	return entry, int(reply.Cursor), ramfs.StatusOK
}
