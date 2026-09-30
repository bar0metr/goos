package linux

import (
	"encoding/binary"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

// StatSize is the size of struct stat on Linux x86_64. The layout below is
// written out field by field from the kernel's arch/x86/include/uapi/asm/stat.h
// and owes nothing to the host this code is built or tested on.
const StatSize = 144

const (
	statDevice     = 0   // st_dev, 8 bytes
	statInode      = 8   // st_ino, 8
	statLinks      = 16  // st_nlink, 8
	statMode       = 24  // st_mode, 4
	statUID        = 28  // st_uid, 4
	statGID        = 32  // st_gid, 4; 4 bytes of padding follow
	statRawDevice  = 40  // st_rdev, 8
	statLength     = 48  // st_size, 8
	statBlockSize  = 56  // st_blksize, 8
	statBlocks     = 64  // st_blocks, 8, in 512-byte units
	statAccessed   = 72  // st_atime then st_atime_nsec, 8 each
	statModified   = 88  // st_mtime then st_mtime_nsec
	statChanged    = 104 // st_ctime then st_ctime_nsec; 24 unused bytes follow
	statBlockUnits = 512
)

const (
	// StatUID and StatGID are the one owner every file shows. They match the
	// identity the initial stack gives the program and grant nothing: GoOS
	// authority is capabilities, never these numbers.
	StatUID = uint32(1000)
	StatGID = uint32(1000)
	// StatBlockSize is the preferred I/O size reported for every file.
	StatBlockSize = 4096
	// StreamDevice is st_dev of a stream descriptor. No mount uses it: mount
	// identities have a generation in their upper half.
	StreamDevice = uint64(1)
)

// FileStatus is the part of struct stat GoOS has an answer for.
type FileStatus struct {
	Device      uint64
	Inode       uint64
	Mode        uint32
	Size        uint64
	Seconds     int64
	Nanoseconds uint32
}

// StatusOfReply turns a VFS stat reply into Linux file status.
//
// The mode is presentation: a regular file is 0644 and a directory 0755, with
// the write bits cleared when the VFS reports the read-only attribute. GoOS
// enforces none of it. The inode is the node identity the VFS returned and
// the device the mount identity, so two names of one file compare equal and
// files of different mounts never do. Zero is not a usable inode, because
// programs read it as "no entry".
//
// GoOS keeps one time per file that Linux has three names for. All three
// show the modification time, or the creation time when the filesystem has
// only that, or zero when it has neither.
func StatusOfReply(reply *vfs.Reply) (FileStatus, abi.Errno) {
	status := FileStatus{Device: uint64(reply.MountID), Inode: reply.Node, Size: reply.Size}
	switch reply.Kind {
	case ramfs.KindFile:
		status.Mode = abi.ModeRegular | 0o644
	case ramfs.KindDirectory:
		status.Mode = abi.ModeDirectory | 0o755
	default:
		return FileStatus{}, abi.EIO
	}
	if reply.AttributesAvailable && reply.Attributes&ramfs.AttrReadOnly != 0 {
		status.Mode &^= 0o222
	}
	if status.Inode == 0 {
		status.Inode = 1
	}
	stamp := reply.Modified
	if !stamp.Available {
		stamp = reply.Created
	}
	if stamp.Available {
		status.Seconds, status.Nanoseconds = stamp.Seconds, stamp.Nanoseconds
	}
	return status, 0
}

// StreamStatus describes a standard stream: a FIFO only its holder may use.
// index tells one open stream of a process from another.
func StreamStatus(index int) FileStatus {
	return FileStatus{Device: StreamDevice, Inode: uint64(index) + 1, Mode: abi.ModeFIFO | 0o600}
}

// EncodeStat writes status as a Linux x86_64 struct stat. Every byte of
// target is set, so no stale kernel data reaches the program.
func EncodeStat(target *[StatSize]byte, status FileStatus) {
	*target = [StatSize]byte{}
	put := binary.LittleEndian.PutUint64
	put(target[statDevice:], status.Device)
	put(target[statInode:], status.Inode)
	put(target[statLinks:], 1)
	binary.LittleEndian.PutUint32(target[statMode:], status.Mode)
	binary.LittleEndian.PutUint32(target[statUID:], StatUID)
	binary.LittleEndian.PutUint32(target[statGID:], StatGID)
	put(target[statRawDevice:], 0)
	put(target[statLength:], status.Size)
	put(target[statBlockSize:], StatBlockSize)
	blocks := status.Size / statBlockUnits
	if status.Size%statBlockUnits != 0 {
		blocks++
	}
	put(target[statBlocks:], blocks)
	for _, offset := range [...]int{statAccessed, statModified, statChanged} {
		put(target[offset:], uint64(status.Seconds))
		put(target[offset+8:], uint64(status.Nanoseconds))
	}
}

// StatFlags checks the flags of newfstatat. AT_SYMLINK_NOFOLLOW and
// AT_NO_AUTOMOUNT change nothing: GoOS has no symbolic links and no
// automounts. emptyPath reports AT_EMPTY_PATH, which makes an empty path
// name the descriptor itself.
func StatFlags(flags uint64) (emptyPath bool, errno abi.Errno) {
	if flags&^(abi.AtSymlinkNoFollow|abi.AtNoAutomount|abi.AtEmptyPath) != 0 {
		return false, abi.EINVAL
	}
	return flags&abi.AtEmptyPath != 0, 0
}
