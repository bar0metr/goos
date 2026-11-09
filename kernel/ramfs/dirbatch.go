package ramfs

import "encoding/binary"

// A directory batch is a run of records in shared memory, one per entry
// (ADR 0208). The filesystem service and the VFS use the same format. A record
// is a fixed header, the name, and padding to an eight-byte boundary:
//
//	id          8 bytes at 0
//	size        8 bytes at 8
//	created     8 bytes at 16, seconds
//	modified    8 bytes at 24, seconds
//	created     4 bytes at 32, nanoseconds
//	modified    4 bytes at 36, nanoseconds
//	mount       8 bytes at 40
//	next        4 bytes at 48, the listing position after this entry
//	name length 2 bytes at 52
//	kind        1 byte at 54
//	attributes  1 byte at 55
//	metadata    1 byte at 56
//	reserved    7 bytes at 57, zero
const (
	// MaxDirBatch is the most bytes of records one exchange carries. A
	// maximum-length name fits, and one page leaves the Linux bridge able to
	// read a batch from the first frame of its I/O buffer.
	MaxDirBatch = 4096

	dirRecordHeader    = 64
	dirRecordAlignment = 8

	// MaxDirRecord is the record of the longest name.
	MaxDirRecord = (dirRecordHeader + MaxName + dirRecordAlignment - 1) &^ (dirRecordAlignment - 1)

	dirCreatedAvailable    = 1
	dirModifiedAvailable   = 2
	dirAttributesAvailable = 4
)

// DirBatch describes the records a listing call wrote. Next is the position to
// continue from. End means nothing follows the records; without it, the next
// call continues at Next, and may report a failure this call stopped before.
type DirBatch struct {
	Used  int
	Count int
	Next  int
	End   bool
}

// DirRecord is one decoded record. Name aliases the decoded memory.
type DirRecord struct {
	Stat Stat
	Next int
	Name []byte
}

// DirRecordSize returns the record length for a name of nameSize bytes.
func DirRecordSize(nameSize int) int {
	return (dirRecordHeader + nameSize + dirRecordAlignment - 1) &^ (dirRecordAlignment - 1)
}

// AppendDirRecord writes one record at the start of target and returns its
// length, or zero when the record is invalid or does not fit.
func AppendDirRecord(target []byte, stat *Stat, next int, name []byte) int {
	size := DirRecordSize(len(name))
	if len(name) == 0 || len(name) > MaxName || size > len(target) || next < 0 || uint64(next) > uint64(^uint32(0)) ||
		!validDirStat(stat) {
		return 0
	}
	record := target[:size]
	clear(record)
	binary.LittleEndian.PutUint64(record[0:], uint64(stat.ID))
	binary.LittleEndian.PutUint64(record[8:], stat.Size)
	binary.LittleEndian.PutUint64(record[16:], uint64(stat.Created.Seconds))
	binary.LittleEndian.PutUint64(record[24:], uint64(stat.Modified.Seconds))
	binary.LittleEndian.PutUint32(record[32:], stat.Created.Nanoseconds)
	binary.LittleEndian.PutUint32(record[36:], stat.Modified.Nanoseconds)
	binary.LittleEndian.PutUint64(record[40:], stat.MountID)
	binary.LittleEndian.PutUint32(record[48:], uint32(next))
	binary.LittleEndian.PutUint16(record[52:], uint16(len(name)))
	record[54] = byte(stat.Kind)
	record[55] = byte(stat.Attributes)
	if stat.Created.Available {
		record[56] |= dirCreatedAvailable
	}
	if stat.Modified.Available {
		record[56] |= dirModifiedAvailable
	}
	if stat.AttributesAvailable {
		record[56] |= dirAttributesAvailable
	}
	copy(record[dirRecordHeader:], name)
	return size
}

// Decode reads the record at the start of data and returns its length, or
// false when the record is malformed. The caller owns data: decoding memory a
// peer can still write would check one value and use another.
func (record *DirRecord) Decode(data []byte) (int, bool) {
	if len(data) < dirRecordHeader {
		return 0, false
	}
	nameSize := int(binary.LittleEndian.Uint16(data[52:]))
	size := DirRecordSize(nameSize)
	metadata := data[56]
	if nameSize == 0 || nameSize > MaxName || size > len(data) || metadata&^byte(dirCreatedAvailable|dirModifiedAvailable|dirAttributesAvailable) != 0 ||
		binary.LittleEndian.Uint32(data[57:]) != 0 || binary.LittleEndian.Uint32(data[60:]) != 0 {
		return 0, false
	}
	record.Stat = Stat{
		ID:                  NodeID(binary.LittleEndian.Uint64(data[0:])),
		Kind:                Kind(data[54]),
		Size:                binary.LittleEndian.Uint64(data[8:]),
		Created:             Timestamp{Seconds: int64(binary.LittleEndian.Uint64(data[16:])), Nanoseconds: binary.LittleEndian.Uint32(data[32:]), Available: metadata&dirCreatedAvailable != 0},
		Modified:            Timestamp{Seconds: int64(binary.LittleEndian.Uint64(data[24:])), Nanoseconds: binary.LittleEndian.Uint32(data[36:]), Available: metadata&dirModifiedAvailable != 0},
		Attributes:          Attributes(data[55]),
		AttributesAvailable: metadata&dirAttributesAvailable != 0,
		MountID:             binary.LittleEndian.Uint64(data[40:]),
	}
	if !validDirStat(&record.Stat) {
		return 0, false
	}
	record.Next = int(binary.LittleEndian.Uint32(data[48:]))
	record.Name = data[dirRecordHeader : dirRecordHeader+nameSize]
	return size, true
}

// ValidDirBatch reports whether data holds exactly count well-formed records
// whose positions rise from cursor and end at next.
func ValidDirBatch(data []byte, count, cursor, next int) bool {
	var record DirRecord
	for ; count > 0; count-- {
		size, ok := record.Decode(data)
		if !ok || record.Next <= cursor {
			return false
		}
		data, cursor = data[size:], record.Next
	}
	return len(data) == 0 && cursor == next
}

func validDirStat(stat *Stat) bool {
	return stat.Kind >= KindFile && stat.Kind <= LastKind && stat.Created.Valid() && stat.Modified.Valid() &&
		stat.Attributes <= LastAttribute*2-1 && (stat.AttributesAvailable || stat.Attributes == 0)
}
