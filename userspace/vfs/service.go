// Package vfsservice implements the bounded userspace VFS request dispatcher.
package vfsservice

import (
	"goos/internal/limits"
	"goos/kernel/initramfs"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

type Service struct {
	fs        ramfs.FileSystem
	namespace vfs.Namespace
	readData  [vfs.MaxReplyData]byte
	image     initramfs.Archive
	// persistent is the volume the FAT32 filesystem service serves (ADR
	// 0096). The VFS holds no filesystem code and no block endpoint for it.
	persistent Remote
	// persistentRoot is the root the volume was mounted with, so a restarted
	// service can be checked to serve the same one.
	persistentRoot    ramfs.NodeID
	persistentMounted bool
	// systemNode is the volume's system directory, protected from the moment
	// the volume is mounted, or zero for a volume without one (ADR 0175).
	systemNode ramfs.NodeID
	// published records that /system and /apps are mounted.
	published bool
}

// MountBootstrap mounts the boot archive read-only at /bootstrap, the
// programs the system starts from before storage is up and recovery runs
// from without it (ADR 0175). The archive is untrusted input, so a malformed
// image is refused here rather than at a later lookup.
func (service *Service) MountBootstrap(archive []byte) bool {
	if !service.image.Init(archive) {
		return false
	}
	// The kernel finds PID 1 as the member system/init, so the archive keeps
	// its members under system/. The mount root is that directory: the
	// member system/bin/ls is /bootstrap/bin/ls.
	name, valid := ramfs.MakeName([]byte(archiveDirectory))
	if !valid {
		return false
	}
	root, status := service.image.OpenName(service.image.Root(), name)
	if status != ramfs.StatusOK {
		return false
	}
	return service.namespace.Mount([]byte(BootstrapPath), &service.image, root) == vfs.StatusOK
}

const (
	// archiveDirectory is the archive directory mounted at BootstrapPath.
	archiveDirectory = "system"
	BootstrapPath    = "/bootstrap"
	// systemDirectory and appsDirectory are the volume directories
	// published at SystemPath and AppsPath.
	systemDirectory = "system"
	appsDirectory   = "apps"
	SystemPath      = "/system"
	AppsPath        = "/apps"
)

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

// AttachFilesystem connects the filesystem service at the other end of
// transport. The first attach mounts its volume at /persist, /home and /state
// and protects its system directory; a later one, after the service was
// restarted, only checks that the new instance serves the volume those mounts
// name, with the system directory where it was, so open files, mounts, and the
// protection stay valid across the restart. A service that refuses its volume
// leaves the namespace without persistent mounts rather than without a root.
func (service *Service) AttachFilesystem(transport Transport) bool {
	service.persistent.transport = transport
	root, status := service.persistent.Root()
	if status != ramfs.StatusOK {
		service.persistent.transport = nil
		return false
	}
	if service.persistentMounted {
		return root == service.persistentRoot && service.lookupSystem(root) == service.systemNode
	}
	if !service.mountPersistent(root) {
		// Mounting /persist, /home and /state is one operation. None can have
		// live references before AttachFilesystem returns, so this rollback
		// cannot be busy.
		service.namespace.UnmountBackend(&service.persistent)
		service.persistent.transport = nil
		return false
	}
	// No file of the volume is open yet, so nothing can have been granted
	// write access below the system directory before it is protected.
	service.systemNode = service.lookupSystem(root)
	if service.systemNode != 0 && service.namespace.Protect(&service.persistent, service.systemNode) != vfs.StatusOK {
		service.namespace.UnmountBackend(&service.persistent)
		service.persistent.transport = nil
		service.systemNode = 0
		return false
	}
	service.persistentRoot, service.persistentMounted = root, true
	return true
}

// lookupSystem returns the volume's system directory, or zero when the volume
// has none or the name is not a directory. It never creates one: a volume
// without an installed system is not given an empty one.
func (service *Service) lookupSystem(root ramfs.NodeID) ramfs.NodeID {
	name, _ := ramfs.MakeName([]byte(systemDirectory))
	node, status := service.persistent.OpenName(root, name)
	if status != ramfs.StatusOK {
		return 0
	}
	if stat, status := service.persistent.Stat(node); status != ramfs.StatusOK || stat.Kind != ramfs.KindDirectory {
		return 0
	}
	return node
}

// SystemProtected reports whether the mounted volume has a system directory
// and the namespace protects it.
func (service *Service) SystemProtected() bool {
	node, protected := service.namespace.Protected(&service.persistent)
	return service.persistentMounted && service.systemNode != 0 && protected && node == service.systemNode
}

// Publish mounts the protected system directory at /system and the volume's
// applications at /apps, creating the apps directory if the volume lacks it.
// Both appear or neither does. Publishing what is already published succeeds.
func (service *Service) Publish() vfs.Status {
	if service.published {
		return vfs.StatusOK
	}
	if !service.SystemProtected() {
		return vfs.StatusNotFound
	}
	apps, ok := service.persistentDirectory(service.persistentRoot, appsDirectory)
	if !ok {
		return vfs.StatusBackendError
	}
	if status := service.namespace.Mount([]byte(SystemPath), &service.persistent, service.systemNode); status != vfs.StatusOK {
		return status
	}
	if status := service.namespace.Mount([]byte(AppsPath), &service.persistent, apps); status != vfs.StatusOK {
		// Nothing can have opened a file through /system yet.
		service.namespace.UnmountPath([]byte(SystemPath))
		return status
	}
	service.published = true
	return vfs.StatusOK
}

// Withdraw removes /apps and /system. A file still open through either makes
// it busy, and then neither is removed.
func (service *Service) Withdraw() vfs.Status {
	if !service.published {
		return vfs.StatusOK
	}
	if status := service.namespace.UnmountPath([]byte(AppsPath)); status != vfs.StatusOK && status != vfs.StatusNotFound {
		return status
	}
	if status := service.namespace.UnmountPath([]byte(SystemPath)); status != vfs.StatusOK && status != vfs.StatusNotFound {
		// Put /apps back, so the namespace is as it was.
		if apps, ok := service.persistentDirectory(service.persistentRoot, appsDirectory); ok {
			service.namespace.Mount([]byte(AppsPath), &service.persistent, apps)
		}
		return status
	}
	service.published = false
	return vfs.StatusOK
}

// Published reports whether /system and /apps are mounted.
func (service *Service) Published() bool { return service.published }

// AttachCache gives the persistent backend the file cache it shares with the
// filesystem service (ADR 0098). Without it the volume can be listed but no
// file on it read or written.
func (service *Service) AttachCache(memory []byte) bool {
	return service.persistent.AttachCache(memory)
}

// DetachFilesystem forgets a filesystem service that has gone. Its mounts stay
// in the namespace and answer with I/O errors until a restarted service is
// attached.
func (service *Service) DetachFilesystem() { service.persistent.transport = nil }

// UnmountFilesystem flushes and atomically removes every namespace alias of
// the persistent backend. Open files make the operation busy; a flush failure
// leaves the namespace and transport intact.
func (service *Service) UnmountFilesystem() vfs.Status {
	if !service.persistentMounted {
		return vfs.StatusNotFound
	}
	if !service.persistent.Flush() {
		return vfs.StatusBackendError
	}
	if status := service.namespace.UnmountBackend(&service.persistent); status != vfs.StatusOK {
		return status
	}
	service.persistent.transport = nil
	service.persistentRoot = 0
	service.persistentMounted = false
	service.systemNode, service.published = 0, false
	service.persistent.forgetAll()
	return vfs.StatusOK
}

// PersistentMounted reports whether the persistent volume is in the namespace.
func (service *Service) PersistentMounted() bool { return service.persistentMounted }

// Sync makes every completed write to the persistent volume durable.
func (service *Service) Sync() bool { return service.persistent.Flush() }

func (service *Service) mountPersistent(root ramfs.NodeID) bool {
	if service.namespace.Mount([]byte("/persist"), &service.persistent, root) != vfs.StatusOK {
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

// ConfigureRAMFS sets where the volatile root keeps its pages and how much it
// may hold (ADR 0105). It must come before Init; without it the root uses the
// default store and limits.
func (service *Service) ConfigureRAMFS(store ramfs.Store, limits ramfs.Limits) bool {
	return service.fs.Configure(store, limits)
}

// SetRAMFSClock supplies the native realtime clock used for volatile metadata.
func (service *Service) SetRAMFSClock(clock func() (ramfs.Timestamp, bool)) {
	service.fs.SetClock(clock)
}

// RAMFSUsage reports what the volatile root holds against its limits.
func (service *Service) RAMFSUsage() ramfs.Usage { return service.fs.Usage() }

// Init builds the volatile root with its /tmp and /run. /bootstrap, /system
// and /apps are not created here: they are mount points, and the namespace
// lists a mount in its parent directory without needing a placeholder to
// stand in for it.
func (service *Service) Init() bool {
	root := service.fs.Init()
	for _, name := range [...]string{"tmp", "run"} {
		if _, status := service.fs.Mkdir(root, []byte(name)); status != ramfs.StatusOK {
			return false
		}
	}
	// The mount points of the system are kept free while nothing is mounted
	// there: a /system a client made in the volatile root is never mistaken
	// for the installed one (ADR 0175).
	for _, path := range [...]string{BootstrapPath, SystemPath, AppsPath} {
		if service.namespace.Reserve([]byte(path)) != vfs.StatusOK {
			return false
		}
	}
	return service.namespace.Mount([]byte("/"), &service.fs, root) == vfs.StatusOK
}

// ReadFile reads from an open file at its position into target, which may be
// as large as the caller likes, and advances the position. The bulk
// operations use it with the memory a client attached (ADR 0107).
func (service *Service) ReadFile(owner vfs.Owner, file vfs.Handle, target []byte) (int, vfs.Status) {
	return service.namespace.Read(owner, file, target)
}

// PrepareExecutable pins file against writes and truncation. The pin follows
// the stable open object across rename and ends when its session closes.
func (service *Service) PrepareExecutable(owner vfs.Owner, file vfs.Handle) vfs.Status {
	return service.namespace.PrepareExecutable(owner, file)
}

// ReadFileAt reads source bytes without disturbing the ordinary file cursor.
func (service *Service) ReadFileAt(owner vfs.Owner, file vfs.Handle, offset uint64, target []byte) (int, vfs.Status) {
	return service.namespace.ReadAt(owner, file, offset, target)
}

// WriteFile writes data to an open file at its position and advances it.
func (service *Service) WriteFile(owner vfs.Owner, file vfs.Handle, data []byte) (int, vfs.Status) {
	return service.namespace.Write(owner, file, data)
}

// CloseSession closes the files a session left open when it went.
func (service *Service) CloseSession(owner vfs.Owner) int { return service.namespace.CloseOwner(owner) }

// Dispatch serves one request from the session owner. File handles are the
// session's own: another session's handle is not found.
func (service *Service) Dispatch(owner vfs.Owner, request vfs.Request) vfs.Reply {
	reply := vfs.Reply{Operation: request.Operation}
	// A path over the native limits is refused as such, not as a malformed
	// request, so a client can say what is wrong (ADR 0110).
	if tooLong(request.Path) || tooLong(request.Target) {
		reply.Status = vfs.StatusNameTooLong
		return reply
	}
	switch request.Operation {
	case vfs.OperationOpen:
		reply.Handle, reply.Status = service.namespace.Open(owner, request.Path, request.Rights)
	case vfs.OperationClose:
		reply.Status = service.namespace.Close(owner, request.Handle)
	case vfs.OperationRead:
		size := int(request.Size)
		if size > len(service.readData) {
			size = len(service.readData)
		}
		count, status := service.namespace.Read(owner, request.Handle, service.readData[:size])
		reply.Status, reply.Data, reply.Size = status, service.readData[:count], uint64(count)
	case vfs.OperationWrite:
		count, status := service.namespace.Write(owner, request.Handle, request.Data)
		reply.Status, reply.Size = status, uint64(count)
	case vfs.OperationSeek:
		reply.Status = service.namespace.Seek(owner, request.Handle, request.Offset)
	case vfs.OperationCreate, vfs.OperationMkdir:
		reply.Status = service.namespace.Create(request.Path, request.Operation == vfs.OperationMkdir)
	case vfs.OperationReadDir:
		entry, cursor, status := service.namespace.ReadDir(request.Path, int(request.Offset))
		reply.Status, reply.Cursor = status, uint32(cursor)
		if status == vfs.StatusOK {
			reply.Kind, reply.Size, reply.Name = entry.Stat.Kind, entry.Stat.Size, entry.Name[:entry.Size]
			reply.Node = uint64(entry.Stat.ID)
			reply.Created, reply.Modified = entry.Stat.Created, entry.Stat.Modified
			reply.Attributes, reply.AttributesAvailable = entry.Stat.Attributes, entry.Stat.AttributesAvailable
			reply.MountID = vfs.MountID(entry.Stat.MountID)
		}
	case vfs.OperationRename:
		reply.Status = service.namespace.Rename(request.Path, request.Target)
	case vfs.OperationReplace:
		reply.Status = service.namespace.Replace(request.Path, request.Target)
	case vfs.OperationUnlink:
		reply.Status = service.namespace.Unlink(request.Path)
	case vfs.OperationStat:
		// A stat with a handle and no path describes that open file.
		var stat ramfs.Stat
		var status vfs.Status
		if len(request.Path) == 0 && request.Handle != 0 {
			stat, status = service.namespace.StatOpen(owner, request.Handle)
		} else {
			stat, status = service.namespace.Stat(request.Path)
		}
		reply.Status, reply.Kind, reply.Size = status, stat.Kind, stat.Size
		reply.Node = uint64(stat.ID)
		reply.Created, reply.Modified = stat.Created, stat.Modified
		reply.Attributes, reply.AttributesAvailable = stat.Attributes, stat.AttributesAvailable
		reply.MountID = vfs.MountID(stat.MountID)
	case vfs.OperationTruncate:
		reply.Status = service.namespace.Truncate(request.Path, request.Offset)
	case vfs.OperationFilesystemInfo:
		info, mountID, status := service.namespace.FilesystemInfo(request.Path, request.MountID)
		reply.Status, reply.MountID, reply.Filesystem = status, mountID, info
		if status == vfs.StatusOK {
			reply.Name = info.Name()
		}
	case vfs.OperationReadMount:
		mount, cursor, status := service.namespace.ReadMount(int(request.Offset))
		reply.Status, reply.Cursor = status, uint32(cursor)
		if status == vfs.StatusOK {
			reply.MountID, reply.Filesystem = mount.ID, mount.Filesystem
			reply.Name = mount.Path[:mount.PathSize]
		}
	default:
		reply.Status = vfs.StatusInvalidArgument
	}
	return reply
}

// tooLong reports whether a path is longer than PathMax or has a component
// longer than NameMax.
func tooLong(path []byte) bool {
	if len(path) > limits.PathMax {
		return true
	}
	start := 0
	for index := 0; index <= len(path); index++ {
		if index == len(path) || path[index] == '/' {
			if index-start > limits.NameMax {
				return true
			}
			start = index + 1
		}
	}
	return false
}
