package vfs

import (
	"goos/internal/limits"
	"goos/kernel/ramfs"
)

const (
	MaxMounts = 8
	// MaxOpenFiles is the files open at once in one namespace, a few dozen
	// bytes each (ADR 0111).
	MaxOpenFiles = 256
	// MaxOpenPerOwner bounds the files one owner holds open, so a single
	// session cannot take the whole table from the others.
	MaxOpenPerOwner = 64
	// MaxPath is the native path limit (ADR 0110).
	MaxPath  = limits.PathMax
	MaxCache = 32
	// maxCachedPath bounds the paths the resolution cache remembers. Longer
	// ones resolve every time; they are rare, and remembering them would
	// cost the cache a PathMax buffer per entry.
	maxCachedPath = 256
	// mountCursor separates the backend phase of a directory scan from the
	// mount phase. It is above every cursor a backend can return and below the
	// uint32 the service protocol carries.
	mountCursor = 1 << 30
)

type Status uint8

const (
	StatusOK Status = iota
	StatusInvalidArgument
	StatusNotFound
	StatusAlreadyExists
	StatusPermissionDenied
	StatusResourceExhausted
	StatusBackendError
	// The statuses below name why a path was refused so a client can say so
	// instead of reporting an invalid argument. They follow the original set
	// to keep its wire values.
	StatusNotDirectory
	StatusIsDirectory
	StatusNotEmpty
	// StatusBusy refuses to remove a path a mount depends on.
	StatusBusy
	// StatusNoSpace is storage that is full.
	StatusNoSpace
	// StatusCorrupt is a filesystem whose on-disk metadata failed validation.
	StatusCorrupt
	// StatusNameTooLong refuses a name longer than NameMax, and answers a
	// listing whose next name is too long for a message and that attached no
	// object to receive it; Size is then the name's length (ADR 0110).
	StatusNameTooLong
	// StatusTimeout is a filesystem service that did not answer in time,
	// StatusPeerDied one that has gone, and StatusUnsupported an operation or
	// on-disk feature a filesystem does not implement. StatusBackendError
	// remains the I/O error: a device or a reply that failed.
	StatusTimeout
	StatusPeerDied
	StatusUnsupported
	lastStatus = StatusUnsupported
)

type Rights uint8

const (
	Read Rights = 1 << iota
	Write
)

type Handle uint64

// MountID is a descriptive namespace identity. Its generation prevents a
// stale enumeration result from naming a later occupant of the same slot.
// It grants no file-operation authority.
type MountID uint64

// Owner is who opened a file: the VFS passes the session a request arrived
// on. A handle is valid only for its owner, and an owner's files are closed
// together when it goes. Zero is no owner.
type Owner uint16

type Backend interface {
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
}

type filesystemBackend interface {
	FilesystemInfo() (ramfs.FilesystemInfo, ramfs.Status)
}

// ReplaceBackend replaces an existing regular file with another one of the
// same filesystem: name in parent takes id's contents and id's own name goes
// (ADR 0129). A backend states in its documentation what an interruption can
// leave behind.
type ReplaceBackend interface {
	ReplaceName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status
}

// ObjectBackend gives an opened file identity independent of pathname lookup.
// Backends whose node IDs are already stable need not implement it.
type ObjectBackend interface {
	AcquireObject(ramfs.NodeID) (ramfs.NodeID, ramfs.Status)
	ReleaseObject(ramfs.NodeID) (ramfs.NodeID, ramfs.Status)
}

type mount struct {
	backend        Backend
	root           ramfs.NodeID
	path           [MaxPath]byte
	size           uint16
	generation     uint32
	filesystemType ramfs.FilesystemType
	readOnly       bool
	used           bool
}

// MountInfo is one bounded mount-table entry.
type MountInfo struct {
	ID         MountID
	Path       [MaxPath]byte
	PathSize   uint16
	Filesystem ramfs.FilesystemInfo
}

type openFile struct {
	node       ramfs.NodeID
	offset     uint64
	generation uint32
	owner      Owner
	mount      uint8
	rights     Rights
	used       bool
	executable bool
}

type cacheEntry struct {
	node  ramfs.NodeID
	mount uint8
	path  [maxCachedPath]byte
	size  uint16
	used  bool
}

type Namespace struct {
	mounts [MaxMounts]mount
	files  [MaxOpenFiles]openFile
	cache  [MaxCache]cacheEntry
	// Canonical paths live here because a local [MaxPath]byte escapes into the
	// helpers below and the kernel's leaking heap never frees it. Resolve owns
	// resolved; the operations that canonicalize a path before calling Resolve
	// own requested.
	resolved  [MaxPath]byte
	requested [MaxPath]byte
}

func (namespace *Namespace) Mount(path []byte, backend Backend, root ramfs.NodeID) Status {
	canonical := &namespace.requested
	size, ok := canonicalPath(canonical, path)
	if !ok || backend == nil || root == 0 {
		return StatusInvalidArgument
	}
	for index := range namespace.mounts {
		entry := &namespace.mounts[index]
		if entry.used && equal(entry.path[:entry.size], canonical[:size]) {
			return StatusAlreadyExists
		}
	}
	for index := range namespace.mounts {
		entry := &namespace.mounts[index]
		if !entry.used {
			if entry.generation == 0 {
				entry.generation = 1
			}
			entry.backend, entry.root, entry.size, entry.used = backend, root, uint16(size), true
			if filesystem, supported := backend.(filesystemBackend); supported {
				if info, status := filesystem.FilesystemInfo(); status == ramfs.StatusOK && info.Valid() && info.Type != 0 {
					entry.filesystemType, entry.readOnly = info.Type, info.ReadOnly
				}
			}
			copy(entry.path[:], canonical[:size])
			namespace.invalidate()
			return StatusOK
		}
	}
	return StatusResourceExhausted
}

// UnmountBackend atomically detaches every mount served by backend. It refuses
// while an open file still refers to any of them, so a successful return makes
// it safe for the caller to stop the backend and release its authority.
func (namespace *Namespace) UnmountBackend(backend Backend) Status {
	if backend == nil {
		return StatusInvalidArgument
	}
	found := false
	for index := range namespace.mounts {
		if namespace.mounts[index].used && namespace.mounts[index].backend == backend {
			found = true
			for fileIndex := range namespace.files {
				file := &namespace.files[fileIndex]
				if file.used && int(file.mount) == index {
					return StatusBusy
				}
			}
		}
	}
	if !found {
		return StatusNotFound
	}
	for index := range namespace.mounts {
		if namespace.mounts[index].used && namespace.mounts[index].backend == backend {
			entry := &namespace.mounts[index]
			generation := entry.generation + 1
			if generation == 0 {
				generation = 1
			}
			*entry = mount{generation: generation}
		}
	}
	namespace.invalidate()
	return StatusOK
}

func (namespace *Namespace) Resolve(path []byte) (uint8, ramfs.NodeID, Status) {
	canonical := &namespace.resolved
	size, ok := canonicalPath(canonical, path)
	if !ok {
		return 0, 0, StatusInvalidArgument
	}
	if mountIndex, node, found := namespace.cached(canonical[:size]); found {
		return mountIndex, node, StatusOK
	}
	mountIndex, relative, status := namespace.selectMount(canonical[:size])
	if status != StatusOK {
		return 0, 0, status
	}
	mounted := &namespace.mounts[mountIndex]
	node := mounted.root
	for relative < size {
		component, rest := nextComponent(canonical[relative:size])
		name, valid := ramfs.MakeName(component)
		if !valid {
			return 0, 0, StatusInvalidArgument
		}
		var backendStatus ramfs.Status
		node, backendStatus = mounted.backend.OpenName(node, name)
		if backendStatus != ramfs.StatusOK {
			return 0, 0, mapStatus(backendStatus)
		}
		relative = size - len(rest)
	}
	namespace.remember(canonical[:size], mountIndex, node)
	return mountIndex, node, StatusOK
}

func (namespace *Namespace) Open(owner Owner, path []byte, rights Rights) (Handle, Status) {
	if owner == 0 || rights == 0 || rights&^(Read|Write) != 0 {
		return 0, StatusInvalidArgument
	}
	if namespace.OpenBy(owner) >= MaxOpenPerOwner {
		return 0, StatusResourceExhausted
	}
	free := -1
	for index := range namespace.files {
		if !namespace.files[index].used {
			free = index
			break
		}
	}
	if free < 0 {
		return 0, StatusResourceExhausted
	}
	mountIndex, node, status := namespace.Resolve(path)
	if status != StatusOK {
		return 0, status
	}
	stat, backendStatus := namespace.mounts[mountIndex].backend.Stat(node)
	if backendStatus != ramfs.StatusOK || stat.Kind != ramfs.KindFile {
		return 0, StatusBackendError
	}
	backend := namespace.mounts[mountIndex].backend
	if objects, ok := backend.(ObjectBackend); ok {
		node, backendStatus = objects.AcquireObject(node)
		if backendStatus != ramfs.StatusOK || node == 0 {
			if backendStatus == ramfs.StatusOK {
				backendStatus = ramfs.StatusIOError
			}
			return 0, mapStatus(backendStatus)
		}
	}
	entry := &namespace.files[free]
	if entry.generation == 0 {
		entry.generation = 1
	}
	entry.node, entry.mount, entry.rights, entry.owner, entry.used = node, mountIndex, rights, owner, true
	return encodeHandle(free, entry.generation), StatusOK
}

func (namespace *Namespace) Close(owner Owner, handle Handle) Status {
	entry, status := namespace.file(owner, handle, 0)
	if status != StatusOK {
		return status
	}
	return namespace.release(entry)
}

// CloseOwner closes every file owner holds, when the session it names has
// gone, and returns how many there were.
func (namespace *Namespace) CloseOwner(owner Owner) int {
	closed := 0
	for index := range namespace.files {
		if entry := &namespace.files[index]; owner != 0 && entry.used && entry.owner == owner {
			namespace.release(entry)
			closed++
		}
	}
	return closed
}

func (namespace *Namespace) release(entry *openFile) Status {
	status := StatusOK
	if objects, ok := namespace.mounts[entry.mount].backend.(ObjectBackend); ok {
		_, backendStatus := objects.ReleaseObject(entry.node)
		status = mapStatus(backendStatus)
	}
	entry.release()
	return status
}

// OpenBy counts the files owner holds open.
func (namespace *Namespace) OpenBy(owner Owner) int {
	count := 0
	for index := range namespace.files {
		if entry := &namespace.files[index]; entry.used && entry.owner == owner {
			count++
		}
	}
	return count
}

// release frees the slot and moves its generation on, so the handle that
// named it names nothing from now on.
func (entry *openFile) release() {
	generation := entry.generation + 1
	if generation == 0 {
		generation = 1
	}
	*entry = openFile{generation: generation}
}

func (namespace *Namespace) Read(owner Owner, handle Handle, target []byte) (int, Status) {
	entry, status := namespace.file(owner, handle, Read)
	if status != StatusOK {
		return 0, status
	}
	count, backendStatus := namespace.mounts[entry.mount].backend.Read(entry.node, entry.offset, target)
	if backendStatus == ramfs.StatusOK {
		entry.offset += uint64(count)
	}
	return count, mapStatus(backendStatus)
}

// ReadAt reads an open file without changing its position. Executable page
// requests use it because faults from several processes may name one source.
func (namespace *Namespace) ReadAt(owner Owner, handle Handle, offset uint64, target []byte) (int, Status) {
	entry, status := namespace.file(owner, handle, Read)
	if status != StatusOK {
		return 0, status
	}
	count, backendStatus := namespace.mounts[entry.mount].backend.Read(entry.node, offset, target)
	return count, mapStatus(backendStatus)
}

// PrepareExecutable makes a readable open file immutable until the handle is
// closed. Rename remains valid because the open object, not its path, is the
// identity held by the executable source.
func (namespace *Namespace) PrepareExecutable(owner Owner, handle Handle) Status {
	entry, status := namespace.file(owner, handle, Read)
	if status == StatusOK {
		entry.executable = true
	}
	return status
}

func (namespace *Namespace) executablePinned(mount uint8, node ramfs.NodeID) bool {
	return namespace.held(mount, node, true)
}

// held reports whether a session holds the file at node open, or, with
// executableOnly, open as a prepared executable.
func (namespace *Namespace) held(mount uint8, node ramfs.NodeID, executableOnly bool) bool {
	return namespace.heldDirect(mount, node, executableOnly) || namespace.heldLocation(mount, node, executableOnly)
}

func (namespace *Namespace) heldDirect(mount uint8, node ramfs.NodeID, executableOnly bool) bool {
	for index := range namespace.files {
		entry := &namespace.files[index]
		if entry.used && (entry.executable || !executableOnly) && entry.mount == mount && entry.node == node {
			return true
		}
	}
	return false
}

// heldLocation compares a pathname node with stable open-object cookies.
// AcquireObject returns the existing cookie for a live file; the temporary
// reference is released before this call returns.
func (namespace *Namespace) heldLocation(mount uint8, node ramfs.NodeID, executableOnly bool) bool {
	if int(mount) >= len(namespace.mounts) {
		return false
	}
	objects, ok := namespace.mounts[mount].backend.(ObjectBackend)
	if !ok {
		return false
	}
	object, status := objects.AcquireObject(node)
	if status != ramfs.StatusOK || object == 0 {
		return false
	}
	held := namespace.heldDirect(mount, object, executableOnly)
	objects.ReleaseObject(object)
	return held
}

func (namespace *Namespace) Write(owner Owner, handle Handle, data []byte) (int, Status) {
	entry, status := namespace.file(owner, handle, Write)
	if status != StatusOK {
		return 0, status
	}
	if namespace.executablePinned(entry.mount, entry.node) {
		return 0, StatusPermissionDenied
	}
	count, backendStatus := namespace.mounts[entry.mount].backend.Write(entry.node, entry.offset, data)
	if backendStatus == ramfs.StatusOK {
		entry.offset += uint64(count)
	}
	return count, mapStatus(backendStatus)
}

func (namespace *Namespace) Seek(owner Owner, handle Handle, offset uint64) Status {
	entry, status := namespace.file(owner, handle, 0)
	if status != StatusOK {
		return status
	}
	entry.offset = offset
	return StatusOK
}

func (namespace *Namespace) Create(path []byte, directory bool) Status {
	mountIndex, parent, name, status := namespace.resolveParent(path)
	if status != StatusOK {
		return status
	}
	if directory {
		_, status = mapNodeResult(namespace.mounts[mountIndex].backend.MkdirName(parent, name))
	} else {
		_, status = mapNodeResult(namespace.mounts[mountIndex].backend.CreateName(parent, name))
	}
	if status == StatusOK {
		namespace.invalidate()
	}
	return status
}

func (namespace *Namespace) Stat(path []byte) (ramfs.Stat, Status) {
	mountIndex, node, status := namespace.Resolve(path)
	if status != StatusOK {
		return ramfs.Stat{}, status
	}
	stat, backendStatus := namespace.mounts[mountIndex].backend.Stat(node)
	if backendStatus == ramfs.StatusOK {
		stat.MountID = uint64(namespace.mountID(int(mountIndex)))
	}
	return stat, mapStatus(backendStatus)
}

// StatOpen describes the file an open handle names. It follows the handle,
// not a path, so the answer stays right after the file is renamed, and a
// client needs no path to learn how long its open file is.
func (namespace *Namespace) StatOpen(owner Owner, handle Handle) (ramfs.Stat, Status) {
	entry, status := namespace.file(owner, handle, 0)
	if status != StatusOK {
		return ramfs.Stat{}, status
	}
	stat, backendStatus := namespace.mounts[entry.mount].backend.Stat(entry.node)
	if backendStatus == ramfs.StatusOK {
		stat.MountID = uint64(namespace.mountID(int(entry.mount)))
	}
	return stat, mapStatus(backendStatus)
}

// ReadDir lists a directory in two phases. Cursors below mountCursor belong to
// the backend that owns the directory; once the backend is exhausted the scan
// continues through the mount table, so a mounted filesystem is discoverable by
// walking the tree rather than only by knowing its path in advance.
func (namespace *Namespace) ReadDir(path []byte, cursor int) (ramfs.Entry, int, Status) {
	canonical := &namespace.requested
	size, ok := canonicalPath(canonical, path)
	if !ok || cursor < 0 {
		return ramfs.Entry{}, cursor, StatusInvalidArgument
	}
	mountIndex, node, status := namespace.Resolve(canonical[:size])
	if status != StatusOK {
		return ramfs.Entry{}, cursor, status
	}
	if cursor < mountCursor {
		entry, next, backendStatus := namespace.mounts[mountIndex].backend.ReadDir(node, cursor)
		if backendStatus != ramfs.StatusNotFound {
			if backendStatus == ramfs.StatusOK {
				entry.Stat.MountID = uint64(namespace.mountID(int(mountIndex)))
			}
			return entry, next, mapStatus(backendStatus)
		}
		cursor = mountCursor
	}
	return namespace.mountedEntry(canonical[:size], node, mountIndex, cursor-mountCursor)
}

// mountedEntry reports the filesystems mounted directly on the directory named
// by path, starting at index. A mount whose name the owning backend already
// listed is skipped, because resolution prefers the mount and the name must
// name one entry rather than two.
func (namespace *Namespace) mountedEntry(path []byte, node ramfs.NodeID, owner uint8, index int) (ramfs.Entry, int, Status) {
	if index < 0 {
		index = 0
	}
	for ; index < len(namespace.mounts); index++ {
		mounted := &namespace.mounts[index]
		if !mounted.used || uint8(index) == owner {
			continue
		}
		component, direct := mountedName(path, mounted.path[:mounted.size])
		if !direct {
			continue
		}
		name, valid := ramfs.MakeName(component)
		if !valid {
			continue
		}
		if _, status := namespace.mounts[owner].backend.OpenName(node, name); status == ramfs.StatusOK {
			continue
		}
		stat, status := mounted.backend.Stat(mounted.root)
		if status != ramfs.StatusOK {
			// A mount root is a directory by construction, so the mount stays
			// visible even when its backend cannot describe it.
			stat = ramfs.Stat{ID: mounted.root, Kind: ramfs.KindDirectory}
		}
		entry := ramfs.Entry{Stat: stat, Size: uint16(len(component))}
		entry.Stat.MountID = uint64(namespace.mountID(index))
		copy(entry.Name[:], component)
		return entry, mountCursor + index + 1, StatusOK
	}
	return ramfs.Entry{}, mountCursor + len(namespace.mounts), StatusNotFound
}

// FilesystemInfo reports the mount selected by path, or the exact mount named
// by id. Exactly one selector must be supplied.
func (namespace *Namespace) FilesystemInfo(path []byte, id MountID) (ramfs.FilesystemInfo, MountID, Status) {
	if (len(path) == 0) == (id == 0) {
		return ramfs.FilesystemInfo{}, 0, StatusInvalidArgument
	}
	index := -1
	if id != 0 {
		var ok bool
		index, ok = namespace.mountIndex(id)
		if !ok {
			return ramfs.FilesystemInfo{}, 0, StatusNotFound
		}
	} else {
		resolved, _, status := namespace.Resolve(path)
		if status != StatusOK {
			return ramfs.FilesystemInfo{}, 0, status
		}
		index = int(resolved)
	}
	backend, ok := namespace.mounts[index].backend.(filesystemBackend)
	if !ok {
		return ramfs.FilesystemInfo{}, 0, StatusUnsupported
	}
	info, backendStatus := backend.FilesystemInfo()
	if backendStatus != ramfs.StatusOK || !info.Valid() {
		if backendStatus == ramfs.StatusOK {
			backendStatus = ramfs.StatusIOError
		}
		return ramfs.FilesystemInfo{}, 0, mapStatus(backendStatus)
	}
	return info, namespace.mountID(index), StatusOK
}

// ReadMount returns the next used slot at or after cursor. The fixed table
// makes enumeration bounded and stable while it is unchanged.
func (namespace *Namespace) ReadMount(cursor int) (MountInfo, int, Status) {
	if cursor < 0 || cursor > len(namespace.mounts) {
		return MountInfo{}, cursor, StatusInvalidArgument
	}
	for index := cursor; index < len(namespace.mounts); index++ {
		entry := &namespace.mounts[index]
		if !entry.used {
			continue
		}
		info := ramfs.FilesystemInfo{Type: entry.filesystemType, ReadOnly: entry.readOnly}
		if info.Type == 0 {
			filesystem, supported := entry.backend.(filesystemBackend)
			if !supported {
				return MountInfo{}, index, StatusUnsupported
			}
			var backendStatus ramfs.Status
			info, backendStatus = filesystem.FilesystemInfo()
			if backendStatus != ramfs.StatusOK || !info.Valid() || info.Type == 0 {
				if backendStatus == ramfs.StatusOK {
					backendStatus = ramfs.StatusIOError
				}
				return MountInfo{}, index, mapStatus(backendStatus)
			}
			entry.filesystemType, entry.readOnly = info.Type, info.ReadOnly
		}
		info = ramfs.FilesystemInfo{Type: entry.filesystemType, ReadOnly: entry.readOnly}
		result := MountInfo{ID: namespace.mountID(index), PathSize: entry.size, Filesystem: info}
		copy(result.Path[:], entry.path[:entry.size])
		return result, index + 1, StatusOK
	}
	return MountInfo{}, len(namespace.mounts), StatusNotFound
}

func (namespace *Namespace) mountID(index int) MountID {
	entry := &namespace.mounts[index]
	return MountID(uint64(entry.generation)<<32 | uint64(index+1))
}

func (namespace *Namespace) mountIndex(id MountID) (int, bool) {
	index := int(uint32(id)) - 1
	if index < 0 || index >= len(namespace.mounts) {
		return 0, false
	}
	entry := &namespace.mounts[index]
	return index, entry.used && uint32(uint64(id)>>32) == entry.generation
}

func (namespace *Namespace) Truncate(path []byte, size uint64) Status {
	mountIndex, node, status := namespace.Resolve(path)
	if status != StatusOK {
		return status
	}
	if namespace.executablePinned(mountIndex, node) {
		return StatusPermissionDenied
	}
	return mapStatus(namespace.mounts[mountIndex].backend.Truncate(node, size))
}

func (namespace *Namespace) Unlink(path []byte) Status {
	if namespace.carriesMount(path) {
		return StatusBusy
	}
	if mount, node, resolved := namespace.Resolve(path); resolved == StatusOK && namespace.executablePinned(mount, node) {
		return StatusPermissionDenied
	}
	mountIndex, parent, name, status := namespace.resolveParent(path)
	if status != StatusOK {
		return status
	}
	status = mapStatus(namespace.mounts[mountIndex].backend.UnlinkName(parent, name))
	if status == StatusOK {
		namespace.invalidate()
	}
	return status
}

// carriesMount reports whether removing path would orphan a mount, either
// because path names a mount root or because a mount sits below it. The node
// check matters because one filesystem can be reachable under several paths:
// removing /persist/home must not strand a mount of the same directory at
// /home.
func (namespace *Namespace) carriesMount(path []byte) bool {
	canonical := &namespace.requested
	size, ok := canonicalPath(canonical, path)
	if !ok {
		return false
	}
	for index := range namespace.mounts {
		if entry := &namespace.mounts[index]; entry.used && mountMatches(entry.path[:entry.size], canonical[:size]) {
			return true
		}
	}
	mountIndex, node, status := namespace.Resolve(canonical[:size])
	if status != StatusOK {
		return false
	}
	backend := namespace.mounts[mountIndex].backend
	for index := range namespace.mounts {
		if entry := &namespace.mounts[index]; entry.used && entry.root == node && entry.backend == backend {
			return true
		}
	}
	return false
}

func (namespace *Namespace) Rename(path []byte, target []byte) Status {
	sourceMount, node, status := namespace.Resolve(path)
	if status != StatusOK {
		return status
	}
	targetMount, parent, name, status := namespace.resolveParent(target)
	if status != StatusOK {
		return status
	}
	if sourceMount != targetMount {
		// A rename moves a name within one filesystem; moving between two is
		// a copy, which is the caller's to make.
		return StatusUnsupported
	}
	status = mapStatus(namespace.mounts[sourceMount].backend.RenameName(node, parent, name))
	if status == StatusOK {
		namespace.invalidate()
	}
	return status
}

// Replace moves the regular file path onto target within one filesystem
// (ADR 0129). An existing target must be a regular file that no session holds
// open; it is removed as path takes its name, and the backend decides what an
// interruption can leave. With no target this is a rename.
func (namespace *Namespace) Replace(path []byte, target []byte) Status {
	if namespace.carriesMount(path) || namespace.carriesMount(target) {
		return StatusBusy
	}
	sourceMount, node, status := namespace.Resolve(path)
	if status != StatusOK {
		return status
	}
	backend := namespace.mounts[sourceMount].backend
	if stat, backendStatus := backend.Stat(node); backendStatus != ramfs.StatusOK {
		return mapStatus(backendStatus)
	} else if stat.Kind != ramfs.KindFile {
		return StatusIsDirectory
	}
	targetMount, targetNode, status := namespace.Resolve(target)
	if status == StatusNotFound {
		return namespace.Rename(path, target)
	}
	if status != StatusOK {
		return status
	}
	if targetMount != sourceMount {
		return StatusUnsupported
	}
	if targetNode == node {
		return StatusInvalidArgument
	}
	if stat, backendStatus := backend.Stat(targetNode); backendStatus != ramfs.StatusOK {
		return mapStatus(backendStatus)
	} else if stat.Kind != ramfs.KindFile {
		return StatusIsDirectory
	}
	// Removing a file that is open is refused everywhere else too: FAT32 has
	// no delete-while-open, and a handle must not start naming new contents.
	if namespace.held(targetMount, targetNode, false) {
		return StatusBusy
	}
	replacer, ok := backend.(ReplaceBackend)
	if !ok {
		return StatusUnsupported
	}
	_, parent, name, status := namespace.resolveParent(target)
	if status != StatusOK {
		return status
	}
	status = mapStatus(replacer.ReplaceName(node, parent, name))
	namespace.invalidate()
	return status
}

func (namespace *Namespace) resolveParent(path []byte) (uint8, ramfs.NodeID, ramfs.Name, Status) {
	canonical := &namespace.requested
	size, ok := canonicalPath(canonical, path)
	if !ok || size == 1 {
		return 0, 0, ramfs.Name{}, StatusInvalidArgument
	}
	last := size - 1
	for last > 0 && canonical[last] != '/' {
		last--
	}
	name, ok := ramfs.MakeName(canonical[last+1 : size])
	if !ok {
		return 0, 0, ramfs.Name{}, StatusInvalidArgument
	}
	parentPath := canonical[:last]
	if len(parentPath) == 0 {
		parentPath = canonical[:1]
	}
	mountIndex, parent, status := namespace.Resolve(parentPath)
	return mountIndex, parent, name, status
}

func (namespace *Namespace) selectMount(path []byte) (uint8, int, Status) {
	selected := -1
	selectedSize := -1
	for index := range namespace.mounts {
		entry := &namespace.mounts[index]
		if !entry.used || int(entry.size) <= selectedSize || !mountMatches(path, entry.path[:entry.size]) {
			continue
		}
		selected, selectedSize = index, int(entry.size)
	}
	if selected < 0 {
		return 0, 0, StatusNotFound
	}
	relative := selectedSize
	for relative < len(path) && path[relative] == '/' {
		relative++
	}
	return uint8(selected), relative, StatusOK
}

// file returns the open file handle names for owner. Another owner's handle
// is not found rather than refused, so a session learns nothing about the
// files others hold.
func (namespace *Namespace) file(owner Owner, handle Handle, required Rights) (*openFile, Status) {
	index := int(uint32(handle)) - 1
	generation := uint32(uint64(handle) >> 32)
	if index < 0 || index >= len(namespace.files) || generation == 0 {
		return nil, StatusNotFound
	}
	entry := &namespace.files[index]
	if !entry.used || entry.generation != generation || owner == 0 || entry.owner != owner {
		return nil, StatusNotFound
	}
	if required != 0 && entry.rights&required != required {
		return nil, StatusPermissionDenied
	}
	return entry, StatusOK
}

func (namespace *Namespace) cached(path []byte) (uint8, ramfs.NodeID, bool) {
	for index := range namespace.cache {
		entry := &namespace.cache[index]
		if entry.used && equal(entry.path[:entry.size], path) {
			return entry.mount, entry.node, true
		}
	}
	return 0, 0, false
}

func (namespace *Namespace) remember(path []byte, mountIndex uint8, node ramfs.NodeID) {
	if len(path) > maxCachedPath {
		return
	}
	for index := range namespace.cache {
		entry := &namespace.cache[index]
		if !entry.used {
			entry.used, entry.mount, entry.node, entry.size = true, mountIndex, node, uint16(len(path))
			copy(entry.path[:], path)
			return
		}
	}
}

func (namespace *Namespace) invalidate() {
	for index := range namespace.cache {
		namespace.cache[index] = cacheEntry{}
	}
}

// canonicalPath writes the canonical form of path into output, which the
// caller owns. A ".." goes back over the last component written, so no table
// of component starts is needed.
func canonicalPath(output *[MaxPath]byte, path []byte) (int, bool) {
	if len(path) == 0 || len(path) > MaxPath || path[0] != '/' {
		return 0, false
	}
	output[0] = '/'
	size := 1
	remaining := path[1:]
	for len(remaining) != 0 {
		component, rest := nextComponent(remaining)
		remaining = rest
		if len(component) == 0 || (len(component) == 1 && component[0] == '.') {
			continue
		}
		if len(component) == 2 && component[0] == '.' && component[1] == '.' {
			for size > 1 && output[size-1] != '/' {
				size--
			}
			if size > 1 {
				size--
			}
			continue
		}
		if len(component) > ramfs.MaxName {
			return 0, false
		}
		for _, value := range component {
			if value == 0 {
				return 0, false
			}
		}
		if size > 1 {
			if size == MaxPath {
				return 0, false
			}
			output[size] = '/'
			size++
		}
		if size+len(component) > MaxPath {
			return 0, false
		}
		copy(output[size:], component)
		size += len(component)
	}
	return size, true
}

func nextComponent(path []byte) ([]byte, []byte) {
	for len(path) != 0 && path[0] == '/' {
		path = path[1:]
	}
	for index, value := range path {
		if value == '/' {
			return path[:index], path[index+1:]
		}
	}
	return path, nil
}

// mountedName returns the single component naming mountPath as a direct child
// of directory, and reports whether it is one.
func mountedName(directory []byte, mountPath []byte) ([]byte, bool) {
	if len(mountPath) <= len(directory) || !mountMatches(mountPath, directory) {
		return nil, false
	}
	remainder := mountPath[len(directory):]
	if remainder[0] == '/' {
		remainder = remainder[1:]
	}
	if len(remainder) == 0 {
		return nil, false
	}
	for _, value := range remainder {
		if value == '/' {
			return nil, false
		}
	}
	return remainder, true
}

func mountMatches(path []byte, mountPath []byte) bool {
	return len(path) >= len(mountPath) && equal(path[:len(mountPath)], mountPath) && (len(mountPath) == 1 || len(path) == len(mountPath) || path[len(mountPath)] == '/')
}

func equal(left []byte, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func encodeHandle(index int, generation uint32) Handle {
	return Handle(uint64(generation)<<32 | uint64(index+1))
}

func mapNodeResult(id ramfs.NodeID, status ramfs.Status) (ramfs.NodeID, Status) {
	return id, mapStatus(status)
}

func mapStatus(status ramfs.Status) Status {
	switch status {
	case ramfs.StatusOK:
		return StatusOK
	case ramfs.StatusInvalidArgument:
		return StatusInvalidArgument
	case ramfs.StatusNotDirectory:
		return StatusNotDirectory
	case ramfs.StatusIsDirectory:
		return StatusIsDirectory
	case ramfs.StatusNotEmpty:
		return StatusNotEmpty
	case ramfs.StatusNotFound:
		return StatusNotFound
	case ramfs.StatusAlreadyExists:
		return StatusAlreadyExists
	case ramfs.StatusResourceExhausted:
		return StatusResourceExhausted
	case ramfs.StatusPermissionDenied:
		return StatusPermissionDenied
	case ramfs.StatusNoSpace:
		return StatusNoSpace
	case ramfs.StatusCorrupt:
		return StatusCorrupt
	case ramfs.StatusTimeout:
		return StatusTimeout
	case ramfs.StatusPeerDied:
		return StatusPeerDied
	case ramfs.StatusUnsupported:
		return StatusUnsupported
	case ramfs.StatusIOError:
		return StatusBackendError
	default:
		return StatusBackendError
	}
}
