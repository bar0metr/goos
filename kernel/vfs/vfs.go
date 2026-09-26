package vfs

import (
	"goos/internal/limits"
	"goos/kernel/ramfs"
)

const (
	MaxMounts    = 8
	MaxOpenFiles = 64
	// MaxOpenPerOwner bounds the files one owner holds open, so a single
	// session cannot take the whole table from the others.
	MaxOpenPerOwner = 16
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
	lastStatus = StatusNameTooLong
)

type Rights uint8

const (
	Read Rights = 1 << iota
	Write
)

type Handle uint64

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

type mount struct {
	backend Backend
	root    ramfs.NodeID
	path    [MaxPath]byte
	size    uint16
	used    bool
}

type openFile struct {
	node       ramfs.NodeID
	offset     uint64
	generation uint32
	owner      Owner
	mount      uint8
	rights     Rights
	used       bool
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
			entry.backend, entry.root, entry.path, entry.size, entry.used = backend, root, *canonical, uint16(size), true
			namespace.invalidate()
			return StatusOK
		}
	}
	return StatusResourceExhausted
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
	mountIndex, node, status := namespace.Resolve(path)
	if status != StatusOK {
		return 0, status
	}
	stat, backendStatus := namespace.mounts[mountIndex].backend.Stat(node)
	if backendStatus != ramfs.StatusOK || stat.Kind != ramfs.KindFile {
		return 0, StatusBackendError
	}
	for index := range namespace.files {
		entry := &namespace.files[index]
		if !entry.used {
			if entry.generation == 0 {
				entry.generation = 1
			}
			entry.node, entry.mount, entry.rights, entry.owner, entry.used = node, mountIndex, rights, owner, true
			return encodeHandle(index, entry.generation), StatusOK
		}
	}
	return 0, StatusResourceExhausted
}

func (namespace *Namespace) Close(owner Owner, handle Handle) Status {
	entry, status := namespace.file(owner, handle, 0)
	if status != StatusOK {
		return status
	}
	entry.release()
	return StatusOK
}

// CloseOwner closes every file owner holds, when the session it names has
// gone, and returns how many there were.
func (namespace *Namespace) CloseOwner(owner Owner) int {
	closed := 0
	for index := range namespace.files {
		if entry := &namespace.files[index]; owner != 0 && entry.used && entry.owner == owner {
			entry.release()
			closed++
		}
	}
	return closed
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

func (namespace *Namespace) Write(owner Owner, handle Handle, data []byte) (int, Status) {
	entry, status := namespace.file(owner, handle, Write)
	if status != StatusOK {
		return 0, status
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
		entry := ramfs.Entry{Stat: stat, Size: uint8(len(component))}
		copy(entry.Name[:], component)
		return entry, mountCursor + index + 1, StatusOK
	}
	return ramfs.Entry{}, mountCursor + len(namespace.mounts), StatusNotFound
}

func (namespace *Namespace) Truncate(path []byte, size uint64) Status {
	mountIndex, node, status := namespace.Resolve(path)
	if status != StatusOK {
		return status
	}
	return mapStatus(namespace.mounts[mountIndex].backend.Truncate(node, size))
}

func (namespace *Namespace) Unlink(path []byte) Status {
	if namespace.carriesMount(path) {
		return StatusBusy
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
	if status != StatusOK || sourceMount != targetMount {
		if status != StatusOK {
			return status
		}
		return StatusInvalidArgument
	}
	status = mapStatus(namespace.mounts[sourceMount].backend.RenameName(node, parent, name))
	if status == StatusOK {
		namespace.invalidate()
	}
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
	case ramfs.StatusIOError:
		// A filesystem service that is gone or stalled is a storage error.
		return StatusBackendError
	default:
		return StatusBackendError
	}
}
