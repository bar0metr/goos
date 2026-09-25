package fsproto

import "goos/kernel/ramfs"

// Volume is what a filesystem service serves: the backend operations the VFS
// mounts, plus the root of the mounted volume and a flush of its device.
type Volume interface {
	Root() (ramfs.NodeID, ramfs.Status)
	OpenName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status)
	CreateName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status)
	MkdirName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status)
	Read(id ramfs.NodeID, offset uint64, target []byte) (int, ramfs.Status)
	Write(id ramfs.NodeID, offset uint64, data []byte) (int, ramfs.Status)
	Truncate(id ramfs.NodeID, size uint64) ramfs.Status
	Stat(id ramfs.NodeID) (ramfs.Stat, ramfs.Status)
	ReadDir(parent ramfs.NodeID, cursor int) (ramfs.Entry, int, ramfs.Status)
	RenameName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status
	UnlinkName(parent ramfs.NodeID, name ramfs.Name) ramfs.Status
	Flush() bool
}

// Serve performs one decoded request on volume. Read data is placed in
// scratch, which the reply aliases until the caller encodes it.
func Serve(volume Volume, request Request, scratch *[MaxReplyData]byte) Reply {
	reply := Reply{Operation: request.Operation}
	name, named := ramfs.MakeName(request.Name)
	switch request.Operation {
	case OperationMount:
		reply.Node, reply.Status = volume.Root()
	case OperationOpen, OperationCreate, OperationMkdir, OperationRename, OperationUnlink:
		if !named {
			reply.Status = ramfs.StatusInvalidArgument
			break
		}
		switch request.Operation {
		case OperationOpen:
			reply.Node, reply.Status = volume.OpenName(request.Parent, name)
		case OperationCreate:
			reply.Node, reply.Status = volume.CreateName(request.Parent, name)
		case OperationMkdir:
			reply.Node, reply.Status = volume.MkdirName(request.Parent, name)
		case OperationRename:
			reply.Status = volume.RenameName(request.Node, request.Parent, name)
		default:
			reply.Status = volume.UnlinkName(request.Parent, name)
		}
		// A node that was looked up or made comes back with its attributes,
		// which the VFS asks for next; answering both in one exchange saves
		// it a round trip (ADR 0096).
		if reply.Status == ramfs.StatusOK && reply.Node != 0 {
			if stat, status := volume.Stat(reply.Node); status == ramfs.StatusOK && stat.ID == reply.Node {
				reply.Kind, reply.Value = stat.Kind, stat.Size
			}
		}
	case OperationRead:
		count, status := volume.Read(request.Node, request.Offset, scratch[:request.Size])
		if count < 0 || count > int(request.Size) {
			count, status = 0, ramfs.StatusIOError
		}
		reply.Status, reply.Data = status, scratch[:count]
	case OperationWrite:
		count, status := volume.Write(request.Node, request.Offset, request.Data)
		reply.Status, reply.Value = status, uint64(count)
	case OperationTruncate:
		reply.Status = volume.Truncate(request.Node, request.Offset)
	case OperationStat:
		stat, status := volume.Stat(request.Node)
		reply.Status, reply.Node, reply.Kind, reply.Value = status, stat.ID, stat.Kind, stat.Size
	case OperationReadDir:
		entry, cursor, status := volume.ReadDir(request.Parent, int(request.Size))
		reply.Status = status
		if status == ramfs.StatusOK {
			if cursor < 0 || uint64(cursor) > uint64(^uint32(0)) || int(entry.Size) > len(entry.Name) {
				reply.Status = ramfs.StatusIOError
				break
			}
			reply.Node, reply.Kind, reply.Value, reply.Cursor = entry.Stat.ID, entry.Stat.Kind, entry.Stat.Size, uint32(cursor)
			// The name is copied into scratch: entry is a local, and the
			// reply must not alias memory that is gone once Serve returns.
			reply.Name = scratch[:copy(scratch[:], entry.Name[:entry.Size])]
		}
	case OperationFlush:
		if !volume.Flush() {
			reply.Status = ramfs.StatusIOError
		}
	default:
		reply.Status = ramfs.StatusInvalidArgument
	}
	return reply
}
