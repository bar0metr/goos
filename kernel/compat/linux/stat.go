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

// FileStatus is the part of struct stat and struct statx GoOS has an answer
// for.
type FileStatus struct {
	Device      uint64
	Inode       uint64
	Mode        uint32
	Size        uint64
	Seconds     int64
	Nanoseconds uint32
	// RawDevice is st_rdev: the device number of a character device and zero
	// for everything else.
	RawDevice uint64
	// Born reports that the filesystem recorded a creation time, which only
	// statx has a field for.
	Born            bool
	BirthSeconds    int64
	BirthNanosecond uint32
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
	if reply.Created.Available {
		status.Born, status.BirthSeconds, status.BirthNanosecond = true, reply.Created.Seconds, reply.Created.Nanoseconds
	}
	return status, 0
}

// StreamStatus describes a stream: a FIFO only its holders may use. The
// stream identity is the inode, so both ends of a pipe report the same one.
func StreamStatus(identity uint64) FileStatus {
	return FileStatus{Device: StreamDevice, Inode: identity, Mode: abi.ModeFIFO | 0o600}
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
	put(target[statRawDevice:], status.RawDevice)
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

// AccessErrno answers access and faccessat from the mode stat reports, as
// the file's owner, which every Linux process here is. The answer therefore
// always agrees with st_mode. requested is a mask of R_OK (4), W_OK (2), and
// X_OK (1); zero asks only whether the file exists.
func AccessErrno(requested uint64, mode uint32) abi.Errno {
	if requested&^7 != 0 {
		return abi.EINVAL
	}
	if uint32(requested)&^(mode>>6&7) != 0 {
		return abi.EACCES
	}
	return 0
}

// StatxSize is the size of struct statx, which is the same on every Linux
// architecture. The offsets below come from include/uapi/linux/stat.h.
const StatxSize = 256

const (
	statxMask        = 0   // stx_mask, 4 bytes
	statxBlockSize   = 4   // stx_blksize, 4
	statxLinks       = 16  // stx_nlink, 4; stx_attributes before it stays zero
	statxUID         = 20  // stx_uid, 4
	statxGID         = 24  // stx_gid, 4
	statxMode        = 28  // stx_mode, 2; 2 bytes of padding follow
	statxInode       = 32  // stx_ino, 8
	statxLength      = 40  // stx_size, 8
	statxBlocks      = 48  // stx_blocks, 8; stx_attributes_mask follows, zero
	statxAccessed    = 64  // stx_atime: 8 bytes of seconds, 4 of nanoseconds, 4 unused
	statxBorn        = 80  // stx_btime
	statxChanged     = 96  // stx_ctime
	statxModified    = 112 // stx_mtime
	statxRawMajor    = 128 // stx_rdev_major, 4
	statxRawMinor    = 132 // stx_rdev_minor, 4
	statxDeviceMajor = 136 // stx_dev_major, 4
	statxDeviceMinor = 140 // stx_dev_minor, 4; the rest stays zero
)

// StatxFlags checks the flags and the requested mask of statx. The sync
// flags change nothing, because a GoOS stat always asks the filesystem. The
// mask is a request Linux may exceed, and GoOS returns the same fields
// whatever it names. emptyPath is AT_EMPTY_PATH, as in StatFlags.
func StatxFlags(flags uint64, mask uint64) (emptyPath bool, errno abi.Errno) {
	if mask&abi.StatxReservedMask != 0 || flags&abi.AtStatxSyncType == abi.AtStatxSyncType {
		return false, abi.EINVAL
	}
	return StatFlags(flags &^ abi.AtStatxSyncType)
}

// DeviceParts splits a device number the way the C library's major() and
// minor() do, so makedev of the two halves gives the st_dev of the same file.
func DeviceParts(device uint64) (major uint32, minor uint32) {
	major = uint32(device>>32&0xfffff000 | device>>8&0xfff)
	minor = uint32(device>>12&0xffffff00 | device&0xff)
	return major, minor
}

// EncodeStatx writes status as a struct statx. Every byte of target is set.
// stx_mask names the basic fields, and the birth time when the filesystem
// recorded one.
func EncodeStatx(target *[StatxSize]byte, status FileStatus) {
	*target = [StatxSize]byte{}
	put32, put64 := binary.LittleEndian.PutUint32, binary.LittleEndian.PutUint64
	mask := abi.StatxBasicStats
	if status.Born {
		mask |= abi.StatxBirthTime
		put64(target[statxBorn:], uint64(status.BirthSeconds))
		put32(target[statxBorn+8:], status.BirthNanosecond)
	}
	put32(target[statxMask:], mask)
	put32(target[statxBlockSize:], StatBlockSize)
	put32(target[statxLinks:], 1)
	put32(target[statxUID:], StatUID)
	put32(target[statxGID:], StatGID)
	binary.LittleEndian.PutUint16(target[statxMode:], uint16(status.Mode))
	put64(target[statxInode:], status.Inode)
	put64(target[statxLength:], status.Size)
	blocks := status.Size / statBlockUnits
	if status.Size%statBlockUnits != 0 {
		blocks++
	}
	put64(target[statxBlocks:], blocks)
	for _, offset := range [...]int{statxAccessed, statxChanged, statxModified} {
		put64(target[offset:], uint64(status.Seconds))
		put32(target[offset+8:], status.Nanoseconds)
	}
	major, minor := DeviceParts(status.RawDevice)
	put32(target[statxRawMajor:], major)
	put32(target[statxRawMinor:], minor)
	major, minor = DeviceParts(status.Device)
	put32(target[statxDeviceMajor:], major)
	put32(target[statxDeviceMinor:], minor)
}
