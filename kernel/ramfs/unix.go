package ramfs

import "encoding/binary"

// UnixRecordSize is the length of Unix metadata on the wire. Both the VFS
// client protocol and the filesystem-service protocol carry this layout:
//
//	mode        2 bytes at 0
//	flags       2 bytes at 2: 1 present, 2 access time, 4 change time
//	uid         4 bytes at 4
//	gid         4 bytes at 8
//	links       4 bytes at 12
//	generation  4 bytes at 16
//	device      4 bytes at 20
//	accessed    8 bytes at 24, seconds
//	changed     8 bytes at 32, seconds
//	accessed    4 bytes at 40, nanoseconds
//	changed     4 bytes at 44, nanoseconds
const UnixRecordSize = 48

const (
	unixPresent  = 1
	unixAccessed = 2
	unixChanged  = 4
)

// EncodeUnix writes unix to the first UnixRecordSize bytes of target.
func EncodeUnix(target []byte, unix *Unix) bool {
	if len(target) < UnixRecordSize {
		return false
	}
	record := target[:UnixRecordSize]
	clear(record)
	if !unix.Available {
		return true
	}
	flags := uint16(unixPresent)
	if unix.Accessed.Available {
		flags |= unixAccessed
	}
	if unix.Changed.Available {
		flags |= unixChanged
	}
	binary.LittleEndian.PutUint16(record[0:], unix.Mode)
	binary.LittleEndian.PutUint16(record[2:], flags)
	binary.LittleEndian.PutUint32(record[4:], unix.UID)
	binary.LittleEndian.PutUint32(record[8:], unix.GID)
	binary.LittleEndian.PutUint32(record[12:], unix.Links)
	binary.LittleEndian.PutUint32(record[16:], unix.Generation)
	binary.LittleEndian.PutUint32(record[20:], unix.Device)
	binary.LittleEndian.PutUint64(record[24:], uint64(unix.Accessed.Seconds))
	binary.LittleEndian.PutUint64(record[32:], uint64(unix.Changed.Seconds))
	binary.LittleEndian.PutUint32(record[40:], unix.Accessed.Nanoseconds)
	binary.LittleEndian.PutUint32(record[44:], unix.Changed.Nanoseconds)
	return true
}

// DecodeUnix reads a record EncodeUnix wrote and checks it against kind.
func DecodeUnix(data []byte, kind Kind) (Unix, bool) {
	if len(data) < UnixRecordSize {
		return Unix{}, false
	}
	flags := binary.LittleEndian.Uint16(data[2:])
	if flags&^uint16(unixPresent|unixAccessed|unixChanged) != 0 || flags != 0 && flags&unixPresent == 0 {
		return Unix{}, false
	}
	unix := Unix{
		Mode:       binary.LittleEndian.Uint16(data[0:]),
		UID:        binary.LittleEndian.Uint32(data[4:]),
		GID:        binary.LittleEndian.Uint32(data[8:]),
		Links:      binary.LittleEndian.Uint32(data[12:]),
		Generation: binary.LittleEndian.Uint32(data[16:]),
		Device:     binary.LittleEndian.Uint32(data[20:]),
		Accessed:   Timestamp{Seconds: int64(binary.LittleEndian.Uint64(data[24:])), Nanoseconds: binary.LittleEndian.Uint32(data[40:]), Available: flags&unixAccessed != 0},
		Changed:    Timestamp{Seconds: int64(binary.LittleEndian.Uint64(data[32:])), Nanoseconds: binary.LittleEndian.Uint32(data[44:]), Available: flags&unixChanged != 0},
		Available:  flags&unixPresent != 0,
	}
	if !unix.Valid(kind) {
		return Unix{}, false
	}
	return unix, true
}
