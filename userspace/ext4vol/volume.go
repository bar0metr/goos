// Package ext4vol serves an ext4 volume through the node-ID backend contract
// the VFS speaks with filesystem services (ADR 0096, ADR 0221). The format
// code lives in userspace/ext4; this package maps its inodes, errors and
// metadata onto ramfs types. A writable mount maps the mutations onto the
// journaled engine (ADR 0225), and each one is durable when it returns.
package ext4vol

import (
	"goos/kernel/ramfs"
	"goos/userspace/ext4"
)

// A node ID carries the inode number in its low 32 bits and the inode
// generation in the high 32. A handle that outlives its inode, or one whose
// number Linux reused for a new file, fails the generation check instead of
// reaching the new file.
func nodeID(inode *ext4.Inode) ramfs.NodeID {
	return ramfs.NodeID(uint64(inode.Generation)<<32 | uint64(inode.Number))
}

// Volume is an ext4 filesystem bound to its device. Until Mount accepts the
// volume, Root reports why it was refused and every other operation fails
// with an I/O error.
type Volume struct {
	fs       *ext4.FS
	refusal  ramfs.Status
	reason   string
	entry    ext4.DirEntry
	writable bool
	// source and target hold copies of names and link targets that arrive
	// in the shared file cache.
	source [ext4.MaxNameLength]byte
	target [ramfs.MaxLinkTarget]byte
}

// Mount checks the volume on device read-only and remembers the outcome.
func (volume *Volume) Mount(device ext4.Device) bool {
	return volume.mount(device, ext4.Options{})
}

// MountWritable mounts the volume read-write: it replays the journal and
// finishes the orphan list first, so it also recovers a volume Mount
// refuses. clock stamps inode times.
func (volume *Volume) MountWritable(device ext4.Device, clock func() ext4.Timestamp) bool {
	return volume.mount(device, ext4.Options{Writable: true, Clock: clock})
}

func (volume *Volume) mount(device ext4.Device, options ext4.Options) bool {
	*volume = Volume{}
	fs, err := ext4.Mount(device, options)
	if err != nil {
		volume.refusal, volume.reason = statusOf(err), err.Error()
		return false
	}
	volume.fs, volume.writable = fs, options.Writable
	return true
}

// Failed reports whether a write or flush failed on a writable volume. The
// engine then refuses every operation and leaves the journal for the next
// writable mount to replay.
func (volume *Volume) Failed() bool {
	return volume.fs != nil && volume.fs.State() == ext4.StateFailed
}

// Writable reports whether the volume accepts mutations.
func (volume *Volume) Writable() bool { return volume.fs != nil && volume.writable }

// Refusal is the error text of a refused mount.
func (volume *Volume) Refusal() string { return volume.reason }

// FS returns the mounted filesystem, or nil.
func (volume *Volume) FS() *ext4.FS { return volume.fs }

// statusOf maps a format error onto the protocol's statuses. A stale inode
// is reported as not found: the object the caller named no longer exists.
func statusOf(err error) ramfs.Status {
	switch ext4.KindOf(err) {
	case 0:
		if err == nil {
			return ramfs.StatusOK
		}
		return ramfs.StatusIOError
	case ext4.KindIO:
		return ramfs.StatusIOError
	case ext4.KindCorrupt:
		return ramfs.StatusCorrupt
	case ext4.KindUnsupported, ext4.KindNeedsRecovery, ext4.KindNotSymlink:
		return ramfs.StatusUnsupported
	case ext4.KindNotFound, ext4.KindStale:
		return ramfs.StatusNotFound
	case ext4.KindNotDirectory:
		return ramfs.StatusNotDirectory
	case ext4.KindIsDirectory:
		return ramfs.StatusIsDirectory
	case ext4.KindReadOnly:
		return ramfs.StatusPermissionDenied
	case ext4.KindNoSpace:
		return ramfs.StatusNoSpace
	case ext4.KindExists:
		return ramfs.StatusAlreadyExists
	case ext4.KindNotEmpty:
		return ramfs.StatusNotEmpty
	case ext4.KindTooManyLinks:
		return ramfs.StatusResourceExhausted
	case ext4.KindInternal:
		return ramfs.StatusIOError
	}
	return ramfs.StatusInvalidArgument
}

// inode resolves a node ID to its live inode.
func (volume *Volume) inode(id ramfs.NodeID) (*ext4.Inode, ramfs.Status) {
	if volume.fs == nil {
		return nil, ramfs.StatusIOError
	}
	number := uint32(id)
	if number == 0 {
		return nil, ramfs.StatusInvalidArgument
	}
	// Node IDs come from the VFS, not the volume: a number past the inode
	// table names nothing rather than damage.
	if number > volume.fs.Superblock().InodesCount {
		return nil, ramfs.StatusNotFound
	}
	inode, err := volume.fs.HeldInode(number)
	if err != nil {
		return nil, statusOf(err)
	}
	if uint32(uint64(id)>>32) != inode.Generation {
		return nil, ramfs.StatusNotFound
	}
	return inode, ramfs.StatusOK
}

// kindOf maps an inode's file type onto the native kinds. The VFS follows
// symbolic links itself (ADR 0223); device, FIFO and socket inodes are
// listed and described but never opened.
func kindOf(inode *ext4.Inode) ramfs.Kind {
	switch inode.Type() {
	case ext4.ModeRegular:
		return ramfs.KindFile
	case ext4.ModeDir:
		return ramfs.KindDirectory
	case ext4.ModeSymlink:
		return ramfs.KindSymlink
	}
	return ramfs.KindSpecial
}

func (volume *Volume) Root() (ramfs.NodeID, ramfs.Status) {
	if volume.fs == nil {
		if volume.refusal != ramfs.StatusOK {
			return 0, volume.refusal
		}
		return 0, ramfs.StatusIOError
	}
	root, err := volume.fs.Root()
	if err != nil {
		return 0, statusOf(err)
	}
	return nodeID(root), ramfs.StatusOK
}

func (volume *Volume) lookup(parent ramfs.NodeID, name ramfs.Name) (*ext4.Inode, ramfs.Status) {
	dir, status := volume.inode(parent)
	if status != ramfs.StatusOK {
		return nil, status
	}
	if !dir.IsDir() {
		return nil, ramfs.StatusNotDirectory
	}
	var buffer [ramfs.MaxName]byte
	size := name.CopyTo(buffer[:])
	if size == 0 {
		return nil, ramfs.StatusInvalidArgument
	}
	if size > ext4.MaxNameLength {
		return nil, ramfs.StatusNotFound
	}
	number, _, err := volume.fs.Lookup(dir, buffer[:size])
	if err != nil {
		return nil, statusOf(err)
	}
	inode, err := volume.fs.Inode(number)
	if err != nil {
		if ext4.KindOf(err) == ext4.KindStale {
			// A name that points at a free inode is damage, not absence.
			return nil, ramfs.StatusCorrupt
		}
		return nil, statusOf(err)
	}
	return inode, ramfs.StatusOK
}

func (volume *Volume) OpenName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	inode, status := volume.lookup(parent, name)
	if status != ramfs.StatusOK {
		return 0, status
	}
	return nodeID(inode), ramfs.StatusOK
}

func (volume *Volume) Read(id ramfs.NodeID, offset uint64, target []byte) (int, ramfs.Status) {
	inode, status := volume.inode(id)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if inode.IsDir() {
		return 0, ramfs.StatusIsDirectory
	}
	if !inode.IsRegular() {
		return 0, ramfs.StatusUnsupported
	}
	count, err := volume.fs.ReadFile(inode, offset, target)
	if err != nil {
		return 0, statusOf(err)
	}
	return count, ramfs.StatusOK
}

func timestamp(stamp ext4.Timestamp, available bool) ramfs.Timestamp {
	if !available {
		return ramfs.Timestamp{}
	}
	return ramfs.Timestamp{Seconds: stamp.Seconds, Nanoseconds: stamp.Nanos, Available: true}
}

// stat reports the native view of an inode. ext4 has no FAT attributes, so
// none are claimed. The Unix metadata is what the inode stores; the VFS
// grants nothing from it (ADR 0223).
func stat(inode *ext4.Inode) ramfs.Stat {
	result := ramfs.Stat{
		ID:       nodeID(inode),
		Kind:     kindOf(inode),
		Size:     inode.Size,
		Created:  timestamp(inode.Create, inode.HasCreate),
		Modified: timestamp(inode.Modify, true),
		Unix: ramfs.Unix{
			Mode:       inode.Mode,
			UID:        inode.UID,
			GID:        inode.GID,
			Links:      uint32(inode.Links),
			Generation: inode.Generation,
			Device:     inode.DeviceNumber(),
			Inode:      uint64(inode.Number),
			Blocks:     inode.Blocks512,
			Accessed:   timestamp(inode.Access, true),
			Changed:    timestamp(inode.Change, true),
			Available:  true,
		},
	}
	// A directory keeps its i_size, as Linux reports it; a listing never
	// reads it as a byte count of entries.
	if result.Kind == ramfs.KindSpecial {
		result.Size = 0
	}
	return result
}

func (volume *Volume) Stat(id ramfs.NodeID) (ramfs.Stat, ramfs.Status) {
	inode, status := volume.inode(id)
	if status != ramfs.StatusOK {
		return ramfs.Stat{}, status
	}
	return stat(inode), ramfs.StatusOK
}

// ReadLink copies the target of the symbolic link id into target. A node
// that is no link is StatusInvalidArgument, as Linux readlink answers EINVAL.
func (volume *Volume) ReadLink(id ramfs.NodeID, target []byte) (int, ramfs.Status) {
	inode, status := volume.inode(id)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if !inode.IsSymlink() {
		return 0, ramfs.StatusInvalidArgument
	}
	link, err := volume.fs.Readlink(inode)
	if err != nil {
		return 0, statusOf(err)
	}
	if !ramfs.ValidLinkTarget(link) {
		return 0, ramfs.StatusCorrupt
	}
	if len(link) > len(target) {
		return 0, ramfs.StatusResourceExhausted
	}
	return copy(target, link), ramfs.StatusOK
}

// listing walks parent from cursor, a byte position in the directory, and
// calls emit for each served entry with the cursor that follows it.
func (volume *Volume) listing(parent ramfs.NodeID, cursor int, emit func(*ext4.Inode, []byte, int) bool) (bool, ramfs.Status) {
	if cursor < 0 {
		return false, ramfs.StatusInvalidArgument
	}
	dir, status := volume.inode(parent)
	if status != ramfs.StatusOK {
		return false, status
	}
	if !dir.IsDir() {
		return false, ramfs.StatusNotDirectory
	}
	if dir.Size > 0xFFFFFFFF {
		return false, ramfs.StatusUnsupported
	}
	failure := ramfs.StatusOK
	next, err := volume.fs.ReadDir(dir, uint64(cursor), func(entry *ext4.DirEntry) bool {
		inode, err := volume.fs.Inode(entry.Inode)
		if err != nil {
			failure = statusOf(err)
			if ext4.KindOf(err) == ext4.KindStale {
				failure = ramfs.StatusCorrupt
			}
			return false
		}
		volume.entry = *entry
		// The entry after this one starts no earlier than one byte on; the
		// directory walk skips to the next record from there.
		return emit(inode, volume.entry.Name(), int(entry.Offset)+1)
	})
	if err != nil {
		return false, statusOf(err)
	}
	if failure != ramfs.StatusOK {
		return false, failure
	}
	return next >= dir.Size, ramfs.StatusOK
}

func (volume *Volume) ReadDir(parent ramfs.NodeID, cursor int) (ramfs.Entry, int, ramfs.Status) {
	var result ramfs.Entry
	next := -1
	_, status := volume.listing(parent, cursor, func(inode *ext4.Inode, name []byte, after int) bool {
		if len(name) > ramfs.MaxName {
			return true
		}
		result.Stat = stat(inode)
		result.Size = uint16(copy(result.Name[:], name))
		next = after
		return false
	})
	if status != ramfs.StatusOK {
		return ramfs.Entry{}, 0, status
	}
	if next < 0 {
		return ramfs.Entry{}, 0, ramfs.StatusNotFound
	}
	return result, next, ramfs.StatusOK
}

func (volume *Volume) ReadDirBatch(parent ramfs.NodeID, cursor int, target []byte) (ramfs.DirBatch, ramfs.Status) {
	batch := ramfs.DirBatch{Next: cursor}
	end, status := volume.listing(parent, cursor, func(inode *ext4.Inode, name []byte, after int) bool {
		if batch.Count == ramfs.MaxDirBatch {
			return false
		}
		entryStat := stat(inode)
		written := ramfs.AppendDirRecord(target[batch.Used:], &entryStat, after, name)
		if written == 0 {
			return false
		}
		batch.Used += written
		batch.Count++
		batch.Next = after
		return true
	})
	if status != ramfs.StatusOK {
		if batch.Count != 0 {
			return batch, ramfs.StatusOK
		}
		return batch, status
	}
	batch.End = end
	return batch, ramfs.StatusOK
}

// AcquireObject and ReleaseObject confirm the object is still the one the
// caller names. A writable volume also pins the inode while a caller holds
// it, so the inode keeps its blocks if its last name goes before the last
// release.
func (volume *Volume) AcquireObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	inode, status := volume.inode(id)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if volume.writable {
		if err := volume.fs.Pin(inode); err != nil {
			return 0, statusOf(err)
		}
	}
	return id, ramfs.StatusOK
}

func (volume *Volume) ReleaseObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	if volume.fs == nil {
		return 0, ramfs.StatusIOError
	}
	if !volume.writable {
		return volume.AcquireObject(id)
	}
	inode, status := volume.inode(id)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if err := volume.fs.Unpin(inode); err != nil {
		return 0, statusOf(err)
	}
	return id, ramfs.StatusOK
}

func (volume *Volume) FilesystemInfo() (ramfs.FilesystemInfo, ramfs.Status) {
	if volume.fs == nil {
		return ramfs.FilesystemInfo{}, ramfs.StatusIOError
	}
	info := volume.fs.Info()
	total := info.Blocks * uint64(info.BlockSize)
	free := info.FreeBlocks * uint64(info.BlockSize)
	if free > total {
		free = total
	}
	result := ramfs.FilesystemInfo{
		Type:              ramfs.FilesystemExt4,
		TotalBytes:        total,
		FreeBytes:         free,
		AvailableBytes:    min(info.AvailableBytes, free),
		ReadOnly:          !volume.writable,
		CapacityAvailable: true,
	}
	label := info.Label
	if len(label) > ramfs.MaxVolumeName {
		label = label[:ramfs.MaxVolumeName]
	}
	result.SetVolumeName(label)
	return result, ramfs.StatusOK
}

// The mutations. A read-only volume refuses each one the way a read-only
// backend does. New objects belong to user and group 0 with modes 0644 and
// 0755: GoOS grants nothing from them (ADR 0223).

const (
	fileMode      = ext4.ModeRegular | 0o644
	directoryMode = 0o755
)

// mutable returns the directory parent of a writable volume.
func (volume *Volume) mutable(parent ramfs.NodeID) (*ext4.Inode, ramfs.Status) {
	if volume.fs == nil {
		return nil, ramfs.StatusIOError
	}
	if !volume.writable {
		return nil, ramfs.StatusPermissionDenied
	}
	dir, status := volume.inode(parent)
	if status != ramfs.StatusOK {
		return nil, status
	}
	if !dir.IsDir() {
		return nil, ramfs.StatusNotDirectory
	}
	return dir, ramfs.StatusOK
}

// nameBytes copies name into buffer, refusing one ext4 cannot store.
func nameBytes(name ramfs.Name, buffer *[ext4.MaxNameLength]byte) ([]byte, ramfs.Status) {
	var whole [ramfs.MaxName]byte
	size := name.CopyTo(whole[:])
	if size == 0 || size > ext4.MaxNameLength {
		return nil, ramfs.StatusInvalidArgument
	}
	return buffer[:copy(buffer[:], whole[:size])], ramfs.StatusOK
}

// created answers with the node of a new inode.
func created(inode *ext4.Inode, err error) (ramfs.NodeID, ramfs.Status) {
	if err != nil {
		return 0, statusOf(err)
	}
	return nodeID(inode), ramfs.StatusOK
}

func (volume *Volume) CreateName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	dir, status := volume.mutable(parent)
	if status != ramfs.StatusOK {
		return 0, status
	}
	var buffer [ext4.MaxNameLength]byte
	bytes, status := nameBytes(name, &buffer)
	if status != ramfs.StatusOK {
		return 0, status
	}
	return created(volume.fs.Create(dir, bytes, fileMode, 0, 0))
}

func (volume *Volume) MkdirName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	dir, status := volume.mutable(parent)
	if status != ramfs.StatusOK {
		return 0, status
	}
	var buffer [ext4.MaxNameLength]byte
	bytes, status := nameBytes(name, &buffer)
	if status != ramfs.StatusOK {
		return 0, status
	}
	return created(volume.fs.Mkdir(dir, bytes, directoryMode, 0, 0))
}

// file returns the regular file id of a writable volume.
func (volume *Volume) file(id ramfs.NodeID) (*ext4.Inode, ramfs.Status) {
	if volume.fs == nil {
		return nil, ramfs.StatusIOError
	}
	if !volume.writable {
		return nil, ramfs.StatusPermissionDenied
	}
	inode, status := volume.inode(id)
	if status != ramfs.StatusOK {
		return nil, status
	}
	if inode.IsDir() {
		return nil, ramfs.StatusIsDirectory
	}
	if !inode.IsRegular() {
		return nil, ramfs.StatusUnsupported
	}
	return inode, ramfs.StatusOK
}

// Write reports the bytes it committed. A write the engine split over
// several transactions may stop after a committed prefix; the caller gets
// that prefix and meets the error on its next write.
func (volume *Volume) Write(id ramfs.NodeID, offset uint64, data []byte) (int, ramfs.Status) {
	inode, status := volume.file(id)
	if status != ramfs.StatusOK {
		return 0, status
	}
	count, err := volume.fs.WriteFile(inode, offset, data)
	if count < 0 || count > len(data) {
		return 0, ramfs.StatusIOError
	}
	if err != nil && count == 0 {
		return 0, statusOf(err)
	}
	return count, ramfs.StatusOK
}

func (volume *Volume) Truncate(id ramfs.NodeID, size uint64) ramfs.Status {
	inode, status := volume.file(id)
	if status != ramfs.StatusOK {
		return status
	}
	return statusOf(volume.fs.Truncate(inode, size))
}

// UnlinkName removes a name of a file, link or special inode, or an empty
// directory.
func (volume *Volume) UnlinkName(parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	dir, status := volume.mutable(parent)
	if status != ramfs.StatusOK {
		return status
	}
	child, status := volume.lookup(parent, name)
	if status != ramfs.StatusOK {
		return status
	}
	var buffer [ext4.MaxNameLength]byte
	bytes, status := nameBytes(name, &buffer)
	if status != ramfs.StatusOK {
		return status
	}
	if child.IsDir() {
		return statusOf(volume.fs.Rmdir(dir, bytes))
	}
	return statusOf(volume.fs.Unlink(dir, bytes))
}

func (volume *Volume) SymlinkName(parent ramfs.NodeID, name ramfs.Name, target []byte) (ramfs.NodeID, ramfs.Status) {
	dir, status := volume.mutable(parent)
	if status != ramfs.StatusOK {
		return 0, status
	}
	if len(target) > len(volume.target) {
		return 0, ramfs.StatusInvalidArgument
	}
	// The target lies in the shared cache: check the copy that is stored.
	link := volume.target[:copy(volume.target[:], target)]
	if !ramfs.ValidLinkTarget(link) {
		return 0, ramfs.StatusInvalidArgument
	}
	var buffer [ext4.MaxNameLength]byte
	bytes, status := nameBytes(name, &buffer)
	if status != ramfs.StatusOK {
		return 0, status
	}
	return created(volume.fs.Symlink(dir, bytes, link, 0, 0))
}

func (volume *Volume) LinkName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	dir, status := volume.mutable(parent)
	if status != ramfs.StatusOK {
		return status
	}
	inode, status := volume.inode(id)
	if status != ramfs.StatusOK {
		return status
	}
	var buffer [ext4.MaxNameLength]byte
	bytes, status := nameBytes(name, &buffer)
	if status != ramfs.StatusOK {
		return status
	}
	return statusOf(volume.fs.Link(dir, bytes, inode))
}

// RenameName and ReplaceName name only the node, and an ext4 inode does not
// record its names. The VFS sends RenameEntry and ReplaceEntry instead.
func (volume *Volume) RenameName(ramfs.NodeID, ramfs.NodeID, ramfs.Name) ramfs.Status {
	if volume.fs == nil {
		return ramfs.StatusIOError
	}
	if !volume.writable {
		return ramfs.StatusPermissionDenied
	}
	return ramfs.StatusUnsupported
}

func (volume *Volume) ReplaceName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	return volume.RenameName(id, parent, name)
}

// moving resolves the source of a rename: sourceName, a copy of which is
// taken from the shared cache, must still name id in sourceParent.
func (volume *Volume) moving(sourceParent ramfs.NodeID, sourceName []byte, id ramfs.NodeID) (*ext4.Inode, []byte, *ext4.Inode, ramfs.Status) {
	from, status := volume.mutable(sourceParent)
	if status != ramfs.StatusOK {
		return nil, nil, nil, status
	}
	if len(sourceName) == 0 || len(sourceName) > len(volume.source) {
		return nil, nil, nil, ramfs.StatusInvalidArgument
	}
	copied := volume.source[:copy(volume.source[:], sourceName)]
	source, valid := ramfs.MakeName(copied)
	if !valid {
		return nil, nil, nil, ramfs.StatusInvalidArgument
	}
	moved, status := volume.lookup(sourceParent, source)
	if status != ramfs.StatusOK {
		return nil, nil, nil, status
	}
	if nodeID(moved) != id {
		return nil, nil, nil, ramfs.StatusNotFound
	}
	return from, copied, moved, ramfs.StatusOK
}

// RenameEntry moves sourceName in sourceParent, which must name id, to
// name in parent. An existing name refuses the move, as RenameName does
// everywhere else.
func (volume *Volume) RenameEntry(sourceParent ramfs.NodeID, sourceName []byte, id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	from, source, _, status := volume.moving(sourceParent, sourceName, id)
	if status != ramfs.StatusOK {
		return status
	}
	switch _, status := volume.lookup(parent, name); status {
	case ramfs.StatusOK:
		return ramfs.StatusAlreadyExists
	case ramfs.StatusNotFound:
	default:
		return status
	}
	return volume.rename(from, source, parent, name)
}

// ReplaceEntry moves the regular file sourceName onto the regular file name
// in parent, which goes in the same transaction (ADR 0129). With no such
// file it is a rename; onto another name of the same inode it does nothing.
func (volume *Volume) ReplaceEntry(sourceParent ramfs.NodeID, sourceName []byte, id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	from, source, moved, status := volume.moving(sourceParent, sourceName, id)
	if status != ramfs.StatusOK {
		return status
	}
	if !moved.IsRegular() {
		return ramfs.StatusIsDirectory
	}
	switch target, status := volume.lookup(parent, name); status {
	case ramfs.StatusOK:
		if !target.IsRegular() {
			return ramfs.StatusIsDirectory
		}
		// Another link of the same inode: nothing changes, as rename(2)
		// leaves both names in place.
		if target.Number == moved.Number {
			return ramfs.StatusOK
		}
	case ramfs.StatusNotFound:
	default:
		return status
	}
	return volume.rename(from, source, parent, name)
}

func (volume *Volume) rename(from *ext4.Inode, source []byte, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	to, status := volume.mutable(parent)
	if status != ramfs.StatusOK {
		return status
	}
	var buffer [ext4.MaxNameLength]byte
	bytes, status := nameBytes(name, &buffer)
	if status != ramfs.StatusOK {
		return status
	}
	return statusOf(volume.fs.Rename(from, source, to, bytes))
}

// SetMetadata sets the modification time and maps the read-only attribute
// onto the write bits: read-only clears all three, writable restores the
// owner's.
func (volume *Volume) SetMetadata(id ramfs.NodeID, change ramfs.MetadataChange) ramfs.Status {
	if volume.fs == nil {
		return ramfs.StatusIOError
	}
	if !volume.writable {
		return ramfs.StatusPermissionDenied
	}
	if !change.Valid() {
		return ramfs.StatusInvalidArgument
	}
	inode, status := volume.inode(id)
	if status != ramfs.StatusOK {
		return status
	}
	var attributes ext4.AttributeChange
	if change.SetModified {
		attributes.SetModify = true
		attributes.Modify = ext4.Timestamp{Seconds: change.Modified.Seconds, Nanos: change.Modified.Nanoseconds}
	}
	if change.SetReadOnly {
		attributes.SetMode = true
		attributes.Mode = inode.Mode &^ 0o222
		if !change.ReadOnly {
			attributes.Mode |= 0o200
		}
	}
	return statusOf(volume.fs.SetAttributes(inode, attributes))
}

// Flush commits the mutations a writable volume holds in memory: when it
// returns true, every mutation that returned before it survives a power
// cut (ADR 0227).
func (volume *Volume) Flush() bool {
	if volume.fs == nil {
		return false
	}
	return !volume.writable || volume.fs.Sync() == nil
}

// Pending reports whether a writable volume holds mutations it has not
// committed yet.
func (volume *Volume) Pending() bool {
	return volume.fs != nil && volume.writable && volume.fs.Pending()
}

// Unmount leaves the volume clean on disk and closes it: every later
// request fails. A writable volume first releases the inodes kept only for
// open handles, then empties the journal and marks the superblock clean.
// After a failed write the journal stays for the next writable mount.
func (volume *Volume) Unmount() ramfs.Status {
	if volume.fs == nil {
		return ramfs.StatusIOError
	}
	err := volume.fs.Unmount()
	volume.fs, volume.writable = nil, false
	if err != nil {
		volume.reason = err.Error()
		return statusOf(err)
	}
	return ramfs.StatusOK
}

// Status reports the engine's mount state and counters, or false when no
// volume is mounted.
func (volume *Volume) Status() (ext4.Status, bool) {
	if volume.fs == nil {
		return ext4.Status{}, false
	}
	return volume.fs.Status(), true
}
