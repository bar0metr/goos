package vfs

import "goos/kernel/ramfs"

const (
	MaxMounts    = 8
	MaxOpenFiles = 64
	MaxPath      = 255
	MaxCache     = 32
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
)

type Rights uint8

const (
	Read Rights = 1 << iota
	Write
)

type Handle uint64

type Backend interface {
	Open(parent ramfs.NodeID, name []byte) (ramfs.NodeID, ramfs.Status)
	Create(parent ramfs.NodeID, name []byte) (ramfs.NodeID, ramfs.Status)
	Mkdir(parent ramfs.NodeID, name []byte) (ramfs.NodeID, ramfs.Status)
	Read(id ramfs.NodeID, offset uint64, target []byte) (int, ramfs.Status)
	Write(id ramfs.NodeID, offset uint64, data []byte) (int, ramfs.Status)
	Truncate(id ramfs.NodeID, size uint64) ramfs.Status
	Stat(id ramfs.NodeID) (ramfs.Stat, ramfs.Status)
	ReadDir(parent ramfs.NodeID, cursor int) (ramfs.Entry, int, ramfs.Status)
	Rename(id ramfs.NodeID, parent ramfs.NodeID, name []byte) ramfs.Status
	Unlink(parent ramfs.NodeID, name []byte) ramfs.Status
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
	mount      uint8
	rights     Rights
	used       bool
}

type cacheEntry struct {
	node  ramfs.NodeID
	mount uint8
	path  [MaxPath]byte
	size  uint16
	used  bool
}

type Namespace struct {
	mounts [MaxMounts]mount
	files  [MaxOpenFiles]openFile
	cache  [MaxCache]cacheEntry
}

func (namespace *Namespace) Mount(path []byte, backend Backend, root ramfs.NodeID) Status {
	canonical, size, ok := canonicalPath(path)
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
			entry.backend, entry.root, entry.path, entry.size, entry.used = backend, root, canonical, uint16(size), true
			namespace.invalidate()
			return StatusOK
		}
	}
	return StatusResourceExhausted
}

func (namespace *Namespace) Resolve(path []byte) (uint8, ramfs.NodeID, Status) {
	canonical, size, ok := canonicalPath(path)
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
	for len(relative) != 0 {
		name, rest := nextComponent(relative)
		var backendStatus ramfs.Status
		node, backendStatus = mounted.backend.Open(node, name)
		if backendStatus != ramfs.StatusOK {
			return 0, 0, mapStatus(backendStatus)
		}
		relative = rest
	}
	namespace.remember(canonical[:size], mountIndex, node)
	return mountIndex, node, StatusOK
}

func (namespace *Namespace) Open(path []byte, rights Rights) (Handle, Status) {
	if rights == 0 || rights&^(Read|Write) != 0 {
		return 0, StatusInvalidArgument
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
			entry.node, entry.mount, entry.rights, entry.used = node, mountIndex, rights, true
			return encodeHandle(index, entry.generation), StatusOK
		}
	}
	return 0, StatusResourceExhausted
}

func (namespace *Namespace) Close(handle Handle) Status {
	entry, status := namespace.file(handle, 0)
	if status != StatusOK {
		return status
	}
	generation := entry.generation + 1
	if generation == 0 {
		generation = 1
	}
	*entry = openFile{generation: generation}
	return StatusOK
}

func (namespace *Namespace) Read(handle Handle, target []byte) (int, Status) {
	entry, status := namespace.file(handle, Read)
	if status != StatusOK {
		return 0, status
	}
	count, backendStatus := namespace.mounts[entry.mount].backend.Read(entry.node, entry.offset, target)
	if backendStatus == ramfs.StatusOK {
		entry.offset += uint64(count)
	}
	return count, mapStatus(backendStatus)
}

func (namespace *Namespace) Write(handle Handle, data []byte) (int, Status) {
	entry, status := namespace.file(handle, Write)
	if status != StatusOK {
		return 0, status
	}
	count, backendStatus := namespace.mounts[entry.mount].backend.Write(entry.node, entry.offset, data)
	if backendStatus == ramfs.StatusOK {
		entry.offset += uint64(count)
	}
	return count, mapStatus(backendStatus)
}

func (namespace *Namespace) Seek(handle Handle, offset uint64) Status {
	entry, status := namespace.file(handle, 0)
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
		_, status = mapNodeResult(namespace.mounts[mountIndex].backend.Mkdir(parent, name))
	} else {
		_, status = mapNodeResult(namespace.mounts[mountIndex].backend.Create(parent, name))
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

func (namespace *Namespace) ReadDir(path []byte, cursor int) (ramfs.Entry, int, Status) {
	mountIndex, node, status := namespace.Resolve(path)
	if status != StatusOK {
		return ramfs.Entry{}, cursor, status
	}
	entry, next, backendStatus := namespace.mounts[mountIndex].backend.ReadDir(node, cursor)
	return entry, next, mapStatus(backendStatus)
}

func (namespace *Namespace) Truncate(path []byte, size uint64) Status {
	mountIndex, node, status := namespace.Resolve(path)
	if status != StatusOK {
		return status
	}
	return mapStatus(namespace.mounts[mountIndex].backend.Truncate(node, size))
}

func (namespace *Namespace) Unlink(path []byte) Status {
	mountIndex, parent, name, status := namespace.resolveParent(path)
	if status != StatusOK {
		return status
	}
	status = mapStatus(namespace.mounts[mountIndex].backend.Unlink(parent, name))
	if status == StatusOK {
		namespace.invalidate()
	}
	return status
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
	status = mapStatus(namespace.mounts[sourceMount].backend.Rename(node, parent, name))
	if status == StatusOK {
		namespace.invalidate()
	}
	return status
}

func (namespace *Namespace) resolveParent(path []byte) (uint8, ramfs.NodeID, []byte, Status) {
	canonical, size, ok := canonicalPath(path)
	if !ok || size == 1 {
		return 0, 0, nil, StatusInvalidArgument
	}
	last := size - 1
	for last > 0 && canonical[last] != '/' {
		last--
	}
	name := canonical[last+1 : size]
	parentPath := canonical[:last]
	if len(parentPath) == 0 {
		parentPath = canonical[:1]
	}
	mountIndex, parent, status := namespace.Resolve(parentPath)
	return mountIndex, parent, name, status
}

func (namespace *Namespace) selectMount(path []byte) (uint8, []byte, Status) {
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
		return 0, nil, StatusNotFound
	}
	relative := path[selectedSize:]
	for len(relative) != 0 && relative[0] == '/' {
		relative = relative[1:]
	}
	return uint8(selected), relative, StatusOK
}

func (namespace *Namespace) file(handle Handle, required Rights) (*openFile, Status) {
	index := int(uint32(handle)) - 1
	generation := uint32(uint64(handle) >> 32)
	if index < 0 || index >= len(namespace.files) || generation == 0 {
		return nil, StatusNotFound
	}
	entry := &namespace.files[index]
	if !entry.used || entry.generation != generation {
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

func canonicalPath(path []byte) ([MaxPath]byte, int, bool) {
	var output [MaxPath]byte
	if len(path) == 0 || len(path) > MaxPath || path[0] != '/' {
		return output, 0, false
	}
	output[0] = '/'
	size := 1
	components := [MaxPath]uint16{}
	count := 0
	remaining := path[1:]
	for len(remaining) != 0 {
		component, rest := nextComponent(remaining)
		remaining = rest
		if len(component) == 0 || (len(component) == 1 && component[0] == '.') {
			continue
		}
		if len(component) == 2 && component[0] == '.' && component[1] == '.' {
			if count != 0 {
				count--
				size = int(components[count])
			}
			continue
		}
		if len(component) > ramfs.MaxName {
			return output, 0, false
		}
		for _, value := range component {
			if value == 0 {
				return output, 0, false
			}
		}
		components[count] = uint16(size)
		count++
		if size > 1 {
			output[size] = '/'
			size++
		}
		if size+len(component) > MaxPath {
			return output, 0, false
		}
		copy(output[size:], component)
		size += len(component)
	}
	return output, size, true
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
	case ramfs.StatusInvalidArgument, ramfs.StatusNotDirectory, ramfs.StatusIsDirectory, ramfs.StatusNotEmpty:
		return StatusInvalidArgument
	case ramfs.StatusNotFound:
		return StatusNotFound
	case ramfs.StatusAlreadyExists:
		return StatusAlreadyExists
	case ramfs.StatusResourceExhausted:
		return StatusResourceExhausted
	default:
		return StatusBackendError
	}
}
