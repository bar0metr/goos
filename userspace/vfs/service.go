// Package vfsservice implements the bounded userspace VFS request dispatcher.
package vfsservice

import (
	"goos/kernel/initramfs"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fat32"
)

type Service struct {
	fs         ramfs.FileSystem
	namespace  vfs.Namespace
	readData   [vfs.MaxReplyData]byte
	persistent fat32.FileSystem
	image      initramfs.Archive
}

// MountSystemImage mounts the boot image read-only at /system. The executables
// the launch path resolves under /system/bin therefore appear in the namespace
// under exactly those names, instead of the search root being a path that
// nothing can list. The archive is untrusted input, so a malformed image is
// refused here rather than at a later lookup.
func (service *Service) MountSystemImage(archive []byte) bool {
	if !service.image.Init(archive) {
		return false
	}
	// The archive stores its members under a system/ prefix, which is what the
	// launch path resolves, so the mount root is that directory rather than the
	// archive root. The two then name the same bytes by the same path.
	name, valid := ramfs.MakeName([]byte(systemImageDirectory))
	if !valid {
		return false
	}
	root, status := service.image.OpenName(service.image.Root(), name)
	if status != ramfs.StatusOK {
		return false
	}
	return service.namespace.Mount([]byte("/"+systemImageDirectory), &service.image, root) == vfs.StatusOK
}

// systemImageDirectory is the archive directory mounted as /system.
const systemImageDirectory = "system"

// persistentMounts names the directories of the persistent volume that get
// their own place in the namespace. User data and mutable system state are
// separate entries so the layout distinguishes them from the immutable system
// image, as the architecture requires, without needing a second device. The
// whole volume stays reachable at /persist; the namespace refuses to remove a
// directory another mount points at, so the two names cannot diverge.
var persistentMounts = [...]struct{ path, directory string }{
	{"/home", "home"},
	{"/state", "state"},
}

func (service *Service) MountPersistent(device fat32.Device) bool {
	root, ok := service.persistent.Mount(device)
	if !ok || service.namespace.Mount([]byte("/persist"), &service.persistent, root) != vfs.StatusOK {
		return false
	}
	for _, mount := range persistentMounts {
		node, ok := service.persistentDirectory(root, mount.directory)
		if !ok || service.namespace.Mount([]byte(mount.path), &service.persistent, node) != vfs.StatusOK {
			return false
		}
	}
	return true
}

// persistentDirectory returns the volume directory of the given name, creating
// it on a volume that does not have one yet. A name already taken by a regular
// file is an error rather than something to overwrite.
func (service *Service) persistentDirectory(root ramfs.NodeID, name string) (ramfs.NodeID, bool) {
	value, valid := ramfs.MakeName([]byte(name))
	if !valid {
		return 0, false
	}
	node, status := service.persistent.OpenName(root, value)
	if status == ramfs.StatusNotFound {
		node, status = service.persistent.MkdirName(root, value)
	}
	if status != ramfs.StatusOK {
		return 0, false
	}
	stat, status := service.persistent.Stat(node)
	if status != ramfs.StatusOK || stat.Kind != ramfs.KindDirectory {
		return 0, false
	}
	return node, true
}

// Init builds the volatile root. /system is not created here: it is the mount
// point of the immutable boot image, and the namespace lists a mount in its
// parent directory without needing a placeholder to stand in for it.
func (service *Service) Init() bool {
	root := service.fs.Init()
	if _, status := service.fs.Mkdir(root, []byte("tmp")); status != ramfs.StatusOK {
		return false
	}
	return service.namespace.Mount([]byte("/"), &service.fs, root) == vfs.StatusOK
}

func (service *Service) Dispatch(request vfs.Request) vfs.Reply {
	reply := vfs.Reply{Operation: request.Operation}
	switch request.Operation {
	case vfs.OperationOpen:
		reply.Handle, reply.Status = service.namespace.Open(request.Path, request.Rights)
	case vfs.OperationClose:
		reply.Status = service.namespace.Close(request.Handle)
	case vfs.OperationRead:
		size := int(request.Size)
		if size > len(service.readData) {
			size = len(service.readData)
		}
		count, status := service.namespace.Read(request.Handle, service.readData[:size])
		reply.Status, reply.Data, reply.Size = status, service.readData[:count], uint64(count)
	case vfs.OperationWrite:
		count, status := service.namespace.Write(request.Handle, request.Data)
		reply.Status, reply.Size = status, uint64(count)
	case vfs.OperationSeek:
		reply.Status = service.namespace.Seek(request.Handle, request.Offset)
	case vfs.OperationCreate, vfs.OperationMkdir:
		reply.Status = service.namespace.Create(request.Path, request.Operation == vfs.OperationMkdir)
	case vfs.OperationReadDir:
		entry, cursor, status := service.namespace.ReadDir(request.Path, int(request.Offset))
		reply.Status, reply.Cursor = status, uint32(cursor)
		if status == vfs.StatusOK {
			reply.Kind, reply.Size, reply.Name = entry.Stat.Kind, entry.Stat.Size, entry.Name[:entry.Size]
		}
	case vfs.OperationRename:
		reply.Status = service.namespace.Rename(request.Path, request.Data)
	case vfs.OperationUnlink:
		reply.Status = service.namespace.Unlink(request.Path)
	case vfs.OperationStat:
		stat, status := service.namespace.Stat(request.Path)
		reply.Status, reply.Kind, reply.Size = status, stat.Kind, stat.Size
	case vfs.OperationTruncate:
		reply.Status = service.namespace.Truncate(request.Path, request.Offset)
	default:
		reply.Status = vfs.StatusInvalidArgument
	}
	return reply
}
