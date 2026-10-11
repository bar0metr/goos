package logfmt

import "encoding/binary"

// FullWire adds the kernel-assigned timestamp and process to the write encoding.
// The old Encode/Decode write format is unchanged.
const FullHeader = 24
const FullWire = FullHeader + MaxWire

func EncodeRecord(record Record, target []byte) (int, bool) {
	if len(target) < FullHeader || !record.Time.Valid() {
		return 0, false
	}
	size, ok := Encode(record.Level, record.Component, record.Message, record.Fields, target[FullHeader:])
	if !ok {
		return 0, false
	}
	binary.LittleEndian.PutUint64(target[0:8], uint64(record.Time.Seconds))
	binary.LittleEndian.PutUint32(target[8:12], record.Time.Nanoseconds)
	clear(target[12:16])
	binary.LittleEndian.PutUint64(target[16:24], record.Process)
	return FullHeader + size, true
}

// DecodeRecord borrows its component, message and fields from data.
func DecodeRecord(data []byte) (Record, bool) {
	if len(data) < FullHeader {
		return Record{}, false
	}
	record, ok := Decode(data[FullHeader:])
	if !ok || binary.LittleEndian.Uint32(data[12:16]) != 0 {
		return Record{}, false
	}
	record.Time.Seconds = int64(binary.LittleEndian.Uint64(data[0:8]))
	record.Time.Nanoseconds = binary.LittleEndian.Uint32(data[8:12])
	record.Process = binary.LittleEndian.Uint64(data[16:24])
	return record, record.Time.Valid()
}
