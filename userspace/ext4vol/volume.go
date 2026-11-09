// Package ext4vol serves a read-only ext4 volume through the node-ID backend
// contract the VFS speaks with filesystem services (ADR 0096, ADR 0221).
// The format code lives in userspace/ext4; this package maps its inodes,
// errors and metadata onto ramfs types.
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
	fs      *ext4.FS
	refusal ramfs.Status
	reason  string
	entry   ext4.DirEntry
}

// Mount checks the volume on device read-only and remembers the outcome.
func (volume *Volume) Mount(device ext4.Device) bool {
	fs, err := ext4.Mount(device, ext4.Options{})
	if err != nil {
		volume.fs, volume.refusal, volume.reason = nil, statusOf(err), err.Error()
		return false
	}
	volume.fs, volume.refusal, volume.reason = fs, ramfs.StatusOK, ""
	return true
}

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
	inode, err := volume.fs.Inode(number)
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
			Accessed:   timestamp(inode.Access, true),
			Changed:    timestamp(inode.Change, true),
			Available:  true,
		},
	}
	switch result.Kind {
	case ramfs.KindDirectory, ramfs.KindSpecial:
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
// caller names. A read-only volume needs no open-object bookkeeping: nothing
// can unlink the inode under the caller.
func (volume *Volume) AcquireObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	if _, status := volume.inode(id); status != ramfs.StatusOK {
		return 0, status
	}
	return id, ramfs.StatusOK
}

func (volume *Volume) ReleaseObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	return volume.AcquireObject(id)
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
		ReadOnly:          true,
		CapacityAvailable: true,
	}
	label := info.Label
	if len(label) > ramfs.MaxVolumeName {
		label = label[:ramfs.MaxVolumeName]
	}
	result.SetVolumeName(label)
	return result, ramfs.StatusOK
}

// Every mutation is refused the way a read-only backend refuses one.

func (volume *Volume) CreateName(ramfs.NodeID, ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	return 0, volume.readOnly()
}

func (volume *Volume) MkdirName(ramfs.NodeID, ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	return 0, volume.readOnly()
}

func (volume *Volume) Write(ramfs.NodeID, uint64, []byte) (int, ramfs.Status) {
	return 0, volume.readOnly()
}

func (volume *Volume) Truncate(ramfs.NodeID, uint64) ramfs.Status { return volume.readOnly() }

func (volume *Volume) RenameName(ramfs.NodeID, ramfs.NodeID, ramfs.Name) ramfs.Status {
	return volume.readOnly()
}

func (volume *Volume) ReplaceName(ramfs.NodeID, ramfs.NodeID, ramfs.Name) ramfs.Status {
	return volume.readOnly()
}

func (volume *Volume) UnlinkName(ramfs.NodeID, ramfs.Name) ramfs.Status { return volume.readOnly() }

func (volume *Volume) SymlinkName(ramfs.NodeID, ramfs.Name, []byte) (ramfs.NodeID, ramfs.Status) {
	return 0, volume.readOnly()
}

func (volume *Volume) LinkName(ramfs.NodeID, ramfs.NodeID, ramfs.Name) ramfs.Status {
	return volume.readOnly()
}

func (volume *Volume) SetMetadata(ramfs.NodeID, ramfs.MetadataChange) ramfs.Status {
	return volume.readOnly()
}

func (volume *Volume) readOnly() ramfs.Status {
	if volume.fs == nil {
		return ramfs.StatusIOError
	}
	return ramfs.StatusPermissionDenied
}

// Flush has nothing to write on a read-only volume.
func (volume *Volume) Flush() bool { return volume.fs != nil }
