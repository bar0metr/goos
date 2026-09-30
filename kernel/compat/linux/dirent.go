package linux

import (
	"encoding/binary"

	"goos/internal/limits"
	"goos/kernel/compat/linux/abi"
	"goos/kernel/ramfs"
)

// A linux_dirent64 record is a fixed header, the name, its terminator, and
// padding to an eight-byte boundary:
//
//	d_ino    8 bytes at 0
//	d_off    8 bytes at 8, the position of the next entry
//	d_reclen 2 bytes at 16
//	d_type   1 byte at 18
//	d_name   from 19, NUL-terminated
const (
	direntHeader    = 19
	direntAlignment = 8
	// MaxDirentSize is the record of the longest name GoOS stores. A kernel
	// scratch buffer of this size holds any entry.
	MaxDirentSize = (direntHeader + limits.NameMax + 1 + direntAlignment - 1) &^ (direntAlignment - 1)
	// MaxDirentExchanges bounds the VFS exchanges one getdents64 call runs:
	// one per entry, and a second for a name too long for a reply message.
	// A listing shorter than the buffer could hold is a valid answer, and
	// the next call continues it.
	MaxDirentExchanges = 128
)

// DirentSize returns the record length for a name of nameSize bytes.
func DirentSize(nameSize int) int {
	return (direntHeader + nameSize + 1 + direntAlignment - 1) &^ (direntAlignment - 1)
}

// EncodeDirent writes one record at the start of target and returns its
// length. next is the listing position after this entry, which a later call
// or an lseek resumes from. The record is never cut short: a target too small
// for all of it gets nothing. Zero is no usable inode, because readers take
// it for a deleted entry.
func EncodeDirent(target []byte, inode uint64, next uint64, kind ramfs.Kind, name []byte) (int, bool) {
	size := DirentSize(len(name))
	if len(name) == 0 || len(name) > limits.NameMax || size > len(target) {
		return 0, false
	}
	entryType := abi.DirentRegular
	switch kind {
	case ramfs.KindFile:
	case ramfs.KindDirectory:
		entryType = abi.DirentDirectory
	default:
		return 0, false
	}
	for _, character := range name {
		if character == 0 || character == '/' {
			return 0, false
		}
	}
	if inode == 0 {
		inode = 1
	}
	record := target[:size]
	clear(record)
	binary.LittleEndian.PutUint64(record[0:], inode)
	binary.LittleEndian.PutUint64(record[8:], next)
	binary.LittleEndian.PutUint16(record[16:], uint16(size))
	record[18] = entryType
	copy(record[direntHeader:], name)
	return size, true
}
