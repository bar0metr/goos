package partition

// The GUID partition table: a header at LBA 1 and a backup at the last LBA,
// each naming an array of entries elsewhere on the disk, each checked by a
// CRC-32.

const (
	gptSignature     = "EFI PART"
	gptRevisionMajor = 0x0001
	gptMinHeader     = 92
	gptMinEntry      = 128
	// gptMaxEntries and gptMaxArray bound the entry array a header may
	// declare: every table in practice is 128 entries of 128 bytes, 16 KiB.
	gptMaxEntries = 4096
	gptMaxArray   = 1 << 20

	gptEntryName = 56
	gptNameUnits = 36
)

// gptHeader is a validated header.
type gptHeader struct {
	self        uint64
	alternate   uint64
	firstUsable uint64
	lastUsable  uint64
	disk        GUID
	entryLBA    uint64
	entries     uint32
	entrySize   uint32
	entriesCRC  uint32
}

// readGPTHeader reads and validates the header at lba, which must name
// itself. Every range it declares must lie inside the device, and the entry
// array must stay clear of LBA 0, both headers and the usable range.
func (scanner *Scanner) readGPTHeader(reader Reader, lba uint64, table *Table) (gptHeader, bool) {
	var header gptHeader
	size := uint64(table.BlockSize)
	blocks := table.Capacity / size
	data := scanner.block[:size]
	if lba == 0 || lba >= blocks || !reader.ReadAt(lba*size, data) {
		return header, false
	}
	headerSize := uint64(get32(data[12:]))
	if string(data[:8]) != gptSignature || get16(data[10:]) != gptRevisionMajor ||
		headerSize < gptMinHeader || headerSize > size || get32(data[20:]) != 0 {
		return header, false
	}
	stored := get32(data[16:])
	put32(data[16:], 0)
	if crc32Update(0, data[:headerSize]) != stored {
		return header, false
	}
	header.self, header.alternate = get64(data[24:]), get64(data[32:])
	header.firstUsable, header.lastUsable = get64(data[40:]), get64(data[48:])
	copy(header.disk[:], data[56:72])
	header.entryLBA, header.entries, header.entrySize, header.entriesCRC = get64(data[72:]), get32(data[80:]), get32(data[84:]), get32(data[88:])
	entrySize := uint64(header.entrySize)
	if header.self != lba || header.alternate == lba || header.alternate >= blocks ||
		header.firstUsable > header.lastUsable || header.lastUsable >= blocks || header.firstUsable == 0 ||
		header.entries == 0 || header.entries > gptMaxEntries ||
		entrySize < gptMinEntry || entrySize > maxBlockSize || entrySize&(entrySize-1) != 0 ||
		uint64(header.entries)*entrySize > gptMaxArray {
		return header, false
	}
	// The usable range holds partitions only: neither header lies in it.
	if inRange(lba, header.firstUsable, header.lastUsable) || inRange(header.alternate, header.firstUsable, header.lastUsable) {
		return header, false
	}
	arrayBlocks := (uint64(header.entries)*entrySize + size - 1) / size
	if header.entryLBA == 0 || header.entryLBA >= blocks || arrayBlocks > blocks-header.entryLBA ||
		overlaps(header.entryLBA, arrayBlocks, header.firstUsable, header.lastUsable-header.firstUsable+1) ||
		overlaps(header.entryLBA, arrayBlocks, lba, 1) || overlaps(header.entryLBA, arrayBlocks, header.alternate, 1) {
		return header, false
	}
	return header, true
}

func inRange(value, first, last uint64) bool { return value >= first && value <= last }

// readGPTEntries reads the entry array a header names and checks its CRC.
// When add is set, a second pass adds the used entries to the table, each
// checked to lie in the usable range and apart from the others: the CRC comes
// first, so nothing is judged from an array that is merely damaged, and a
// table whose CRC matches but whose entries do not fit is consistently wrong,
// and so corrupt.
func (scanner *Scanner) readGPTEntries(reader Reader, header *gptHeader, table *Table, add bool) Status {
	size := uint64(header.entrySize)
	total := uint64(header.entries) * size
	base := header.entryLBA * uint64(table.BlockSize)
	crc := uint32(0)
	for done := uint64(0); done < total; {
		chunk := min(uint64(len(scanner.entries)), total-done)
		if !reader.ReadAt(base+done, scanner.entries[:chunk]) {
			return StatusIOError
		}
		crc = crc32Update(crc, scanner.entries[:chunk])
		done += chunk
	}
	if crc != header.entriesCRC {
		return StatusCorrupt
	}
	index := uint32(0)
	for done := uint64(0); done < total && add; {
		chunk := min(uint64(len(scanner.entries)), total-done)
		data := scanner.entries[:chunk]
		if !reader.ReadAt(base+done, data) {
			return StatusIOError
		}
		for offset := uint64(0); offset < chunk; offset += size {
			index++
			if status := addGPTEntry(data[offset:offset+size], index, header, table); status != StatusOK {
				return status
			}
		}
		done += chunk
	}
	return StatusOK
}

func addGPTEntry(entry []byte, index uint32, header *gptHeader, table *Table) Status {
	var kind GUID
	copy(kind[:], entry[:16])
	if kind.zero() {
		return StatusOK
	}
	first, last := get64(entry[32:]), get64(entry[40:])
	if first > last || first < header.firstUsable || last > header.lastUsable {
		return StatusCorrupt
	}
	count := last - first + 1
	block := uint64(table.BlockSize)
	for other := 0; other < table.Count; other++ {
		previous := &table.Partitions[other]
		if overlaps(first, count, previous.Start/block, previous.Length/block) {
			return StatusCorrupt
		}
	}
	partition, ok := table.add()
	if !ok {
		return StatusUnsupported
	}
	partition.Index, partition.Start, partition.Length = index, first*block, count*block
	partition.Type = kind
	copy(partition.Unique[:], entry[16:32])
	switch kind {
	case TypeBasicData:
		partition.Kind = KindData
	case TypeEFISystem:
		partition.Kind = KindEFI
	}
	partition.NameSize = gptName(entry[gptEntryName:gptEntryName+2*gptNameUnits], partition.Name[:])
	return StatusOK
}

// gptName converts a NUL-terminated UTF-16LE name to UTF-8. A name that is not
// valid UTF-16 is left empty: the partition is still reachable by number and
// GUID.
func gptName(data []byte, target []byte) uint8 {
	size := 0
	for index := 0; index < gptNameUnits; index++ {
		value := rune(get16(data[2*index:]))
		if value == 0 {
			break
		}
		switch {
		case value >= 0xdc00 && value < 0xe000:
			return 0
		case value >= 0xd800 && value < 0xdc00:
			if index+1 >= gptNameUnits {
				return 0
			}
			low := rune(get16(data[2*index+2:]))
			if low < 0xdc00 || low >= 0xe000 {
				return 0
			}
			value = 0x10000 + (value-0xd800)<<10 + (low - 0xdc00)
			index++
		}
		if value < 0x20 {
			return 0
		}
		size += encodeRune(target[size:], value)
	}
	return uint8(size)
}

func encodeRune(target []byte, value rune) int {
	switch {
	case value < 0x80:
		target[0] = byte(value)
		return 1
	case value < 0x800:
		target[0], target[1] = 0xc0|byte(value>>6), 0x80|byte(value)&0x3f
		return 2
	case value < 0x10000:
		target[0], target[1], target[2] = 0xe0|byte(value>>12), 0x80|byte(value>>6)&0x3f, 0x80|byte(value)&0x3f
		return 3
	}
	target[0], target[1] = 0xf0|byte(value>>18), 0x80|byte(value>>12)&0x3f
	target[2], target[3] = 0x80|byte(value>>6)&0x3f, 0x80|byte(value)&0x3f
	return 4
}

// scanGPT validates both copies of the GPT. The primary governs when it is
// valid, header and entries; otherwise the backup — found where the primary
// says, or at the last block — is used and the table reported degraded.
// Neither copy is rewritten. A copy that is valid but disagrees with the
// other is reported, not merged.
func (scanner *Scanner) scanGPT(reader Reader, table *Table) Status {
	blocks := table.Capacity / uint64(table.BlockSize)
	last := blocks - 1
	primary, primaryOK := scanner.readGPTHeader(reader, 1, table)
	if primaryOK {
		switch status := scanner.readGPTEntries(reader, &primary, table, true); status {
		case StatusOK:
		case StatusIOError, StatusUnsupported:
			return status
		default:
			primaryOK = false
			table.Count = 0
		}
	}
	backupAt := last
	if primaryOK {
		backupAt = primary.alternate
	}
	backup, backupOK := scanner.readGPTHeader(reader, backupAt, table)
	if backupOK && backup.alternate != 1 {
		backupOK = false
	}
	if backupOK {
		switch status := scanner.readGPTEntries(reader, &backup, table, !primaryOK); status {
		case StatusOK:
		case StatusIOError, StatusUnsupported:
			return status
		default:
			backupOK = false
			if !primaryOK {
				table.Count = 0
			}
		}
	}
	switch {
	case primaryOK:
		table.DiskGUID = primary.disk
		table.GPT.BackupInvalid = !backupOK
		table.GPT.BackupDiffers = backupOK && !sameGPT(&primary, &backup)
		table.GPT.BackupMisplaced = primary.alternate != last
	case backupOK:
		table.DiskGUID = backup.disk
		table.GPT.UsedBackup = true
		table.GPT.BackupMisplaced = backupAt != last
	default:
		table.Count = 0
		return StatusCorrupt
	}
	table.Scheme = SchemeGPT
	return StatusOK
}

// sameGPT reports whether two headers describe the same disk and entries.
func sameGPT(first, second *gptHeader) bool {
	return first.disk == second.disk && first.firstUsable == second.firstUsable && first.lastUsable == second.lastUsable &&
		first.entries == second.entries && first.entrySize == second.entrySize && first.entriesCRC == second.entriesCRC
}
