package vfs

import "goos/kernel/ramfs"

// MaxLinkExpansions bounds the symbolic links one path resolution follows.
// Linux stops at the same count with ELOOP.
const MaxLinkExpansions = 40

// linkHit is the symbolic link a resolve met: its mount and node, where its
// component starts and ends in the canonical path, and whether it was the
// last component.
type linkHit struct {
	node  ramfs.NodeID
	start int
	end   int
	mount uint8
	final bool
	valid bool
}

// linkState holds the paths expansion builds. path is the expanded path a
// caller receives, work the next candidate, and target a link's target as
// the backend returned it.
type linkState struct {
	path   [MaxPath]byte
	work   [MaxPath]byte
	target [MaxPath]byte
}

// holdsLinks reports whether a walk through backend has to look for symbolic
// links. FAT32 and the boot archive store none, so their walks stay as cheap
// as before.
func holdsLinks(backend Backend, filesystem ramfs.FilesystemType) bool {
	if _, ok := backend.(LinkBackend); !ok {
		return false
	}
	return filesystem != ramfs.FilesystemFAT32 && filesystem != ramfs.FilesystemSystemImage
}

// lastComponent returns where the last component of a canonical path starts.
func lastComponent(path []byte) int {
	index := len(path)
	for index > 0 && path[index-1] != '/' {
		index--
	}
	return index
}

func (namespace *Namespace) anyLinks() bool {
	for index := range namespace.mounts {
		if entry := &namespace.mounts[index]; entry.used && entry.links {
			return true
		}
	}
	return false
}

// rootOf returns the root owner's session is limited to, or nothing.
func (namespace *Namespace) rootOf(owner Owner) []byte {
	rooted := namespace.rooted
	if rooted == nil {
		return nil
	}
	for index := range namespace.authorities {
		if namespace.authorities[index].owner == owner {
			return rooted.roots[index][:rooted.sizes[index]]
		}
	}
	return nil
}

// Expand returns path with every symbolic link replaced by its target, for
// owner, and the last component replaced too when followFinal is set. The
// result names the same object every later step of the operation resolves,
// so what is authorized is what is changed (ADR 0223).
//
// A relative target continues from the directory holding the link. An
// absolute one starts at the owner's root: a session limited to /home/u that
// meets a link to /docs reaches /home/u/docs. ".." is applied to the spelled
// path, as everywhere in the namespace, so it cannot climb out through a
// link. Every expansion must stay inside the owner's root, and the owner
// must be allowed to read each link it follows. A walk that cannot finish,
// for a missing name or anything else, returns the path expanded so far and
// leaves the error to the operation.
//
// The returned slice is the namespace's own buffer and is valid until the
// next Expand.
func (namespace *Namespace) Expand(owner Owner, path []byte, followFinal bool) ([]byte, Status) {
	state := namespace.links
	if state == nil || !namespace.anyLinks() {
		return path, StatusOK
	}
	size, ok := canonicalPath(&state.path, path)
	if !ok {
		return path, StatusOK
	}
	// A rooted session reads no link outside its root, not even one that
	// points back in.
	if !namespace.insideRoot(owner, state.path[:size], false) {
		return nil, StatusPermissionDenied
	}
	for expansions := 0; ; expansions++ {
		namespace.resolve(state.path[:size])
		hit := namespace.hit
		if !hit.valid || hit.final && !followFinal {
			return state.path[:size], StatusOK
		}
		if expansions == MaxLinkExpansions {
			return nil, StatusLoop
		}
		count, status := namespace.readLink(owner, hit)
		if status != StatusOK {
			return nil, status
		}
		target := state.target[:count]
		var base []byte
		if target[0] == '/' {
			base = namespace.rootOf(owner)
		} else {
			base = state.path[:hit.start]
		}
		rest := state.path[hit.end:size]
		if len(base)+len(target)+len(rest) > MaxPath {
			return nil, StatusNameTooLong
		}
		built := copy(state.work[:], base)
		built += copy(state.work[built:], target)
		built += copy(state.work[built:], rest)
		size, ok = canonicalPath(&state.path, state.work[:built])
		if !ok {
			return nil, StatusNameTooLong
		}
		if !namespace.insideRoot(owner, state.path[:size], false) {
			return nil, StatusPermissionDenied
		}
	}
}

// readLink reads the target of the link hit names into links.target, for
// owner, who must be allowed to read the link.
func (namespace *Namespace) readLink(owner Owner, hit linkHit) (int, Status) {
	backend := namespace.mounts[hit.mount].backend
	links, ok := backend.(LinkBackend)
	if !ok {
		return 0, StatusBackendError
	}
	mask, status := namespace.nodeScopes(backend, hit.node)
	if status != StatusOK {
		return 0, status
	}
	if !namespace.permits(owner, mask, Read) {
		return 0, StatusPermissionDenied
	}
	count, backendStatus := links.ReadLink(hit.node, namespace.links.target[:ramfs.MaxLinkTarget])
	if backendStatus != ramfs.StatusOK {
		return 0, mapStatus(backendStatus)
	}
	if count < 0 || count > ramfs.MaxLinkTarget || !ramfs.ValidLinkTarget(namespace.links.target[:count]) {
		return 0, StatusCorrupt
	}
	return count, StatusOK
}

// SyncOpen makes the open file handle durable through the backend that
// serves it, and no other.
func (namespace *Namespace) SyncOpen(owner Owner, handle Handle) Status {
	entry, status := namespace.file(owner, handle, 0)
	if status != StatusOK {
		return status
	}
	syncer, ok := namespace.mounts[entry.mount].backend.(SyncBackend)
	if !ok {
		return StatusUnsupported
	}
	return mapStatus(syncer.SyncFile(entry.node))
}

// SyncPath makes the directory or file path names durable through its own
// backend. Linux fsync of a directory descriptor arrives here.
func (namespace *Namespace) SyncPath(path []byte) Status {
	mountIndex, node, _, status := namespace.resolve(path)
	if status != StatusOK {
		return status
	}
	backend := namespace.mounts[mountIndex].backend
	syncer, ok := backend.(SyncBackend)
	if !ok {
		return StatusUnsupported
	}
	stat, backendStatus := backend.Stat(node)
	switch {
	case backendStatus != ramfs.StatusOK:
		return mapStatus(backendStatus)
	case stat.Kind == ramfs.KindDirectory:
		return mapStatus(syncer.SyncDirectory(node))
	case stat.Kind == ramfs.KindFile:
		return mapStatus(syncer.SyncFile(node))
	}
	return StatusUnsupported
}

// ReadLink returns the target of the symbolic link path names, without
// following it. The caller has expanded path with followFinal unset and
// authorized it. The slice is valid until the next ReadLink or Expand.
func (namespace *Namespace) ReadLink(owner Owner, path []byte) ([]byte, Status) {
	_, _, _, status := namespace.resolve(path)
	hit := namespace.hit
	if status != StatusOK {
		return nil, status
	}
	if !hit.valid || !hit.final {
		return nil, StatusInvalidArgument
	}
	count, status := namespace.readLink(owner, hit)
	if status != StatusOK {
		return nil, status
	}
	return namespace.links.target[:count], StatusOK
}

// Symlink makes path a symbolic link to target. target is stored as given;
// nothing about it is checked but its length and that it holds no NUL, as on
// Linux. A dangling link is a valid link.
func (namespace *Namespace) Symlink(path []byte, target []byte) Status {
	if !ramfs.ValidLinkTarget(target) {
		if len(target) > ramfs.MaxLinkTarget {
			return StatusNameTooLong
		}
		return StatusInvalidArgument
	}
	if namespace.reservedTarget(path) {
		return StatusPermissionDenied
	}
	mountIndex, parent, name, protected, status := namespace.resolveParent(path)
	if status != StatusOK {
		return status
	}
	if protected || namespace.mounts[mountIndex].readOnly {
		return StatusPermissionDenied
	}
	links, ok := namespace.mounts[mountIndex].backend.(LinkBackend)
	if !ok {
		return StatusUnsupported
	}
	_, status = mapNodeResult(links.SymlinkName(parent, name, target))
	namespace.invalidate()
	return status
}

// Link gives the existing node path the further name target. Both must lie
// in one filesystem instance: two mounts of one backend qualify, two
// backends do not. A directory cannot be linked.
func (namespace *Namespace) Link(path []byte, target []byte) Status {
	if namespace.reservedTarget(target) || namespace.changesProtected(path, target) {
		return StatusPermissionDenied
	}
	sourceMount, node, protected, status := namespace.resolve(path)
	if status != StatusOK {
		return status
	}
	if protected {
		return StatusPermissionDenied
	}
	backend := namespace.mounts[sourceMount].backend
	stat, backendStatus := backend.Stat(node)
	if backendStatus != ramfs.StatusOK {
		return mapStatus(backendStatus)
	}
	if stat.Kind == ramfs.KindDirectory {
		return StatusPermissionDenied
	}
	targetMount, parent, name, targetProtected, status := namespace.resolveParent(target)
	if status != StatusOK {
		return status
	}
	if targetProtected {
		return StatusPermissionDenied
	}
	if namespace.mounts[targetMount].backend != backend {
		return StatusCrossDevice
	}
	if namespace.mounts[targetMount].readOnly {
		return StatusPermissionDenied
	}
	links, ok := backend.(HardLinkBackend)
	if !ok {
		return StatusUnsupported
	}
	status = mapStatus(links.LinkName(node, parent, name))
	namespace.invalidate()
	return status
}
