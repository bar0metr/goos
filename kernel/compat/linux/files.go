package linux

import (
	"goos/kernel/compat/linux/abi"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

// OpenIntent is a decoded open flags word.
type OpenIntent struct {
	Rights      vfs.Rights
	Access      uint32
	Create      bool
	Exclusive   bool
	Truncate    bool
	Append      bool
	Directory   bool
	CloseOnExec bool
	NonBlock    bool
	// NoFollow is O_NOFOLLOW: an open of a symbolic link fails with ELOOP.
	NoFollow bool
}

// StatRights are the rights of the stat an open starts with and of the
// create that may follow it. O_NOFOLLOW, and O_CREAT with O_EXCL, which must
// not create through a link, act on the name itself. The create carries the
// flag too: a link another process makes between the two exchanges then
// stops the create instead of steering it to the link's target.
func (intent OpenIntent) StatRights() vfs.Rights {
	if intent.NoFollow || intent.Create && intent.Exclusive {
		return vfs.NoFollow
	}
	return 0
}

// OpenRights are the rights of the VFS open.
func (intent OpenIntent) OpenRights() vfs.Rights {
	if intent.NoFollow {
		return intent.Rights | vfs.NoFollow
	}
	return intent.Rights
}

// StatusFlags is what the open-file description keeps of the intent: the
// access mode, O_APPEND, and O_NONBLOCK. read, write, and lseek consult it.
func (intent OpenIntent) StatusFlags() uint32 {
	flags := intent.Access
	if intent.Append {
		flags |= abi.OpenAppend
	}
	if intent.NonBlock {
		flags |= abi.OpenNonBlock
	}
	return flags
}

// DecodeOpen accepts the documented open flags. O_LARGEFILE is always in
// effect on x86_64. O_NOCTTY changes nothing here: no GoOS file is a
// terminal device. O_NOFOLLOW refuses a path that ends in a symbolic link.
// O_NONBLOCK changes nothing
// either: a path names a file, a directory, or a device that never blocks,
// and Linux ignores the flag for the first two. Every other flag is refused,
// because ignoring it would claim a behavior the open does not have.
func DecodeOpen(flags uint64) (OpenIntent, abi.Errno) {
	value := uint32(flags)
	const accepted = abi.OpenAccess | abi.OpenCreate | abi.OpenExclusive | abi.OpenNoCTTY | abi.OpenTruncate |
		abi.OpenAppend | abi.OpenLargeFile | abi.OpenDirectory | abi.OpenNoFollow | abi.OpenCloseExec | abi.OpenNonBlock
	if value&^accepted != 0 {
		return OpenIntent{}, abi.EOPNOTSUPP
	}
	intent := OpenIntent{
		Access:      value & abi.OpenAccess,
		Create:      value&abi.OpenCreate != 0,
		Exclusive:   value&abi.OpenExclusive != 0,
		Truncate:    value&abi.OpenTruncate != 0,
		Append:      value&abi.OpenAppend != 0,
		Directory:   value&abi.OpenDirectory != 0,
		CloseOnExec: value&abi.OpenCloseExec != 0,
		NonBlock:    value&abi.OpenNonBlock != 0,
		NoFollow:    value&abi.OpenNoFollow != 0,
	}
	switch intent.Access {
	case abi.OpenReadOnly:
		intent.Rights = vfs.Read
	case abi.OpenWriteOnly:
		intent.Rights = vfs.Write
	case abi.OpenReadWrite:
		intent.Rights = vfs.Read | vfs.Write
	default:
		return OpenIntent{}, abi.EINVAL
	}
	if intent.Directory && intent.Create {
		return OpenIntent{}, abi.EINVAL
	}
	return intent, 0
}

// OpenStep is what an open does after the stat of its resolved path.
type OpenStep uint8

const (
	OpenFail OpenStep = iota
	// OpenCreateFile creates the missing file, then opens it.
	OpenCreateFile
	// OpenTruncateFile empties the existing file, then opens it.
	OpenTruncateFile
	// OpenExistingFile opens the file as it is.
	OpenExistingFile
	// OpenExistingDirectory installs a directory descriptor. A directory has
	// no VFS open file; the descriptor keeps its path.
	OpenExistingDirectory
)

// An open runs up to three VFS exchanges. The stage numbers rise in the
// order the exchanges can occur, which the continuation requires.
const (
	OpenStageStat     = uint8(1)
	OpenStageCreate   = uint8(2)
	OpenStageTruncate = uint8(3)
	OpenStageOpen     = uint8(4)
)

// PlanOpen applies Linux open semantics to the stat result. trailing reports
// that the path itself demanded a directory.
func PlanOpen(intent OpenIntent, trailing bool, status vfs.Status, kind ramfs.Kind) (OpenStep, abi.Errno) {
	switch status {
	case vfs.StatusNotFound:
		if !intent.Create {
			return OpenFail, abi.ENOENT
		}
		if trailing {
			return OpenFail, abi.EISDIR
		}
		return OpenCreateFile, 0
	case vfs.StatusOK:
	default:
		return OpenFail, VFSErrno(status, VFSPermissionAccess, abi.ENFILE)
	}
	if intent.Create && intent.Exclusive {
		return OpenFail, abi.EEXIST
	}
	if kind == ramfs.KindDirectory {
		if intent.Access != abi.OpenReadOnly || intent.Create || intent.Truncate {
			return OpenFail, abi.EISDIR
		}
		return OpenExistingDirectory, 0
	}
	switch kind {
	case ramfs.KindFile:
	case ramfs.KindSymlink:
		// Only a stat that did not follow the last link sees one.
		return OpenFail, abi.ELOOP
	case ramfs.KindSpecial:
		// A device, FIFO or socket node a filesystem stores has nothing
		// behind it in GoOS.
		return OpenFail, abi.ENXIO
	default:
		return OpenFail, abi.EIO
	}
	if trailing || intent.Directory {
		return OpenFail, abi.ENOTDIR
	}
	if intent.Truncate {
		return OpenTruncateFile, 0
	}
	return OpenExistingFile, 0
}

// SeekTarget computes the offset an lseek asks for. current is the file
// position and size the file length, which SEEK_END alone needs. A negative
// or unrepresentable result is EINVAL, as is an unknown whence: GoOS files
// have no holes to seek between.
func SeekTarget(whence uint64, offset int64, current uint64, size uint64) (uint64, abi.Errno) {
	var base uint64
	switch whence {
	case abi.SeekSet:
	case abi.SeekCur:
		base = current
	case abi.SeekEnd:
		base = size
	default:
		return 0, abi.EINVAL
	}
	const limit = uint64(1)<<63 - 1
	if base > limit {
		return 0, abi.EINVAL
	}
	if offset < 0 {
		back := uint64(-(offset + 1)) + 1
		if back > base {
			return 0, abi.EINVAL
		}
		return base - back, 0
	}
	if uint64(offset) > limit-base {
		return 0, abi.EINVAL
	}
	return base + uint64(offset), 0
}

// RemovalErrno checks the kind of a name about to be removed. unlink removes
// files and rmdir directories; a path that demanded a directory cannot name a
// file.
func RemovalErrno(removeDirectory bool, trailing bool, kind ramfs.Kind) abi.Errno {
	switch {
	case kind == ramfs.KindDirectory && !removeDirectory:
		return abi.EISDIR
	case kind != ramfs.KindDirectory && (removeDirectory || trailing):
		return abi.ENOTDIR
	case kind < ramfs.KindFile || kind > ramfs.LastKind:
		return abi.EIO
	}
	return 0
}

// MutationErrno translates the status of a create, remove, truncate, or
// rename. GoOS has no per-file permissions, so a refusal is a read-only
// mount; a full filesystem is the only exhaustion a mutation meets.
func MutationErrno(status vfs.Status) abi.Errno {
	return VFSErrno(status, VFSPermissionReadOnly, abi.ENOSPC)
}

// SyncErrno translates the status of fsync and fdatasync. A filesystem
// without a durability operation is EINVAL, as on Linux; any other failure
// is EIO, the error a failed flush reports.
func SyncErrno(status vfs.Status) abi.Errno {
	switch status {
	case vfs.StatusOK:
		return 0
	case vfs.StatusUnsupported:
		return abi.EINVAL
	case vfs.StatusNotFound:
		return abi.EBADF
	}
	return abi.EIO
}

// LinkErrno translates the status of a symlink or a hard link. A filesystem
// without the kind of link asked for, and a hard link to a directory, are
// EPERM on Linux; a refused change is a read-only mount.
func LinkErrno(status vfs.Status) abi.Errno {
	if status == vfs.StatusUnsupported {
		return abi.EPERM
	}
	return MutationErrno(status)
}

// StatusFlagsOf is what F_GETFL reports for a description. A regular file or
// a directory shows O_LARGEFILE, which every x86_64 open carries; a directory
// also shows O_DIRECTORY. A stream shows its direction alone.
func StatusFlagsOf(description FileDescription) uint32 {
	switch description.Kind {
	case FDStreamReader:
		return abi.OpenReadOnly
	case FDStreamWriter:
		return abi.OpenWriteOnly
	case FDVFSDirectory:
		return description.Flags | abi.OpenLargeFile | abi.OpenDirectory
	case FDSocket:
		// A socket is read-write, and Linux forces no O_LARGEFILE on it.
		return abi.OpenReadWrite | description.Flags&abi.OpenNonBlock
	}
	return description.Flags | abi.OpenLargeFile
}

// SetStatusFlags applies F_SETFL. Linux lets it change O_APPEND, O_NONBLOCK,
// and three flags GoOS has no behavior for; asking for one of those is
// refused, and every other bit is ignored as Linux ignores it. O_APPEND takes
// effect on a regular file, the one kind of description a write consults it
// for. O_NONBLOCK is kept on a file, a directory, or a device, none of which
// blocks; a stream refuses it because its reads and writes do block.
func SetStatusFlags(description *FileDescription, flags uint64) abi.Errno {
	value := uint32(flags)
	if value&(abi.OpenAsync|abi.OpenDirect|abi.OpenNoATime) != 0 {
		return abi.EINVAL
	}
	switch description.Kind {
	case FDStreamReader, FDStreamWriter:
		if value&abi.OpenNonBlock != 0 {
			return abi.EINVAL
		}
		return 0
	case FDVFSFile:
		description.Flags = description.Flags&^abi.OpenAppend | value&abi.OpenAppend
	}
	description.Flags = description.Flags&^abi.OpenNonBlock | value&abi.OpenNonBlock
	return 0
}
