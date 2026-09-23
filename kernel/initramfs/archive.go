package initramfs

import "goos/kernel/ramfs"

// Archive is a read-only filesystem backend over the boot system image. The
// namespace mounts it so the image is visible under the same names the launch
// path resolves, instead of the executables existing only as a search-path
// string. Every mutation is refused: the system image is immutable, and
// nothing is copied into a writable filesystem to make it listable.
type Archive struct {
	data []byte
	// name holds the component a lookup is matching. It lives here because a
	// local array escapes into ramfs.Name.CopyTo and the callers of this
	// backend run on heaps that reclaim little.
	name [ramfs.MaxName]byte
}

// rootNode names the archive root, which has no header of its own. Member node
// IDs start above it, one per header block.
const rootNode = ramfs.NodeID(1)

// Init validates the whole archive before it is mounted, so a malformed image
// is rejected at mount time rather than on a later lookup.
func (archive *Archive) Init(data []byte) bool {
	if len(data) < blockSize {
		return false
	}
	members := 0
	for offset := 0; ; {
		// A well-formed archive ends at a zero block. Running out of data
		// instead means the image was truncated, so the mount fails rather
		// than exposing the prefix that happened to arrive.
		if offset > len(data)-blockSize {
			return false
		}
		if zero(data[offset : offset+blockSize]) {
			archive.data = data
			return members != 0
		}
		_, next, ok := scan(data, offset)
		if !ok {
			return false
		}
		members++
		offset = next
	}
}

func (archive *Archive) Root() ramfs.NodeID { return rootNode }

func (archive *Archive) OpenName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	prefix, status := archive.directoryPath(parent)
	if status != ramfs.StatusOK {
		return 0, status
	}
	size := name.CopyTo(archive.name[:])
	if size == 0 {
		return 0, ramfs.StatusInvalidArgument
	}
	wanted := archive.name[:size]
	for offset := 0; ; {
		entry, next, ok := scan(archive.data, offset)
		if !ok {
			return 0, ramfs.StatusNotFound
		}
		if component, direct := child(entry.name, prefix); direct && equalName(component, wanted) {
			return nodeID(entry.offset), ramfs.StatusOK
		}
		offset = next
	}
}

func (archive *Archive) Read(id ramfs.NodeID, offset uint64, target []byte) (int, ramfs.Status) {
	entry, ok := archive.member(id)
	if !ok {
		return 0, ramfs.StatusNotFound
	}
	if entry.typeFlag == typeDirectory {
		return 0, ramfs.StatusIsDirectory
	}
	data, resolved := archive.content(entry)
	if !resolved {
		return 0, ramfs.StatusNotFound
	}
	if offset > uint64(len(data)) {
		return 0, ramfs.StatusInvalidArgument
	}
	return copy(target, data[offset:]), ramfs.StatusOK
}

func (archive *Archive) Stat(id ramfs.NodeID) (ramfs.Stat, ramfs.Status) {
	if id == rootNode {
		return ramfs.Stat{ID: rootNode, Kind: ramfs.KindDirectory}, ramfs.StatusOK
	}
	entry, ok := archive.member(id)
	if !ok {
		return ramfs.Stat{}, ramfs.StatusNotFound
	}
	if entry.typeFlag == typeDirectory {
		return ramfs.Stat{ID: id, Kind: ramfs.KindDirectory}, ramfs.StatusOK
	}
	data, resolved := archive.content(entry)
	if !resolved {
		return ramfs.Stat{}, ramfs.StatusNotFound
	}
	return ramfs.Stat{ID: id, Kind: ramfs.KindFile, Size: uint64(len(data))}, ramfs.StatusOK
}

func (archive *Archive) ReadDir(parent ramfs.NodeID, cursor int) (ramfs.Entry, int, ramfs.Status) {
	prefix, status := archive.directoryPath(parent)
	if status != ramfs.StatusOK {
		return ramfs.Entry{}, cursor, status
	}
	if cursor < 0 || cursor%blockSize != 0 {
		return ramfs.Entry{}, cursor, ramfs.StatusInvalidArgument
	}
	for offset := cursor; ; {
		entry, next, ok := scan(archive.data, offset)
		if !ok {
			return ramfs.Entry{}, len(archive.data), ramfs.StatusNotFound
		}
		component, direct := child(entry.name, prefix)
		if direct && len(component) <= ramfs.MaxName {
			stat, status := archive.Stat(nodeID(entry.offset))
			if status != ramfs.StatusOK {
				return ramfs.Entry{}, cursor, status
			}
			result := ramfs.Entry{Stat: stat, Size: uint8(len(component))}
			copy(result.Name[:], component)
			return result, next, ramfs.StatusOK
		}
		offset = next
	}
}

// The system image is immutable, so every mutation is refused rather than
// silently applied to a copy.

func (*Archive) CreateName(ramfs.NodeID, ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	return 0, ramfs.StatusPermissionDenied
}

func (*Archive) MkdirName(ramfs.NodeID, ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	return 0, ramfs.StatusPermissionDenied
}

func (*Archive) Write(ramfs.NodeID, uint64, []byte) (int, ramfs.Status) {
	return 0, ramfs.StatusPermissionDenied
}

func (*Archive) Truncate(ramfs.NodeID, uint64) ramfs.Status { return ramfs.StatusPermissionDenied }

func (*Archive) RenameName(ramfs.NodeID, ramfs.NodeID, ramfs.Name) ramfs.Status {
	return ramfs.StatusPermissionDenied
}

func (*Archive) UnlinkName(ramfs.NodeID, ramfs.Name) ramfs.Status {
	return ramfs.StatusPermissionDenied
}

// nodeID names a member by the block its header occupies, which keeps node IDs
// stable for as long as the archive is mounted.
func nodeID(offset int) ramfs.NodeID { return ramfs.NodeID(offset/blockSize) + rootNode + 1 }

func (archive *Archive) member(id ramfs.NodeID) (member, bool) {
	if id <= rootNode {
		return member{}, false
	}
	offset := (int(id) - int(rootNode) - 1) * blockSize
	entry, _, ok := scan(archive.data, offset)
	return entry, ok
}

// directoryPath returns the archive-relative name of a directory node, without
// the trailing separator USTAR writes on directory members. The root has no
// header, so its name is empty.
func (archive *Archive) directoryPath(id ramfs.NodeID) ([]byte, ramfs.Status) {
	if id == rootNode {
		return nil, ramfs.StatusOK
	}
	entry, ok := archive.member(id)
	if !ok {
		return nil, ramfs.StatusNotFound
	}
	if entry.typeFlag != typeDirectory {
		return nil, ramfs.StatusNotDirectory
	}
	return trimSeparator(entry.name), ramfs.StatusOK
}

// content resolves a member to its bytes, following the hard links the image
// uses to give one program several names.
func (archive *Archive) content(entry member) ([]byte, bool) {
	if entry.typeFlag != typeLink {
		return entry.data, true
	}
	if len(entry.link) == 0 {
		return nil, false
	}
	return find(archive.data, entry.link, 1)
}

// child reports whether name is a direct child of the directory named by
// parent, and returns the child's own component.
func child(name []byte, parent []byte) ([]byte, bool) {
	if len(parent) != 0 {
		if len(name) <= len(parent) || !equalName(name[:len(parent)], parent) || name[len(parent)] != '/' {
			return nil, false
		}
		name = name[len(parent)+1:]
	}
	name = trimSeparator(name)
	if len(name) == 0 {
		return nil, false
	}
	for _, value := range name {
		if value == '/' {
			return nil, false
		}
	}
	return name, true
}

func trimSeparator(name []byte) []byte {
	if len(name) != 0 && name[len(name)-1] == '/' {
		return name[:len(name)-1]
	}
	return name
}
