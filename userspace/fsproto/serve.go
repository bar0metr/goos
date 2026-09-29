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

type objectVolume interface {
	AcquireObject(ramfs.NodeID) (ramfs.NodeID, ramfs.Status)
	ReleaseObject(ramfs.NodeID) (ramfs.NodeID, ramfs.Status)
}

type filesystemVolume interface {
	FilesystemInfo() (ramfs.FilesystemInfo, ramfs.Status)
}

type replaceVolume interface {
	ReplaceName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status
}

// Serve performs one decoded request on volume. A read fills, and a write
// takes its bytes from, the request's slot of cache, the file cache mapping;
// a listed name is placed in name, which the reply aliases until the caller
// encodes it.
func Serve(volume Volume, request Request, cache []byte, name *[ramfs.MaxName]byte) Reply {
	reply := Reply{Operation: request.Operation}
	requested, named := ramfs.MakeName(request.Name)
	switch request.Operation {
	case OperationMount:
		reply.Node, reply.Status = volume.Root()
	case OperationOpen, OperationCreate, OperationMkdir, OperationRename, OperationReplace, OperationUnlink:
		if !named {
			reply.Status = ramfs.StatusInvalidArgument
			break
		}
		switch request.Operation {
		case OperationReplace:
			if replacer, ok := volume.(replaceVolume); ok {
				reply.Status = replacer.ReplaceName(request.Node, request.Parent, requested)
			} else {
				reply.Status = ramfs.StatusUnsupported
			}
		case OperationOpen:
			reply.Node, reply.Status = volume.OpenName(request.Parent, requested)
		case OperationCreate:
			reply.Node, reply.Status = volume.CreateName(request.Parent, requested)
		case OperationMkdir:
			reply.Node, reply.Status = volume.MkdirName(request.Parent, requested)
		case OperationRename:
			reply.Status = volume.RenameName(request.Node, request.Parent, requested)
		default:
			reply.Status = volume.UnlinkName(request.Parent, requested)
		}
		// A node that was looked up or made comes back with its attributes,
		// which the VFS asks for next; answering both in one exchange saves
		// it a round trip (ADR 0096).
		if reply.Status == ramfs.StatusOK && reply.Node != 0 {
			if stat, status := volume.Stat(reply.Node); status == ramfs.StatusOK && stat.ID == reply.Node {
				reply.setStat(stat)
			}
		}
	case OperationRead, OperationWrite:
		bytes, mapped := SlotBytes(cache, request)
		if !mapped {
			// No cache is mapped, or the request names none of it.
			reply.Status = ramfs.StatusIOError
			break
		}
		var count int
		if request.Operation == OperationRead {
			count, reply.Status = volume.Read(request.Node, request.Offset, bytes)
		} else {
			count, reply.Status = volume.Write(request.Node, request.Offset, bytes)
		}
		if count < 0 || count > len(bytes) {
			count, reply.Status = 0, ramfs.StatusIOError
		}
		reply.Value = uint64(count)
	case OperationTruncate:
		reply.Status = volume.Truncate(request.Node, request.Offset)
	case OperationAcquire:
		objects, ok := volume.(objectVolume)
		if !ok {
			reply.Status = ramfs.StatusUnsupported
			break
		}
		reply.Node, reply.Status = objects.AcquireObject(request.Node)
	case OperationRelease:
		objects, ok := volume.(objectVolume)
		if !ok {
			reply.Status = ramfs.StatusUnsupported
			break
		}
		reply.Node, reply.Status = objects.ReleaseObject(request.Node)
	case OperationStat:
		stat, status := volume.Stat(request.Node)
		reply.Status = status
		reply.setStat(stat)
	case OperationReadDir:
		entry, cursor, status := volume.ReadDir(request.Parent, int(request.Size))
		reply.Status = status
		if status == ramfs.StatusOK {
			if cursor < 0 || uint64(cursor) > uint64(^uint32(0)) || int(entry.Size) > len(entry.Name) {
				reply.Status = ramfs.StatusIOError
				break
			}
			reply.setStat(entry.Stat)
			reply.Cursor = uint32(cursor)
			// The name is copied out: entry is a local, and the reply must
			// not alias memory that is gone once Serve returns.
			reply.Name = name[:copy(name[:], entry.Name[:entry.Size])]
		}
	case OperationFlush:
		if !volume.Flush() {
			reply.Status = ramfs.StatusIOError
		}
	case OperationFilesystemInfo:
		filesystem, ok := volume.(filesystemVolume)
		if !ok {
			reply.Status = ramfs.StatusUnsupported
			break
		}
		reply.Filesystem, reply.Status = filesystem.FilesystemInfo()
		if reply.Status == ramfs.StatusOK {
			if !reply.Filesystem.Valid() {
				reply.Filesystem = ramfs.FilesystemInfo{}
				reply.Status = ramfs.StatusIOError
				break
			}
			reply.Name = name[:copy(name[:], reply.Filesystem.Name())]
		}
	default:
		reply.Status = ramfs.StatusUnsupported
	}
	return reply
}

func (reply *Reply) setStat(stat ramfs.Stat) {
	reply.Node, reply.Kind, reply.Value = stat.ID, stat.Kind, stat.Size
	reply.Created, reply.Modified = stat.Created, stat.Modified
	reply.Attributes, reply.AttributesAvailable = stat.Attributes, stat.AttributesAvailable
}
