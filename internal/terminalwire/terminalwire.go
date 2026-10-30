// Package terminalwire is the fixed record PID 1 writes to the control
// stream of a Linux terminal. It carries geometry only, needs no allocation,
// and is shared by the userspace writer and the kernel reader so neither
// imports the other's terminal packages.
package terminalwire

import "encoding/binary"

// RecordSize is the size of one record. It is far below one atomic stream
// write, so a reader never sees half of one.
const RecordSize = 16

// Version is the record layout. A reader drops a record of another version.
const Version = 1

const magic0, magic1 = 'G', 'T'

// Geometry is the size of the terminal in character cells. Sequence orders
// the records: a reader keeps the newest.
type Geometry struct {
	Rows, Columns uint16
	Sequence      uint64
}

// Valid reports whether the geometry names a usable terminal.
func (geometry Geometry) Valid() bool {
	return geometry.Rows != 0 && geometry.Columns != 0
}

// Encode writes one record: the magic, the version, two reserved zero
// bytes, rows, columns, and the sequence, little endian.
func Encode(geometry Geometry, target *[RecordSize]byte) bool {
	if !geometry.Valid() {
		return false
	}
	*target = [RecordSize]byte{}
	target[0], target[1], target[2] = magic0, magic1, Version
	binary.LittleEndian.PutUint16(target[4:6], geometry.Rows)
	binary.LittleEndian.PutUint16(target[6:8], geometry.Columns)
	binary.LittleEndian.PutUint64(target[8:16], geometry.Sequence)
	return true
}

// Decode reads one record.
func Decode(record []byte) (Geometry, bool) {
	if len(record) != RecordSize || record[0] != magic0 || record[1] != magic1 || record[2] != Version || record[3] != 0 {
		return Geometry{}, false
	}
	geometry := Geometry{
		Rows:     binary.LittleEndian.Uint16(record[4:6]),
		Columns:  binary.LittleEndian.Uint16(record[6:8]),
		Sequence: binary.LittleEndian.Uint64(record[8:16]),
	}
	return geometry, geometry.Valid()
}
