package vfs

import (
	"goos/internal/limits"
	"goos/kernel/ramfs"
)

const (
	// MaxMounts holds the eight mounts of the boot archive and the
	// persistent volume and one for each additional volume (ADR 0222).
	MaxMounts = 16
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
	// StatusLoop is a path that met more than MaxLinkExpansions symbolic
	// links, or an open that refused to follow one (ADR 0223).
	StatusLoop
	// StatusCrossDevice refuses a hard link between two filesystem
	// instances.
	StatusCrossDevice
	lastStatus = StatusCrossDevice
)

type Rights uint8

const (
	Read Rights = 1 << iota
	Write
)

// OpenFlags carry the namespace mutations and kind constraints of one open.
// NoFollow remains a right because path expansion also uses it.
type OpenFlags uint8

const (
	OpenCreate OpenFlags = 1 << iota
	OpenExclusive
	OpenTruncate
	OpenDirectory
	OpenTrailingSlash
	openFlagsMask = OpenCreate | OpenExclusive | OpenTruncate | OpenDirectory | OpenTrailingSlash
)

// Valid reports whether flags are a supported explicit open intent.
func (flags OpenFlags) Valid() bool {
	return flags&^openFlagsMask == 0 && (flags&OpenExclusive == 0 || flags&OpenCreate != 0)
}

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

// BatchBackend writes many directory records in one call. A backend without
// it, or one answering StatusUnsupported, lists one entry at a time. End and a
// short batch keep the meaning ramfs.DirBatch gives them.
type BatchBackend interface {
	ReadDirBatch(parent ramfs.NodeID, cursor int, target []byte) (ramfs.DirBatch, ramfs.Status)
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

// EntryBackend renames and replaces a name its caller spells out, for a
// filesystem whose node IDs record no name: an ext4 inode with two hard
// links has two names, and only the path says which one moves. The VFS
// prefers it to RenameName and ReplaceName; a backend answering
// StatusUnsupported gets those instead.
type EntryBackend interface {
	RenameEntry(sourceParent ramfs.NodeID, sourceName ramfs.Name, id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status
	ReplaceEntry(sourceParent ramfs.NodeID, sourceName ramfs.Name, id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status
}

// MetadataBackend changes a node's modification time and a file's read-only
// attribute. A backend without it answers SetMetadata with Unsupported.
type MetadataBackend interface {
	SetMetadata(id ramfs.NodeID, change ramfs.MetadataChange) ramfs.Status
}

// LinkBackend holds symbolic links. ReadLink copies a link's target and
// returns its length; SymlinkName makes a link. The VFS follows links itself
// and never asks a backend to (ADR 0223).
type LinkBackend interface {
	ReadLink(id ramfs.NodeID, target []byte) (int, ramfs.Status)
	SymlinkName(parent ramfs.NodeID, name ramfs.Name, target []byte) (ramfs.NodeID, ramfs.Status)
}

// HardLinkBackend gives the existing node id a further name in parent. A
// backend without it answers Link with StatusUnsupported.
type HardLinkBackend interface {
	LinkName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status
}

// SyncBackend makes a backend's completed changes durable. SyncFile covers
// a file's data and the metadata needed to find it, SyncDirectory a
// directory's entries, and SyncFilesystem everything the instance holds. A
// sync reaches only the backend that owns the object, never another
// filesystem. A backend without this interface answers a sync with
// StatusUnsupported rather than claim durability it does not have.
type SyncBackend interface {
	SyncFile(id ramfs.NodeID) ramfs.Status
	SyncDirectory(id ramfs.NodeID) ramfs.Status
	SyncFilesystem() ramfs.Status
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
	// links reports a backend that can hold symbolic links, so a walk
	// through it checks the kind of every node it reaches.
	links bool
	used  bool
}

// MountInfo is one bounded mount-table entry.
type MountInfo struct {
	ID         MountID
	Path       [MaxPath]byte
	PathSize   uint16
	Filesystem ramfs.FilesystemInfo
}

type openFile struct {
	node ramfs.NodeID
	// identity is the node the path named when the file was opened. A
	// backend with open objects hands out a cookie for node, which differs
	// from what a stat of the path reports; identity is the value both
	// agree on. A rename moves it with the name.
	identity   ramfs.NodeID
	offset     uint64
	readToken  uint64
	generation uint32
	owner      Owner
	mount      uint8
	rights     Rights
	used       bool
	executable bool
	// protected marks a file opened below the protected directory.
	protected bool
	scopes    uint8
}

type cacheEntry struct {
	node      ramfs.NodeID
	mount     uint8
	path      [maxCachedPath]byte
	size      uint16
	protected bool
	// link marks a path whose last component is a symbolic link. A walk
	// that meets a link earlier stops there and caches nothing.
	link bool
	used bool
}

type Namespace struct {
	mounts [MaxMounts]mount
	files  [MaxOpenFiles]openFile
	cache  [MaxCache]cacheEntry
	// nextRead identifies a cursor reservation until its owner finishes it.
	nextRead uint64
	// Canonical paths live here because a local [MaxPath]byte escapes into the
	// helpers below and the kernel's leaking heap never frees it. Resolve owns
	// resolved; the operations that canonicalize a path before calling Resolve
	// own requested.
	resolved  [MaxPath]byte
	requested [MaxPath]byte
	// rooted exists once a session has a root. Native images carry this
	// structure, so what most of them never use stays out of it.
	rooted *rootedSessions
	// guard and guarded name the one protected directory: a node of one
	// backend, below which no path may be changed (ADR 0175). The check is
	// made on the node a walk reaches, not on the path that reached it, so
	// every name of the directory is covered: another mount of the same
	// volume, a name that differs only in case on a filesystem that ignores
	// case, and a mount of the directory itself.
	guard   Backend
	guarded ramfs.NodeID
	// reserved are mount points nothing may be created at or below while
	// they are not mounted, so a client cannot build a directory in the
	// volatile root that a path meant for a mount would reach instead.
	reserved    [maxReserved]reservedPath
	scopes      [MaxScopes]scope
	authorities [MaxAuthorities]ownerAuthority
	scopeNames  [2][ramfs.MaxName]byte
	counters    AuthorityCounters
	// dirBatch holds records a backend returned until each one's authority is
	// checked, and dirEntry an entry that arrived alone. A scope check can
	// reach the backend, so neither may be shared memory or a stack value the
	// check could outlive.
	dirBatch [MaxDirBatch]byte
	dirEntry ramfs.Entry
	// scopeMembers answer repeated subtree checks without walking the backend,
	// one per scope slot. Every change to a name, mount, or scope clears them
	// through invalidate.
	scopeMembers [MaxScopes]scopeMembers
	// hit is the symbolic link the last resolve met, and links the buffers
	// expansion builds paths in. links is allocated when a mount that can
	// hold links first appears.
	hit   linkHit
	links *linkState
}

const maxReserved = 4

type reservedPath struct {
	path [maxReservedPath]byte
	size uint8
}

const maxReservedPath = 32

// Reserve keeps the canonical path free for a mount: until a filesystem is
// mounted there, creating it or anything below it, or moving a name there, is
// refused.
func (namespace *Namespace) Reserve(path []byte) Status {
	canonical := &namespace.requested
	size, ok := canonicalPath(canonical, path)
	if !ok || size == 1 || size > maxReservedPath {
		return StatusInvalidArgument
	}
	for index := range namespace.reserved {
		entry := &namespace.reserved[index]
		if entry.size == 0 {
			entry.size = uint8(copy(entry.path[:], canonical[:size]))
			return StatusOK
		}
	}
	return StatusResourceExhausted
}

// reservedTarget reports whether path is at or below a reserved mount point
// that nothing is mounted at.
func (namespace *Namespace) reservedTarget(path []byte) bool {
	canonical := &namespace.requested
	size, ok := canonicalPath(canonical, path)
	if !ok {
		return false
	}
	for index := range namespace.reserved {
		entry := &namespace.reserved[index]
		if entry.size == 0 || !mountMatches(canonical[:size], entry.path[:entry.size]) {
			continue
		}
		mounted := false
		for mountIndex := range namespace.mounts {
			if point := &namespace.mounts[mountIndex]; point.used && equal(point.path[:point.size], entry.path[:entry.size]) {
				mounted = true
			}
		}
		if !mounted {
			return true
		}
	}
	return false
}

// Protect makes the directory node of backend and everything below it
// immutable through this namespace: no create, writable open, truncate,
// unlink, rename, or replace reaches it. It refuses while a file of backend is
// open for writing, because that handle was granted before the protection and
// could still write below the directory. Only one directory is protected at a
// time; protecting another replaces it.
func (namespace *Namespace) Protect(backend Backend, node ramfs.NodeID) Status {
	if backend == nil || node == 0 {
		return StatusInvalidArgument
	}
	for index := range namespace.files {
		file := &namespace.files[index]
		if file.used && file.rights&Write != 0 && namespace.mounts[file.mount].backend == backend {
			return StatusBusy
		}
	}
	namespace.guard, namespace.guarded = backend, node
	namespace.invalidate()
	return StatusOK
}

// Unprotect lifts the protection Protect set, if any.
func (namespace *Namespace) Unprotect() {
	namespace.guard, namespace.guarded = nil, 0
	namespace.invalidate()
}

// Protected returns the protected directory of backend, if it has one.
func (namespace *Namespace) Protected(backend Backend) (ramfs.NodeID, bool) {
	if backend == nil || namespace.guard != backend {
		return 0, false
	}
	return namespace.guarded, true
}

// protectedMount reports whether the mount in slot index is of the protected
// directory itself, which is then reported read-only.
func (namespace *Namespace) protectedMount(index int) bool {
	entry := &namespace.mounts[index]
	return entry.used && namespace.guards(entry, entry.root)
}

// guards reports whether node of mount is the protected directory.
func (namespace *Namespace) guards(mount *mount, node ramfs.NodeID) bool {
	return namespace.guard != nil && mount.backend == namespace.guard && node == namespace.guarded
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
			entry.filesystemType, entry.readOnly = 0, false
			if filesystem, supported := backend.(filesystemBackend); supported {
				if info, status := filesystem.FilesystemInfo(); status == ramfs.StatusOK && info.Valid() && info.Type != 0 {
					entry.filesystemType, entry.readOnly = info.Type, info.ReadOnly
				}
			}
			entry.links = holdsLinks(backend, entry.filesystemType)
			if entry.links && namespace.links == nil {
				namespace.links = new(linkState)
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
// BackendInUse reports whether UnmountBackend would refuse backend:
// StatusNotFound when nothing mounts it, StatusBusy while a file or a scope
// holds it, StatusOK otherwise.
func (namespace *Namespace) BackendInUse(backend Backend) Status {
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
	for index := range namespace.scopes {
		if namespace.scopes[index].backend == backend {
			return StatusBusy
		}
	}
	return StatusOK
}

func (namespace *Namespace) UnmountBackend(backend Backend) Status {
	if backend == nil {
		return StatusInvalidArgument
	}
	if status := namespace.BackendInUse(backend); status != StatusOK {
		return status
	}
	if namespace.guard == backend {
		namespace.guard, namespace.guarded = nil, 0
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

// UnmountPath detaches the one mount at path. It refuses while an open file
// was opened through that mount, and never detaches the root.
func (namespace *Namespace) UnmountPath(path []byte) Status {
	canonical := &namespace.requested
	size, ok := canonicalPath(canonical, path)
	if !ok || size == 1 {
		return StatusInvalidArgument
	}
	for index := range namespace.mounts {
		entry := &namespace.mounts[index]
		if !entry.used || !equal(entry.path[:entry.size], canonical[:size]) {
			continue
		}
		for fileIndex := range namespace.files {
			if file := &namespace.files[fileIndex]; file.used && int(file.mount) == index {
				return StatusBusy
			}
		}
		generation := entry.generation + 1
		if generation == 0 {
			generation = 1
		}
		*entry = mount{generation: generation}
		namespace.invalidate()
		return StatusOK
	}
	return StatusNotFound
}

func (namespace *Namespace) Resolve(path []byte) (uint8, ramfs.NodeID, Status) {
	mountIndex, node, _, status := namespace.resolve(path)
	return mountIndex, node, status
}

// resolve walks path to its node and reports whether the walk reached the
// protected directory, so the node is it or lies below it.
func (namespace *Namespace) resolve(path []byte) (uint8, ramfs.NodeID, bool, Status) {
	canonical := &namespace.resolved
	namespace.hit = linkHit{}
	size, ok := canonicalPath(canonical, path)
	if !ok {
		return 0, 0, false, StatusInvalidArgument
	}
	if mountIndex, node, protected, link, found := namespace.cached(canonical[:size]); found {
		if link {
			namespace.hit = linkHit{mount: mountIndex, node: node, start: lastComponent(canonical[:size]), end: size, final: true, valid: true}
		}
		return mountIndex, node, protected, StatusOK
	}
	mountIndex, relative, status := namespace.selectMount(canonical[:size])
	if status != StatusOK {
		return 0, 0, false, status
	}
	mounted := &namespace.mounts[mountIndex]
	node := mounted.root
	protected := namespace.guards(mounted, node)
	for relative < size {
		component, rest := nextComponent(canonical[relative:size])
		name, valid := ramfs.MakeName(component)
		if !valid {
			return 0, 0, false, StatusInvalidArgument
		}
		var backendStatus ramfs.Status
		node, backendStatus = mounted.backend.OpenName(node, name)
		if backendStatus != ramfs.StatusOK {
			return 0, 0, false, mapStatus(backendStatus)
		}
		protected = protected || namespace.guards(mounted, node)
		start := relative
		relative = size - len(rest)
		if mounted.links {
			stat, statStatus := mounted.backend.Stat(node)
			if statStatus != ramfs.StatusOK {
				return 0, 0, false, mapStatus(statStatus)
			}
			if stat.Kind == ramfs.KindSymlink {
				final := relative >= size
				namespace.hit = linkHit{mount: mountIndex, node: node, start: start, end: start + len(component), final: final, valid: true}
				if !final {
					// The link is not a directory. A caller that did not
					// expand the path gets the backend's answer for that.
					return 0, 0, false, StatusNotDirectory
				}
			}
		}
	}
	namespace.remember(canonical[:size], mountIndex, node, protected, namespace.hit.valid)
	return mountIndex, node, protected, StatusOK
}

func (namespace *Namespace) Open(owner Owner, path []byte, rights Rights) (Handle, Status) {
	handle, _, status := namespace.OpenIntent(owner, path, rights, 0)
	return handle, status
}

// OpenIntent resolves and optionally creates or truncates one regular file,
// acquires its stable object, and installs the handle without another path
// lookup. A directory is reported by kind and receives no VFS handle.
func (namespace *Namespace) OpenIntent(owner Owner, path []byte, rights Rights, flags OpenFlags) (Handle, ramfs.Kind, Status) {
	if owner == 0 || rights == 0 || rights&^(Read|Write) != 0 || !flags.Valid() || flags&(OpenCreate|OpenDirectory) == OpenCreate|OpenDirectory {
		return 0, 0, StatusInvalidArgument
	}
	if !namespace.insideRoot(owner, path, false) {
		return 0, 0, StatusPermissionDenied
	}
	mountIndex, node, protected, status := namespace.resolve(path)
	if status == StatusNotFound {
		if flags&OpenCreate == 0 {
			return 0, 0, StatusNotFound
		}
		if flags&OpenTrailingSlash != 0 {
			return 0, 0, StatusIsDirectory
		}
		if namespace.reservedTarget(path) {
			return 0, 0, StatusPermissionDenied
		}
		free, reserveStatus := namespace.reserveOpen(owner)
		if reserveStatus != StatusOK {
			return 0, 0, reserveStatus
		}
		mountIndex, parent, name, parentProtected, parentStatus := namespace.resolveParent(path)
		if parentStatus != StatusOK {
			return 0, 0, parentStatus
		}
		backend := namespace.mounts[mountIndex].backend
		scopes, permission := namespace.nameScopes(mountIndex, parent, name)
		if parentProtected || permission != StatusOK || !namespace.permits(owner, scopes, rights|Write) {
			return 0, 0, StatusPermissionDenied
		}
		var backendStatus ramfs.Status
		node, backendStatus = backend.CreateName(parent, name)
		namespace.invalidate()
		if backendStatus != ramfs.StatusOK || node == 0 {
			if backendStatus == ramfs.StatusOK {
				backendStatus = ramfs.StatusIOError
			}
			return 0, 0, mapStatus(backendStatus)
		}
		protected = parentProtected || namespace.guards(&namespace.mounts[mountIndex], node)
		return namespace.finishOpen(free, owner, mountIndex, node, protected, rights, flags, true, scopes)
	}
	if status != StatusOK {
		return 0, 0, status
	}
	if flags&(OpenCreate|OpenExclusive) == OpenCreate|OpenExclusive {
		return 0, 0, StatusAlreadyExists
	}
	backend := namespace.mounts[mountIndex].backend
	stat, backendStatus := backend.Stat(node)
	if backendStatus != ramfs.StatusOK {
		return 0, 0, mapStatus(backendStatus)
	}
	switch stat.Kind {
	case ramfs.KindDirectory:
		if rights&Write != 0 || flags&(OpenCreate|OpenTruncate) != 0 {
			return 0, stat.Kind, StatusIsDirectory
		}
		scopes, permission := namespace.nodeScopes(backend, node)
		if permission != StatusOK || !namespace.permits(owner, scopes, rights) {
			return 0, stat.Kind, StatusPermissionDenied
		}
		return 0, stat.Kind, StatusOK
	case ramfs.KindSymlink:
		return 0, stat.Kind, StatusLoop
	case ramfs.KindSpecial:
		return 0, stat.Kind, StatusUnsupported
	case ramfs.KindFile:
	default:
		return 0, stat.Kind, StatusBackendError
	}
	if flags&(OpenDirectory|OpenTrailingSlash) != 0 {
		return 0, stat.Kind, StatusNotDirectory
	}
	if flags&OpenTruncate != 0 && namespace.reservedTarget(path) {
		return 0, stat.Kind, StatusPermissionDenied
	}
	free, reserveStatus := namespace.reserveOpen(owner)
	if reserveStatus != StatusOK {
		return 0, stat.Kind, reserveStatus
	}
	scopes, permission := namespace.nodeScopes(backend, node)
	if permission != StatusOK {
		return 0, stat.Kind, StatusPermissionDenied
	}
	return namespace.finishOpen(free, owner, mountIndex, node, protected, rights, flags, false, scopes)
}

func (namespace *Namespace) reserveOpen(owner Owner) (int, Status) {
	if namespace.OpenBy(owner) >= MaxOpenPerOwner {
		return -1, StatusResourceExhausted
	}
	for index := range namespace.files {
		if !namespace.files[index].used {
			return index, StatusOK
		}
	}
	return -1, StatusResourceExhausted
}

func (namespace *Namespace) finishOpen(free int, owner Owner, mountIndex uint8, identity ramfs.NodeID, protected bool, rights Rights, flags OpenFlags, created bool, scopes uint8) (Handle, ramfs.Kind, Status) {
	backend := namespace.mounts[mountIndex].backend
	if !namespace.permits(owner, scopes, rights) ||
		!created && flags&OpenTruncate != 0 && !namespace.permits(owner, scopes, Write) {
		return 0, ramfs.KindFile, StatusPermissionDenied
	}
	if protected && (rights&Write != 0 || flags&OpenTruncate != 0) {
		return 0, ramfs.KindFile, StatusPermissionDenied
	}
	object := identity
	objects, stable := backend.(ObjectBackend)
	if stable {
		var backendStatus ramfs.Status
		object, backendStatus = objects.AcquireObject(identity)
		if backendStatus != ramfs.StatusOK || object == 0 {
			if backendStatus == ramfs.StatusOK {
				backendStatus = ramfs.StatusIOError
			}
			return 0, ramfs.KindFile, mapStatus(backendStatus)
		}
	}
	if !created && flags&OpenTruncate != 0 {
		if namespace.heldDirect(mountIndex, object, true) {
			if stable {
				objects.ReleaseObject(object)
			}
			return 0, ramfs.KindFile, StatusPermissionDenied
		}
		if backendStatus := backend.Truncate(identity, 0); backendStatus != ramfs.StatusOK {
			if stable {
				objects.ReleaseObject(object)
			}
			return 0, ramfs.KindFile, mapStatus(backendStatus)
		}
	}
	entry := &namespace.files[free]
	if entry.generation == 0 {
		entry.generation = 1
	}
	entry.node, entry.mount, entry.rights, entry.owner, entry.used = object, mountIndex, rights, owner, true
	entry.identity, entry.protected, entry.scopes = identity, protected, scopes
	return encodeHandle(free, entry.generation), ramfs.KindFile, StatusOK
}

func (namespace *Namespace) Close(owner Owner, handle Handle) Status {
	entry, status := namespace.file(owner, handle, 0)
	if status != StatusOK {
		return status
	}
	return namespace.release(entry)
}

// ReacquireObjects gives every file open on backend an object from a
// filesystem service that has restarted. The objects the old instance handed
// out died with it; identity, where each file's name is now, is what the new
// instance is asked for. All old objects are released before any new one is
// acquired, so an old cookie that happens to equal a new one cannot release
// it. A file whose identity the new instance does not resolve keeps no
// cookie: the new instance numbers its objects afresh, and an old cookie
// could name another file's new object. It returns how many files got no
// object; they answer not found.
func (namespace *Namespace) ReacquireObjects(backend Backend) int {
	objects, ok := backend.(ObjectBackend)
	if !ok {
		return 0
	}
	for index := range namespace.files {
		if file := &namespace.files[index]; namespace.openOn(file, backend) {
			// The new instance knows no such object; the call only drops
			// what this side cached for it.
			objects.ReleaseObject(file.node)
		}
	}
	lost := 0
	for index := range namespace.files {
		file := &namespace.files[index]
		if !namespace.openOn(file, backend) {
			continue
		}
		file.node = 0
		node, status := objects.AcquireObject(file.identity)
		if status != ramfs.StatusOK || node == 0 {
			lost++
			continue
		}
		file.node = node
	}
	return lost
}

// InvalidateBackendObjects revokes open handles without contacting a failed backend.
func (namespace *Namespace) InvalidateBackendObjects(backend Backend) {
	for index := range namespace.files {
		if file := &namespace.files[index]; namespace.openOn(file, backend) {
			file.release()
		}
	}
}

func (namespace *Namespace) openOn(file *openFile, backend Backend) bool {
	return file.used && int(file.mount) < len(namespace.mounts) && namespace.mounts[file.mount].used &&
		namespace.mounts[file.mount].backend == backend
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
	plan, status := namespace.BeginRead(owner, handle)
	if status != StatusOK {
		return 0, status
	}
	count, status := plan.Read(target)
	return count, namespace.FinishRead(plan, count, status)
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
		if entry.used && (entry.executable || !executableOnly) && namespace.sameFilesystem(entry.mount, mount) && entry.node == node {
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
	if namespace.reservedTarget(path) {
		return StatusPermissionDenied
	}
	mountIndex, parent, name, protected, status := namespace.resolveParent(path)
	if status != StatusOK {
		return status
	}
	if protected {
		return StatusPermissionDenied
	}
	if directory {
		_, status = mapNodeResult(namespace.mounts[mountIndex].backend.MkdirName(parent, name))
	} else {
		_, status = mapNodeResult(namespace.mounts[mountIndex].backend.CreateName(parent, name))
	}
	// A backend can leave the name behind when its durability step fails.
	namespace.invalidate()
	return status
}

func (namespace *Namespace) Stat(path []byte) (ramfs.Stat, Status) {
	mountIndex, node, protected, status := namespace.resolve(path)
	if status != StatusOK {
		return ramfs.Stat{}, status
	}
	stat, backendStatus := namespace.mounts[mountIndex].backend.Stat(node)
	if backendStatus == ramfs.StatusOK {
		stat.MountID = uint64(namespace.mountID(int(mountIndex)))
		markProtected(&stat, protected)
	}
	return stat, mapStatus(backendStatus)
}

// markProtected reports a protected node as read-only, as a read-only
// filesystem's are. It describes the protection; it does not enforce it.
func markProtected(stat *ramfs.Stat, protected bool) {
	if protected {
		stat.Attributes |= ramfs.AttrReadOnly
		stat.AttributesAvailable = true
	}
}

// StatOpen describes the file an open handle names. It follows the handle,
// not a path, so the answer stays right after the file is renamed, and a
// client needs no path to learn how long its open file is. Its ID is the one
// a stat of the path gave when the file was opened.
func (namespace *Namespace) StatOpen(owner Owner, handle Handle) (ramfs.Stat, Status) {
	entry, status := namespace.file(owner, handle, 0)
	if status != StatusOK {
		return ramfs.Stat{}, status
	}
	if !namespace.permits(owner, entry.scopes, Read) {
		return ramfs.Stat{}, StatusPermissionDenied
	}
	stat, backendStatus := namespace.mounts[entry.mount].backend.Stat(entry.node)
	if backendStatus == ramfs.StatusOK {
		stat.MountID = uint64(namespace.mountID(int(entry.mount)))
		stat.ID = entry.identity
		markProtected(&stat, entry.protected)
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
	mountIndex, node, protected, status := namespace.resolve(canonical[:size])
	if status != StatusOK {
		return ramfs.Entry{}, cursor, status
	}
	if cursor < mountCursor {
		entry, next, backendStatus := namespace.mounts[mountIndex].backend.ReadDir(node, cursor)
		if backendStatus != ramfs.StatusNotFound {
			if backendStatus == ramfs.StatusOK {
				entry.Stat.MountID = uint64(namespace.mountID(int(mountIndex)))
				markProtected(&entry.Stat, protected)
			}
			return entry, next, mapStatus(backendStatus)
		}
		cursor = mountCursor
	}
	next, status := namespace.mountedEntry(canonical[:size], node, mountIndex, cursor-mountCursor)
	if status != StatusOK {
		return ramfs.Entry{}, next, status
	}
	return namespace.dirEntry, next, StatusOK
}

// mountedEntry finds the next filesystem mounted directly on the directory
// named by path, starting at index, and leaves it in dirEntry. A mount whose
// name the owning backend already listed is skipped, because resolution prefers
// the mount and the name must name one entry rather than two.
func (namespace *Namespace) mountedEntry(path []byte, node ramfs.NodeID, owner uint8, index int) (int, Status) {
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
		stat.MountID = uint64(namespace.mountID(index))
		namespace.dirEntry.Stat = stat
		namespace.dirEntry.Size = uint16(copy(namespace.dirEntry.Name[:], component))
		return mountCursor + index + 1, StatusOK
	}
	return mountCursor + len(namespace.mounts), StatusNotFound
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
	info.ReadOnly = info.ReadOnly || namespace.protectedMount(index)
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
		info = ramfs.FilesystemInfo{Type: entry.filesystemType, ReadOnly: entry.readOnly || namespace.protectedMount(index)}
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
	mountIndex, node, protected, status := namespace.resolve(path)
	if status != StatusOK {
		return status
	}
	if protected {
		return StatusPermissionDenied
	}
	if namespace.executablePinned(mountIndex, node) {
		return StatusPermissionDenied
	}
	return mapStatus(namespace.mounts[mountIndex].backend.Truncate(node, size))
}

// SetMetadata applies change to the node at path. A protected node and a
// pinned executable cannot change, as they cannot be truncated.
func (namespace *Namespace) SetMetadata(path []byte, change ramfs.MetadataChange) Status {
	mountIndex, node, protected, status := namespace.resolve(path)
	if status != StatusOK {
		return status
	}
	return namespace.setMetadata(mountIndex, node, protected, change)
}

// SetMetadataOpen applies change to an open file.
func (namespace *Namespace) SetMetadataOpen(owner Owner, handle Handle, change ramfs.MetadataChange) Status {
	entry, status := namespace.file(owner, handle, 0)
	if status != StatusOK {
		return status
	}
	if !namespace.permits(owner, entry.scopes, Write) {
		return StatusPermissionDenied
	}
	return namespace.setMetadata(entry.mount, entry.node, entry.protected, change)
}

func (namespace *Namespace) setMetadata(mountIndex uint8, node ramfs.NodeID, protected bool, change ramfs.MetadataChange) Status {
	if !change.Valid() {
		return StatusInvalidArgument
	}
	if protected || namespace.executablePinned(mountIndex, node) {
		return StatusPermissionDenied
	}
	backend, ok := namespace.mounts[mountIndex].backend.(MetadataBackend)
	if !ok {
		return StatusUnsupported
	}
	return mapStatus(backend.SetMetadata(node, change))
}

func (namespace *Namespace) Unlink(path []byte) Status {
	if namespace.carriesMount(path) {
		return StatusBusy
	}
	if mount, node, protected, resolved := namespace.resolve(path); resolved == StatusOK && (protected || namespace.executablePinned(mount, node)) {
		return StatusPermissionDenied
	}
	mountIndex, parent, name, protected, status := namespace.resolveParent(path)
	if status != StatusOK {
		return status
	}
	if protected {
		return StatusPermissionDenied
	}
	status = mapStatus(namespace.mounts[mountIndex].backend.UnlinkName(parent, name))
	namespace.invalidate()
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
	if namespace.reservedTarget(target) || namespace.changesProtected(path, target) {
		return StatusPermissionDenied
	}
	sourceMount, node, status := namespace.Resolve(path)
	if status != StatusOK {
		return status
	}
	targetMount, parent, name, _, status := namespace.resolveParent(target)
	if status != StatusOK {
		return status
	}
	if !namespace.sameFilesystem(sourceMount, targetMount) {
		// A rename moves a name within one filesystem; moving between two is
		// a copy, which is the caller's to make. Two mounts of one backend,
		// such as /home and /persist/home, are one filesystem.
		return StatusCrossDevice
	}
	status = mapStatus(namespace.renameBackend(sourceMount, path, node, parent, name, false))
	namespace.invalidate()
	if status == StatusOK {
		namespace.follow(sourceMount, node, target)
	} else {
		namespace.scopeMutationFailed(namespace.mounts[sourceMount].backend)
	}
	return status
}

// renameBackend moves the entry path names, node, to name in parent. A
// backend with EntryBackend gets the source's parent and name as well; the
// source must then sit in the mount it resolved in, which a mount root does
// not. Replace falls back to ReplaceName itself on StatusUnsupported.
func (namespace *Namespace) renameBackend(sourceMount uint8, path []byte, node, parent ramfs.NodeID, name ramfs.Name, replace bool) ramfs.Status {
	backend := namespace.mounts[sourceMount].backend
	entries, ok := backend.(EntryBackend)
	if ok {
		if from, sourceParent, sourceName, _, status := namespace.resolveParent(path); status == StatusOK && namespace.sameFilesystem(from, sourceMount) {
			var result ramfs.Status
			if replace {
				result = entries.ReplaceEntry(sourceParent, sourceName, node, parent, name)
			} else {
				result = entries.RenameEntry(sourceParent, sourceName, node, parent, name)
			}
			if result != ramfs.StatusUnsupported {
				return result
			}
		}
	}
	if replace {
		return ramfs.StatusUnsupported
	}
	return backend.RenameName(node, parent, name)
}

// follow gives files opened under a name that just moved the identity of the
// new name. A backend whose node IDs are directory locations changes the ID a
// stat of the path reports when the entry moves; an open file has to change
// with it, or a stat by handle and a stat by path would stop agreeing.
func (namespace *Namespace) follow(mountIndex uint8, previous ramfs.NodeID, target []byte) {
	namespace.followScopes(mountIndex, previous, target)
	held := false
	for index := range namespace.files {
		entry := &namespace.files[index]
		if entry.used && namespace.sameFilesystem(entry.mount, mountIndex) && entry.identity == previous {
			held = true
			break
		}
	}
	if !held {
		return
	}
	targetMount, moved, status := namespace.Resolve(target)
	if status != StatusOK || !namespace.sameFilesystem(targetMount, mountIndex) || moved == previous {
		return
	}
	for index := range namespace.files {
		entry := &namespace.files[index]
		if entry.used && namespace.sameFilesystem(entry.mount, mountIndex) && entry.identity == previous {
			entry.identity = moved
		}
	}
}

// sameFilesystem reports whether two mounts are served by one backend
// instance, whose node IDs mean the same thing under either mount.
func (namespace *Namespace) sameFilesystem(first, second uint8) bool {
	return int(first) < len(namespace.mounts) && int(second) < len(namespace.mounts) &&
		namespace.mounts[first].backend == namespace.mounts[second].backend
}

// Replace moves the regular file path onto target within one filesystem
// (ADR 0129). An existing target must be a regular file that no session holds
// open; it is removed as path takes its name, and the backend decides what an
// interruption can leave. With no target this is a rename.
func (namespace *Namespace) Replace(path []byte, target []byte) Status {
	if namespace.reservedTarget(target) || namespace.changesProtected(path, target) {
		return StatusPermissionDenied
	}
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
	if !namespace.sameFilesystem(sourceMount, targetMount) {
		return StatusCrossDevice
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
	_, parent, name, _, status := namespace.resolveParent(target)
	if status != StatusOK {
		return status
	}
	var backendStatus ramfs.Status
	if backendStatus = namespace.renameBackend(sourceMount, path, node, parent, name, true); backendStatus == ramfs.StatusUnsupported {
		backendStatus = replacer.ReplaceName(node, parent, name)
	}
	status = mapStatus(backendStatus)
	namespace.invalidate()
	if status == StatusOK {
		namespace.follow(sourceMount, node, target)
	} else {
		namespace.scopeMutationFailed(backend)
	}
	return status
}

// changesProtected reports whether moving path to target would take a name
// out of the protected directory, put one into it, or move the directory
// itself. Either path naming nothing yet is decided by its parent.
func (namespace *Namespace) changesProtected(path []byte, target []byte) bool {
	if namespace.guard == nil {
		return false
	}
	for _, name := range [2][]byte{path, target} {
		if _, _, protected, status := namespace.resolve(name); status == StatusOK && protected {
			return true
		}
		if _, _, _, protected, status := namespace.resolveParent(name); status == StatusOK && protected {
			return true
		}
	}
	return false
}

func (namespace *Namespace) resolveParent(path []byte) (uint8, ramfs.NodeID, ramfs.Name, bool, Status) {
	canonical := &namespace.requested
	size, ok := canonicalPath(canonical, path)
	if !ok || size == 1 {
		return 0, 0, ramfs.Name{}, false, StatusInvalidArgument
	}
	last := size - 1
	for last > 0 && canonical[last] != '/' {
		last--
	}
	name, ok := ramfs.MakeName(canonical[last+1 : size])
	if !ok {
		return 0, 0, ramfs.Name{}, false, StatusInvalidArgument
	}
	parentPath := canonical[:last]
	if len(parentPath) == 0 {
		parentPath = canonical[:1]
	}
	mountIndex, parent, protected, status := namespace.resolve(parentPath)
	return mountIndex, parent, name, protected, status
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
	if required != 0 && (entry.rights&required != required || !namespace.permits(owner, entry.scopes, required)) {
		return nil, StatusPermissionDenied
	}
	return entry, StatusOK
}

func (namespace *Namespace) cached(path []byte) (uint8, ramfs.NodeID, bool, bool, bool) {
	for index := range namespace.cache {
		entry := &namespace.cache[index]
		if entry.used && equal(entry.path[:entry.size], path) {
			return entry.mount, entry.node, entry.protected, entry.link, true
		}
	}
	return 0, 0, false, false, false
}

func (namespace *Namespace) remember(path []byte, mountIndex uint8, node ramfs.NodeID, protected bool, link bool) {
	if len(path) > maxCachedPath {
		return
	}
	for index := range namespace.cache {
		entry := &namespace.cache[index]
		if !entry.used {
			entry.used, entry.mount, entry.node, entry.size, entry.protected = true, mountIndex, node, uint16(len(path)), protected
			entry.link = link
			copy(entry.path[:], path)
			return
		}
	}
}

func (namespace *Namespace) invalidate() {
	for index := range namespace.cache {
		namespace.cache[index] = cacheEntry{}
	}
	for slot := range namespace.scopes {
		namespace.scopes[slot].exactKnown = false
		namespace.scopeMembers[slot].known = false
	}
}

// Invalidate forgets resolved paths and scope decisions after a backend changed
// outside this namespace: a filesystem service came or went, or its owner
// changed the volume directly.
func (namespace *Namespace) Invalidate() { namespace.invalidate() }

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
