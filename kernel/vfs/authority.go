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
	// MaxRootPath is the longest path a session can be rooted at.
	MaxRootPath = 64
)

// Authority grants access to installed scope slots. Confined sessions have no
// access outside their grants, and overlapping grants intersect. An ordinary
// session still obeys scope denials.
//
// A nonzero RootSize also limits every path the session names to the canonical
// directory in Root and what lies under it, whatever else it is granted. The
// root is a path, not a scope slot: a subtree slot records every node it covers
// and stops at maxScopeWalk, which a user's own directory can exceed, and a slot
// lives as long as the namespace, which a configurable root must not.
type Authority struct {
	Confined bool
	Grants   [MaxScopes]Rights
	Root     [MaxRootPath]byte
	RootSize uint8
}

func (authority Authority) Valid() bool {
	for _, rights := range authority.Grants {
		if rights & ^(Read|Write) != 0 {
			return false
		}
	}
	if int(authority.RootSize) > len(authority.Root) {
		return false
	}
	for _, value := range authority.Root[authority.RootSize:] {
		if value != 0 {
			return false
		}
	}
	return authority.RootSize == 0 || ValidRootPath(authority.Root[:authority.RootSize])
}

// RootedAuthority returns the authority of a session limited to root. The
// session keeps the ordinary treatment of scopes inside it.
func RootedAuthority(root []byte) (Authority, bool) {
	var authority Authority
	if !ValidRootPath(root) {
		return authority, false
	}
	authority.RootSize = uint8(copy(authority.Root[:], root))
	return authority, true
}

// ValidRootPath accepts an absolute path in canonical form, other than "/".
func ValidRootPath(path []byte) bool {
	if len(path) < 2 || len(path) > MaxRootPath || path[0] != '/' || path[len(path)-1] == '/' {
		return false
	}
	start := 1
	for index := 1; index <= len(path); index++ {
		if index < len(path) && path[index] != '/' {
			if path[index] == 0 {
				return false
			}
			continue
		}
		component := path[start:index]
		if len(component) == 0 || len(component) == 1 && component[0] == '.' || len(component) == 2 && component[0] == '.' && component[1] == '.' {
			return false
		}
		start = index + 1
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
	// exact remembers what an exact name resolved to, zero when it was
	// missing, while exactKnown holds. Namespace.invalidate clears exactKnown.
	exact      ramfs.NodeID
	exactKnown bool
}

// scopeMembers remembers the nodes one walk of a subtree scope reached, until
// Namespace.invalidate clears known. It decides membership by node identity, as
// the walk does, so it needs no assumption that a backend's directories form a
// tree: FAT32 mounts do not look for cross-links (ADR 0117). A walk stopped by
// its bound leaves complete false, and a node it did not reach stays undecided.
type scopeMembers struct {
	nodes    [maxScopeWalk]ramfs.NodeID
	count    int
	known    bool
	complete bool
}

// ownerAuthority keeps a session's grants. Its root, if any, is in the same
// entry of Namespace.rooted, and the Root of authority here stays empty.
type ownerAuthority struct {
	owner     Owner
	authority Authority
}

// rootedSessions holds the sessions' roots by authority entry. check is the
// canonical path insideRoot compares; resolve keeps Namespace.requested.
type rootedSessions struct {
	roots [MaxAuthorities][MaxRootPath]byte
	sizes [MaxAuthorities]uint8
	check [MaxPath]byte
}

// setRoot records entry's root, or that it has none.
func (namespace *Namespace) setRoot(entry int, authority *Authority) {
	if authority.RootSize == 0 && namespace.rooted == nil {
		return
	}
	if namespace.rooted == nil {
		namespace.rooted = new(rootedSessions)
	}
	namespace.rooted.sizes[entry] = uint8(copy(namespace.rooted.roots[entry][:], authority.Root[:authority.RootSize]))
	authority.Root, authority.RootSize = [MaxRootPath]byte{}, 0
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
	namespace.invalidate()
	for index := range namespace.files {
		file := &namespace.files[index]
		if namespace.openOn(file, entry.backend) {
			mask, status := namespace.nodeScopes(entry.backend, file.identity)
			if status != StatusOK || mask&(1<<slot) != 0 {
				namespace.release(file)
			}
		}
	}
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
			namespace.setRoot(index, &authority)
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
	namespace.setRoot(free, &authority)
	namespace.authorities[free] = ownerAuthority{owner: owner, authority: authority}
	return StatusOK
}

func (namespace *Namespace) ForgetAuthority(owner Owner) {
	namespace.CloseOwner(owner)
	for index := range namespace.authorities {
		if namespace.authorities[index].owner == owner {
			namespace.authorities[index] = ownerAuthority{}
			if namespace.rooted != nil {
				namespace.rooted.sizes[index] = 0
			}
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

// AuthorityCounters counts the backend work scope checks cost since the
// namespace started. A DIAG build reports them after each directory listing.
type AuthorityCounters struct {
	ReadDirRequests     uint64
	ScopeChecks         uint64
	ScopeContains       uint64
	ScopeOpenName       uint64
	ScopeBackendReadDir uint64
	// BatchRequests counts batched listing calls, and BatchEntries the records
	// they returned.
	BatchRequests uint64
	BatchEntries  uint64
}

func (namespace *Namespace) AuthorityCounters() AuthorityCounters { return namespace.counters }

// scopeWalk is bounded and fails closed when ancestry cannot be established.
// Backend/node identity covers case-insensitive names and directory mounts.
// With members it records every node it reaches and looks for none. Hitting a
// bound answers StatusResourceExhausted, a backend failure any other status.
func (namespace *Namespace) scopeWalk(backend Backend, root, node ramfs.NodeID, members *scopeMembers) (bool, Status) {
	namespace.counters.ScopeContains++
	if members == nil && root == node {
		return true, StatusOK
	}
	stat, result := backend.Stat(root)
	if result != ramfs.StatusOK {
		return false, StatusPermissionDenied
	}
	if stat.Kind != ramfs.KindDirectory {
		return false, StatusOK
	}
	var nodes [maxScopeDepth]ramfs.NodeID
	var cursors [maxScopeDepth]int
	nodes[0] = root
	depth := 0
	for scanned := 0; scanned < maxScopeWalk; scanned++ {
		namespace.counters.ScopeBackendReadDir++
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
		if members != nil {
			members.nodes[members.count] = entry.Stat.ID
			members.count++
		} else if entry.Stat.ID == node {
			return true, StatusOK
		}
		if entry.Stat.Kind == ramfs.KindDirectory {
			if depth+1 == maxScopeDepth {
				return false, StatusResourceExhausted
			}
			depth++
			nodes[depth], cursors[depth] = entry.Stat.ID, 0
		}
	}
	return false, StatusResourceExhausted
}

// subtreeContains answers from one walk of the scope per namespace state. A
// backend failure is not remembered: it denies this check and the next one
// walks again. Nodes the walk reached before stopping still count as members.
func (namespace *Namespace) subtreeContains(slot int, node ramfs.NodeID) (bool, Status) {
	entry := &namespace.scopes[slot]
	if entry.root == node {
		return true, StatusOK
	}
	members := &namespace.scopeMembers[slot]
	if !members.known {
		members.count = 0
		_, status := namespace.scopeWalk(entry.backend, entry.root, 0, members)
		members.known = status == StatusOK || status == StatusResourceExhausted
		members.complete = status == StatusOK
	}
	for _, member := range members.nodes[:members.count] {
		if member == node {
			return true, StatusOK
		}
	}
	if !members.complete {
		return false, StatusPermissionDenied
	}
	return false, StatusOK
}

// exactNode resolves an exact scope once per namespace state, by identity, so
// the backend's own rules decide case folding and short-name aliases. Failures
// other than a missing name are not remembered.
func (namespace *Namespace) exactNode(entry *scope) (ramfs.NodeID, Status) {
	if entry.exactKnown {
		if entry.exact == 0 {
			return 0, StatusNotFound
		}
		return entry.exact, StatusOK
	}
	namespace.counters.ScopeOpenName++
	node, result := entry.backend.OpenName(entry.parent, entry.name)
	status := mapStatus(result)
	if status == StatusNotFound {
		node = 0
	}
	if status == StatusOK || status == StatusNotFound {
		entry.exact, entry.exactKnown = node, true
	}
	return node, status
}

func (namespace *Namespace) nodeScopes(backend Backend, node ramfs.NodeID) (uint8, Status) {
	namespace.counters.ScopeChecks++
	mask := uint8(0)
	for slot := range namespace.scopes {
		entry := &namespace.scopes[slot]
		if entry.backend != backend {
			continue
		}
		matched := false
		if entry.subtree {
			var status Status
			matched, status = namespace.subtreeContains(slot, node)
			if status != StatusOK {
				return 0, StatusPermissionDenied
			}
		} else {
			exact, status := namespace.exactNode(entry)
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

// insideRoot reports whether a session without a root, or one whose root
// contains path, may name it. It compares the canonical path the namespace goes
// on to resolve, so ".." cannot leave the root. A path spelled differently from
// the root, as a case-insensitive filesystem would accept, is refused. A moved
// path must lie strictly below the root: the root itself stays where it is.
func (namespace *Namespace) insideRoot(owner Owner, path []byte, moving bool) bool {
	rooted := namespace.rooted
	if rooted == nil {
		return true
	}
	for index := range namespace.authorities {
		if namespace.authorities[index].owner != owner {
			continue
		}
		size := int(rooted.sizes[index])
		if size == 0 {
			return true
		}
		canonical, ok := canonicalPath(&rooted.check, path)
		if !ok || canonical < size || string(rooted.check[:size]) != string(rooted.roots[index][:size]) {
			return false
		}
		if canonical == size {
			return !moving
		}
		return rooted.check[size] == '/'
	}
	return true
}

// Rooted reports whether the owner's session is limited to a root. Such a
// session cannot describe mounts, which are named outside it.
func (namespace *Namespace) Rooted(owner Owner) bool {
	if namespace.rooted == nil {
		return false
	}
	for index := range namespace.authorities {
		if namespace.authorities[index].owner == owner {
			return namespace.rooted.sizes[index] != 0
		}
	}
	return false
}

// AuthorizePath checks an existing node or a name to be created. Moving also
// refuses directory ancestors of scopes, even for a granted session.
func (namespace *Namespace) AuthorizePath(owner Owner, path []byte, rights Rights, moving bool) Status {
	if rights == 0 || rights & ^(Read|Write) != 0 || owner == 0 {
		return StatusInvalidArgument
	}
	if !namespace.insideRoot(owner, path, moving) {
		return StatusPermissionDenied
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
				carries, result := namespace.scopeWalk(backend, node, anchor, nil)
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
		mask, result = namespace.nameScopes(parentMount, parent, name)
		if result != StatusOK {
			return result
		}
	}
	if !namespace.permits(owner, mask, rights) {
		return StatusPermissionDenied
	}
	return StatusOK
}

func (namespace *Namespace) nameScopes(mount uint8, parent ramfs.NodeID, name ramfs.Name) (uint8, Status) {
	backend := namespace.mounts[mount].backend
	if namespace.mounts[mount].filesystemType == 0 {
		for slot := range namespace.scopes {
			entry := &namespace.scopes[slot]
			if entry.backend == backend && !entry.subtree {
				return 0, StatusPermissionDenied
			}
		}
	}
	mask, status := namespace.nodeScopes(backend, parent)
	if status != StatusOK {
		return 0, status
	}
	for slot := range namespace.scopes {
		entry := &namespace.scopes[slot]
		if !entry.subtree {
			mask &^= 1 << slot
			if entry.backend == backend && entry.parent == parent && namespace.scopeNameEqual(mount, entry.name, name) {
				mask |= 1 << slot
			}
		}
	}
	return mask, StatusOK
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
	namespace.counters.ReadDirRequests++
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

// dirBatchRefill is the free space below which a batch that already holds a
// record stops asking the backend for more. Most names would not fit, and the
// exchange would be wasted.
const dirBatchRefill = 128

// ReadDirBatchFor writes the entries the session can read to target, from
// cursor on. Only permitted records reach target, which a client may be able
// to read, and hidden entries take no room in it. The call scans on until
// target is full, the listing ends, or it has examined maxScopeWalk entries.
// A failure after a record ends the batch early, and the next call reports it.
func (namespace *Namespace) ReadDirBatchFor(owner Owner, path []byte, cursor int, target []byte) (ramfs.DirBatch, Status) {
	namespace.counters.ReadDirRequests++
	namespace.counters.BatchRequests++
	batch := ramfs.DirBatch{Next: cursor}
	if cursor < 0 || len(target) < ramfs.MaxDirRecord {
		return batch, StatusInvalidArgument
	}
	if status := namespace.AuthorizePath(owner, path, Read, false); status != StatusOK {
		return batch, status
	}
	canonical := &namespace.requested
	size, ok := canonicalPath(canonical, path)
	if !ok {
		return batch, StatusInvalidArgument
	}
	mountIndex, node, protected, status := namespace.resolve(canonical[:size])
	if status != StatusOK {
		return batch, status
	}
	scanned := 0
	for batch.Next < mountCursor {
		free := len(target) - batch.Used
		if batch.Count != 0 && free < dirBatchRefill {
			return namespace.batchDone(batch), StatusOK
		}
		fetched, result := namespace.fetchDirBatch(namespace.mounts[mountIndex].backend, node, batch.Next, free)
		if result == ramfs.StatusNotFound {
			batch.Next = mountCursor
			break
		}
		if result != ramfs.StatusOK {
			return namespace.batchFailed(batch, mapStatus(result))
		}
		var record ramfs.DirRecord
		for offset := 0; offset < fetched.Used; {
			length, valid := record.Decode(namespace.dirBatch[offset:fetched.Used])
			if !valid || record.Next <= batch.Next || record.Next >= mountCursor {
				return namespace.batchFailed(batch, StatusBackendError)
			}
			offset += length
			record.Stat.MountID = uint64(namespace.mountID(int(mountIndex)))
			markProtected(&record.Stat, protected)
			var done bool
			if scanned, done, status = namespace.batchRecord(owner, mountIndex, &record, scanned, target, &batch); status != StatusOK {
				return namespace.batchFailed(batch, status)
			}
			if done {
				return namespace.batchDone(batch), StatusOK
			}
		}
		if fetched.End {
			batch.Next = mountCursor
			break
		}
		if fetched.Count == 0 {
			if batch.Count == 0 {
				return batch, StatusBackendError
			}
			return namespace.batchDone(batch), StatusOK
		}
	}
	for {
		next, status := namespace.mountedEntry(canonical[:size], node, mountIndex, batch.Next-mountCursor)
		if status == StatusNotFound {
			batch.Next, batch.End = next, true
			return namespace.batchDone(batch), StatusOK
		}
		if status != StatusOK {
			return namespace.batchFailed(batch, status)
		}
		mount, valid := namespace.mountIndex(MountID(namespace.dirEntry.Stat.MountID))
		if !valid {
			return namespace.batchFailed(batch, StatusPermissionDenied)
		}
		record := ramfs.DirRecord{Stat: namespace.dirEntry.Stat, Next: next, Name: namespace.dirEntry.Name[:namespace.dirEntry.Size]}
		var done bool
		if scanned, done, status = namespace.batchRecord(owner, uint8(mount), &record, scanned, target, &batch); status != StatusOK {
			return namespace.batchFailed(batch, status)
		}
		if done {
			return namespace.batchDone(batch), StatusOK
		}
	}
}

// fetchDirBatch fills the start of dirBatch with up to limit bytes of records,
// from a batching backend or from one entry of any other. The backend's
// not-found status means what ReadDir's does: nothing more to list.
func (namespace *Namespace) fetchDirBatch(backend Backend, node ramfs.NodeID, cursor, limit int) (ramfs.DirBatch, ramfs.Status) {
	if limit > len(namespace.dirBatch) {
		limit = len(namespace.dirBatch)
	}
	if batched, supported := backend.(BatchBackend); supported {
		batch, status := batched.ReadDirBatch(node, cursor, namespace.dirBatch[:limit])
		if status != ramfs.StatusUnsupported {
			return batch, status
		}
	}
	var next int
	var status ramfs.Status
	namespace.dirEntry, next, status = backend.ReadDir(node, cursor)
	if status != ramfs.StatusOK {
		return ramfs.DirBatch{}, status
	}
	written := ramfs.AppendDirRecord(namespace.dirBatch[:limit], &namespace.dirEntry.Stat, next, namespace.dirEntry.Name[:namespace.dirEntry.Size])
	if written == 0 {
		return ramfs.DirBatch{Next: cursor}, ramfs.StatusOK
	}
	return ramfs.DirBatch{Used: written, Count: 1, Next: next}, ramfs.StatusOK
}

// batchRecord applies the session's authority to one entry and, when it may
// read it, writes it to target. done means the batch ends before this entry:
// the examined limit, or no room left. A hidden entry moves the position on so
// the next call does not examine it again.
func (namespace *Namespace) batchRecord(owner Owner, mount uint8, record *ramfs.DirRecord, scanned int, target []byte, batch *ramfs.DirBatch) (int, bool, Status) {
	if scanned == maxScopeWalk {
		if batch.Count == 0 {
			return scanned, false, StatusPermissionDenied
		}
		return scanned, true, StatusOK
	}
	mask, status := namespace.nodeScopes(namespace.mounts[mount].backend, record.Stat.ID)
	if status != StatusOK {
		return scanned, false, status
	}
	if namespace.permits(owner, mask, Read) {
		written := ramfs.AppendDirRecordInode(target[batch.Used:], &record.Stat, record.Inode, record.Next, record.Name)
		if written == 0 {
			if batch.Count == 0 {
				return scanned, false, StatusBackendError
			}
			return scanned, true, StatusOK
		}
		batch.Used += written
		batch.Count++
	}
	batch.Next = record.Next
	return scanned + 1, false, StatusOK
}

func (namespace *Namespace) batchDone(batch ramfs.DirBatch) ramfs.DirBatch {
	namespace.counters.BatchEntries += uint64(batch.Count)
	return batch
}

func (namespace *Namespace) batchFailed(batch ramfs.DirBatch, status Status) (ramfs.DirBatch, Status) {
	if batch.Count != 0 {
		return namespace.batchDone(batch), StatusOK
	}
	return batch, status
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
