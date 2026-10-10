package vfs

import (
	"goos/kernel/ramfs"
	"unicode"
	"unicode/utf8"
)

const (
	MaxScopes      = 8
	MaxAuthorities = 64
	maxScopeWalk   = 256
	maxScopeDepth  = 32
)

// Authority grants access to installed scope slots. Confined sessions have no
// access outside their grants, and overlapping grants intersect. An ordinary
// session still obeys scope denials.
type Authority struct {
	Confined bool
	Grants   [MaxScopes]Rights
}

func (authority Authority) Valid() bool {
	for _, rights := range authority.Grants {
		if rights & ^(Read|Write) != 0 {
			return false
		}
	}
	return true
}

type scope struct {
	backend Backend
	root    ramfs.NodeID
	parent  ramfs.NodeID
	name    ramfs.Name
	denied  Rights
	subtree bool
}

type ownerAuthority struct {
	owner     Owner
	authority Authority
}

// InstallScope binds an unused slot to an existing directory or an exact name
// in an existing parent. Slots cannot be replaced while the namespace lives.
// Installing protection revokes matching old handles, including readers.
func (namespace *Namespace) InstallScope(slot uint8, path []byte, subtree bool, denied Rights) Status {
	if int(slot) >= MaxScopes || denied & ^(Read|Write) != 0 || namespace.scopes[slot].backend != nil {
		return StatusInvalidArgument
	}
	var entry scope
	if subtree {
		mount, node, status := namespace.Resolve(path)
		if status != StatusOK {
			return status
		}
		backend := namespace.mounts[mount].backend
		stat, result := backend.Stat(node)
		if result != ramfs.StatusOK {
			return mapStatus(result)
		}
		if stat.Kind != ramfs.KindDirectory {
			return StatusNotDirectory
		}
		entry.backend, entry.root, entry.subtree, entry.denied = backend, node, true, denied
	} else {
		mount, parent, name, _, status := namespace.resolveParent(path)
		if status != StatusOK {
			return status
		}
		if namespace.mounts[mount].filesystemType == 0 {
			return StatusPermissionDenied
		}
		entry.backend, entry.parent, entry.name, entry.denied = namespace.mounts[mount].backend, parent, name, denied
		stat, result := entry.backend.Stat(parent)
		if result != ramfs.StatusOK {
			return mapStatus(result)
		}
		if stat.Kind != ramfs.KindDirectory {
			return StatusNotDirectory
		}
		if node, result := entry.backend.OpenName(parent, name); result == ramfs.StatusOK {
			stat, result := entry.backend.Stat(node)
			if result != ramfs.StatusOK {
				return mapStatus(result)
			}
			if stat.Kind != ramfs.KindFile {
				return StatusIsDirectory
			}
		} else if result != ramfs.StatusNotFound {
			return mapStatus(result)
		}
	}
	namespace.scopes[slot] = entry
	for index := range namespace.files {
		file := &namespace.files[index]
		if namespace.openOn(file, entry.backend) {
			mask, status := namespace.nodeScopes(entry.backend, file.identity)
			if status != StatusOK || mask&(1<<slot) != 0 {
				namespace.release(file)
			}
		}
	}
	namespace.invalidate()
	return StatusOK
}

func (namespace *Namespace) SetAuthority(owner Owner, authority Authority) Status {
	if owner == 0 || !authority.Valid() {
		return StatusInvalidArgument
	}
	for slot, rights := range authority.Grants {
		if rights != 0 && namespace.scopes[slot].backend == nil {
			return StatusNotFound
		}
	}
	free := -1
	for index := range namespace.authorities {
		entry := &namespace.authorities[index]
		if entry.owner == owner {
			namespace.CloseOwner(owner)
			entry.authority = authority
			return StatusOK
		}
		if entry.owner == 0 && free < 0 {
			free = index
		}
	}
	if free < 0 {
		return StatusResourceExhausted
	}
	namespace.CloseOwner(owner)
	namespace.authorities[free] = ownerAuthority{owner: owner, authority: authority}
	return StatusOK
}

func (namespace *Namespace) ForgetAuthority(owner Owner) {
	namespace.CloseOwner(owner)
	for index := range namespace.authorities {
		if namespace.authorities[index].owner == owner {
			namespace.authorities[index] = ownerAuthority{}
		}
	}
}

func (namespace *Namespace) authority(owner Owner) Authority {
	for index := range namespace.authorities {
		if namespace.authorities[index].owner == owner {
			return namespace.authorities[index].authority
		}
	}
	return Authority{}
}

func (namespace *Namespace) permits(owner Owner, mask uint8, rights Rights) bool {
	if owner == 0 {
		return false
	}
	authority := namespace.authority(owner)
	granted := Rights(0)
	for slot := range namespace.scopes {
		if mask&(1<<slot) == 0 {
			continue
		}
		entry := &namespace.scopes[slot]
		if rights&entry.denied & ^authority.Grants[slot] != 0 || authority.Confined && rights & ^authority.Grants[slot] != 0 {
			return false
		}
		granted |= authority.Grants[slot]
	}
	return !authority.Confined || granted&rights == rights
}

// contains is bounded and fails closed when ancestry cannot be established.
// Backend/node identity covers case-insensitive names and directory mounts.
func scopeContains(backend Backend, root, node ramfs.NodeID) (bool, Status) {
	if root == node {
		return true, StatusOK
	}
	stat, result := backend.Stat(root)
	if result != ramfs.StatusOK {
		return false, mapStatus(result)
	}
	if stat.Kind != ramfs.KindDirectory {
		return false, StatusOK
	}
	var nodes [maxScopeDepth]ramfs.NodeID
	var cursors [maxScopeDepth]int
	nodes[0] = root
	depth := 0
	for scanned := 0; scanned < maxScopeWalk; scanned++ {
		entry, next, result := backend.ReadDir(nodes[depth], cursors[depth])
		if result == ramfs.StatusNotFound {
			if depth == 0 {
				return false, StatusOK
			}
			depth--
			continue
		}
		if result != ramfs.StatusOK || next <= cursors[depth] {
			return false, StatusPermissionDenied
		}
		cursors[depth] = next
		if entry.Stat.ID == node {
			return true, StatusOK
		}
		if entry.Stat.Kind == ramfs.KindDirectory {
			if depth+1 == maxScopeDepth {
				return false, StatusPermissionDenied
			}
			depth++
			nodes[depth], cursors[depth] = entry.Stat.ID, 0
		}
	}
	return false, StatusPermissionDenied
}

func (entry *scope) exactNode() (ramfs.NodeID, Status) {
	node, status := entry.backend.OpenName(entry.parent, entry.name)
	return node, mapStatus(status)
}

func (namespace *Namespace) nodeScopes(backend Backend, node ramfs.NodeID) (uint8, Status) {
	mask := uint8(0)
	for slot := range namespace.scopes {
		entry := &namespace.scopes[slot]
		if entry.backend != backend {
			continue
		}
		matched := false
		if entry.subtree {
			var status Status
			matched, status = scopeContains(backend, entry.root, node)
			if status != StatusOK {
				return 0, StatusPermissionDenied
			}
		} else {
			exact, status := entry.exactNode()
			if status != StatusOK && status != StatusNotFound {
				return 0, StatusPermissionDenied
			}
			matched = status == StatusOK && exact == node
		}
		if matched {
			mask |= 1 << slot
		}
	}
	return mask, StatusOK
}

// AuthorizePath checks an existing node or a name to be created. Moving also
// refuses directory ancestors of scopes, even for a granted session.
func (namespace *Namespace) AuthorizePath(owner Owner, path []byte, rights Rights, moving bool) Status {
	if rights == 0 || rights & ^(Read|Write) != 0 || owner == 0 {
		return StatusInvalidArgument
	}
	if rights&Write != 0 && namespace.reservedTarget(path) {
		return StatusPermissionDenied
	}
	mount, node, status := namespace.Resolve(path)
	var mask uint8
	if status == StatusOK {
		backend := namespace.mounts[mount].backend
		mask, status = namespace.nodeScopes(backend, node)
		if status != StatusOK {
			return status
		}
		if moving {
			for slot := range namespace.scopes {
				entry := &namespace.scopes[slot]
				if entry.backend != backend {
					continue
				}
				anchor := entry.parent
				if entry.subtree {
					anchor = entry.root
				}
				carries, result := scopeContains(backend, node, anchor)
				if result != StatusOK || carries {
					return StatusPermissionDenied
				}
			}
		}
	} else if status != StatusNotFound {
		return status
	}
	if status == StatusNotFound {
		parentMount, parent, name, _, result := namespace.resolveParent(path)
		if result != StatusOK {
			return result
		}
		backend := namespace.mounts[parentMount].backend
		if namespace.mounts[parentMount].filesystemType == 0 {
			for slot := range namespace.scopes {
				entry := &namespace.scopes[slot]
				if entry.backend == backend && !entry.subtree {
					return StatusPermissionDenied
				}
			}
		}
		mask, result = namespace.nodeScopes(backend, parent)
		if result != StatusOK {
			return result
		}
		for slot := range namespace.scopes {
			if !namespace.scopes[slot].subtree {
				mask &^= 1 << slot
			}
		}
		for slot := range namespace.scopes {
			entry := &namespace.scopes[slot]
			if entry.backend != backend || entry.subtree || entry.parent != parent {
				continue
			}
			if namespace.scopeNameEqual(parentMount, entry.name, name) {
				mask |= 1 << slot
			}
		}
	}
	if !namespace.permits(owner, mask, rights) {
		return StatusPermissionDenied
	}
	return StatusOK
}

func (namespace *Namespace) scopeNameEqual(mount uint8, leftName, rightName ramfs.Name) bool {
	if leftName == rightName {
		return true
	}
	if namespace.mounts[mount].filesystemType != ramfs.FilesystemFAT32 {
		return false
	}
	left := leftName.CopyTo(namespace.scopeNames[0][:])
	right := rightName.CopyTo(namespace.scopeNames[1][:])
	first, second := namespace.scopeNames[0][:left], namespace.scopeNames[1][:right]
	for len(first) != 0 && len(second) != 0 {
		a, sizeA := utf8.DecodeRune(first)
		b, sizeB := utf8.DecodeRune(second)
		if a == utf8.RuneError && sizeA == 1 || b == utf8.RuneError && sizeB == 1 || unicode.ToUpper(a) != unicode.ToUpper(b) {
			return false
		}
		first, second = first[sizeA:], second[sizeB:]
	}
	return len(first) == 0 && len(second) == 0
}

// AuthorizeDirectory prevents an exact file scope from becoming a directory.
func (namespace *Namespace) AuthorizeDirectory(owner Owner, path []byte, moving bool) Status {
	if status := namespace.AuthorizePath(owner, path, Write, moving); status != StatusOK {
		return status
	}
	mount, parent, name, _, status := namespace.resolveParent(path)
	if status != StatusOK {
		return status
	}
	backend := namespace.mounts[mount].backend
	for slot := range namespace.scopes {
		entry := &namespace.scopes[slot]
		if entry.backend == backend && !entry.subtree && entry.parent == parent && namespace.scopeNameEqual(mount, entry.name, name) {
			return StatusPermissionDenied
		}
	}
	return StatusOK
}

// ReadDirFor omits entries the session cannot read, including scoped files in
// otherwise ordinary directories and mounted aliases of protected directories.
func (namespace *Namespace) ReadDirFor(owner Owner, path []byte, cursor int) (ramfs.Entry, int, Status) {
	if status := namespace.AuthorizePath(owner, path, Read, false); status != StatusOK {
		return ramfs.Entry{}, cursor, status
	}
	for skipped := 0; skipped < maxScopeWalk; skipped++ {
		entry, next, status := namespace.ReadDir(path, cursor)
		if status != StatusOK {
			return entry, next, status
		}
		mount, valid := namespace.mountIndex(MountID(entry.Stat.MountID))
		if !valid {
			return ramfs.Entry{}, cursor, StatusPermissionDenied
		}
		mask, status := namespace.nodeScopes(namespace.mounts[mount].backend, entry.Stat.ID)
		if status != StatusOK {
			return ramfs.Entry{}, cursor, status
		}
		if namespace.permits(owner, mask, Read) {
			return entry, next, StatusOK
		}
		if next <= cursor {
			return ramfs.Entry{}, cursor, StatusPermissionDenied
		}
		cursor = next
	}
	return ramfs.Entry{}, cursor, StatusPermissionDenied
}

// A backend may move live objects before its final durability step fails.
func (namespace *Namespace) scopeMutationFailed(backend Backend) {
	guarded := false
	for slot := range namespace.scopes {
		guarded = guarded || namespace.scopes[slot].backend == backend
	}
	if !guarded {
		return
	}
	for index := range namespace.files {
		entry := &namespace.files[index]
		if namespace.openOn(entry, backend) {
			namespace.release(entry)
		}
	}
}

func (namespace *Namespace) followScopes(mount uint8, previous ramfs.NodeID, target []byte) {
	backend := namespace.mounts[mount].backend
	_, moved, status := namespace.Resolve(target)
	if status != StatusOK {
		namespace.scopeMutationFailed(backend)
		return
	}
	for index := range namespace.files {
		entry := &namespace.files[index]
		if !namespace.openOn(entry, backend) {
			continue
		}
		if entry.identity == previous {
			entry.identity = moved
		}
		mask, status := namespace.nodeScopes(backend, entry.identity)
		if status != StatusOK {
			namespace.release(entry)
		} else {
			entry.scopes = mask
		}
	}
}
