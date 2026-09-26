package partition

// Writing tables, for the tools that build disk images and for tests. Nothing
// in the running system writes a partition table.

// Writer writes bytes at a byte offset of a device.
type Writer interface {
	WriteAt(offset uint64, data []byte) bool
}

// MBRPartition describes one primary MBR entry, in blocks.
type MBRPartition struct {
	Type   byte
	Start  uint32
	Count  uint32
	Active bool
}

// WriteMBR writes LBA 0 as an MBR holding up to four entries, with no boot
// code. The entries are not checked: tests use it to build broken tables.
func WriteMBR(writer Writer, blockSize uint32, partitions []MBRPartition) bool {
	if len(partitions) > 4 || blockSize < minBlockSize || blockSize > maxBlockSize {
		return false
	}
	var sector [maxBlockSize]byte
	for slot, partition := range partitions {
		putMBREntry(sector[mbrTable+slot*mbrEntrySize:], partition)
	}
	sector[510], sector[511] = 0x55, 0xaa
	return writer.WriteAt(0, sector[:blockSize])
}

func putMBREntry(entry []byte, partition MBRPartition) {
	if partition.Active {
		entry[0] = 0x80
	}
	// CHS addresses are meaningless on disks of this size; like every
	// current tool, the maximum is recorded.
	entry[1], entry[2], entry[3] = 0xfe, 0xff, 0xff
	entry[4] = partition.Type
	entry[5], entry[6], entry[7] = 0xfe, 0xff, 0xff
	put32(entry[8:], partition.Start)
	put32(entry[12:], partition.Count)
}

// GPTPartition describes one GPT entry, in blocks, Last inclusive.
type GPTPartition struct {
	Type   GUID
	Unique GUID
	First  uint64
	Last   uint64
	Name   string
}

// gptWrittenEntries is the size of the entry array written: 128 entries of
// 128 bytes, as every tool writes.
const gptWrittenEntries = 128

// GPTUsable is the usable block range of a GPT written by WriteGPT on a
// device of capacity bytes.
func GPTUsable(capacity uint64, blockSize uint32) (uint64, uint64) {
	return gptUsable(capacity, blockSize, gptWrittenEntries)
}

func gptUsable(capacity uint64, blockSize uint32, slots int) (uint64, uint64) {
	size := uint64(blockSize)
	arrayBlocks := (uint64(slots)*gptMinEntry + size - 1) / size
	last := capacity/size - 1
	return 2 + arrayBlocks, last - 1 - arrayBlocks
}

// WriteGPT writes a protective MBR, the primary header and entry array at LBA
// 1 and 2, and the backup array and header at the end of the device. It
// refuses partitions outside the usable range or with names longer than 36
// UTF-16 units.
func WriteGPT(writer Writer, capacity uint64, blockSize uint32, disk GUID, partitions []GPTPartition) bool {
	return writeGPT(writer, capacity, blockSize, disk, partitions, gptWrittenEntries)
}

// writeGPT writes a GPT with slots entries in its array.
func writeGPT(writer Writer, capacity uint64, blockSize uint32, disk GUID, partitions []GPTPartition, slots int) bool {
	size := uint64(blockSize)
	if blockSize < minBlockSize || blockSize > maxBlockSize || size&(size-1) != 0 || capacity%size != 0 ||
		len(partitions) > slots || slots > gptMaxEntries || capacity/size < 128 {
		return false
	}
	blocks := capacity / size
	last := blocks - 1
	firstUsable, lastUsable := gptUsable(capacity, blockSize, slots)
	arrayBlocks := (uint64(slots)*gptMinEntry + size - 1) / size
	array := make([]byte, arrayBlocks*size)
	for index, partition := range partitions {
		entry := array[index*gptMinEntry:]
		if partition.First < firstUsable || partition.Last > lastUsable || partition.First > partition.Last ||
			!putGPTName(entry[gptEntryName:gptEntryName+2*gptNameUnits], partition.Name) {
			return false
		}
		copy(entry[:16], partition.Type[:])
		copy(entry[16:32], partition.Unique[:])
		put64(entry[32:], partition.First)
		put64(entry[40:], partition.Last)
	}
	arrayCRC := crc32Update(0, array[:slots*gptMinEntry])
	count := blocks - 1
	if count > 0xffffffff {
		count = 0xffffffff
	}
	if !WriteMBR(writer, blockSize, []MBRPartition{{Type: mbrTypeProtective, Start: 1, Count: uint32(count)}}) {
		return false
	}
	for _, copyAt := range [2]struct{ self, alternate, entries uint64 }{{1, last, 2}, {last, 1, last - arrayBlocks}} {
		var header [maxBlockSize]byte
		copy(header[:8], gptSignature)
		put32(header[8:], gptRevisionMajor<<16)
		put32(header[12:], gptMinHeader)
		put64(header[24:], copyAt.self)
		put64(header[32:], copyAt.alternate)
		put64(header[40:], firstUsable)
		put64(header[48:], lastUsable)
		copy(header[56:72], disk[:])
		put64(header[72:], copyAt.entries)
		put32(header[80:], uint32(slots))
		put32(header[84:], gptMinEntry)
		put32(header[88:], arrayCRC)
		put32(header[16:], crc32Update(0, header[:gptMinHeader]))
		if !writer.WriteAt(copyAt.entries*size, array) || !writer.WriteAt(copyAt.self*size, header[:size]) {
			return false
		}
	}
	return true
}

// putGPTName writes a name as NUL-padded UTF-16LE.
func putGPTName(target []byte, name string) bool {
	units := 0
	for rest := []byte(name); len(rest) != 0; {
		value, size := decodeRune(rest)
		if size == 0 {
			return false
		}
		rest = rest[size:]
		if value >= 0x10000 {
			if units+2 > gptNameUnits {
				return false
			}
			value -= 0x10000
			put16(target[2*units:], uint16(0xd800+value>>10))
			put16(target[2*units+2:], uint16(0xdc00+value&0x3ff))
			units += 2
			continue
		}
		if units+1 > gptNameUnits {
			return false
		}
		put16(target[2*units:], uint16(value))
		units++
	}
	return true
}

// decodeRune decodes one UTF-8 code point; size zero means invalid.
func decodeRune(data []byte) (rune, int) {
	first := data[0]
	switch {
	case first < 0x80:
		return rune(first), 1
	case first >= 0xc2 && first < 0xe0 && len(data) >= 2 && data[1]&0xc0 == 0x80:
		return rune(first&0x1f)<<6 | rune(data[1]&0x3f), 2
	case first >= 0xe0 && first < 0xf0 && len(data) >= 3 && data[1]&0xc0 == 0x80 && data[2]&0xc0 == 0x80:
		value := rune(first&0x0f)<<12 | rune(data[1]&0x3f)<<6 | rune(data[2]&0x3f)
		if value < 0x800 || value >= 0xd800 && value < 0xe000 {
			return 0, 0
		}
		return value, 3
	case first >= 0xf0 && first < 0xf5 && len(data) >= 4 && data[1]&0xc0 == 0x80 && data[2]&0xc0 == 0x80 && data[3]&0xc0 == 0x80:
		value := rune(first&0x07)<<18 | rune(data[1]&0x3f)<<12 | rune(data[2]&0x3f)<<6 | rune(data[3]&0x3f)
		if value < 0x10000 || value > 0x10ffff {
			return 0, 0
		}
		return value, 4
	}
	return 0, 0
}
