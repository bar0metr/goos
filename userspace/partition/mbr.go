package partition

// The MBR: four 16-byte entries at byte 446 of LBA 0.

const (
	mbrTable     = 446
	mbrEntrySize = 16

	mbrTypeEmpty      = 0x00
	mbrTypeProtective = 0xee
)

type mbrKind uint8

const (
	mbrUnused mbrKind = iota
	mbrUsed
	mbrProtective
	// mbrInvalid: a boot indicator other than 0x00 or 0x80, which no
	// partition table has; the sector is then not an MBR.
	mbrInvalid
)

type mbrEntry struct {
	kind  mbrKind
	boot  byte
	kind8 byte
	start uint32
	count uint32
}

func readMBREntry(data []byte) mbrEntry {
	entry := mbrEntry{boot: data[0], kind8: data[4], start: get32(data[8:]), count: get32(data[12:])}
	switch {
	case entry.boot != 0x00 && entry.boot != 0x80:
		entry.kind = mbrInvalid
	case entry.kind8 == mbrTypeEmpty:
		entry.kind = mbrUnused
	case entry.kind8 == mbrTypeProtective && entry.start == 1:
		entry.kind = mbrProtective
	default:
		entry.kind = mbrUsed
	}
	return entry
}

// mbrTypeKind classifies an MBR type byte.
func mbrTypeKind(value byte) Kind {
	switch value {
	case 0x0b, 0x0c, 0x1b, 0x1c:
		return KindData
	case 0xef:
		return KindEFI
	case 0x05, 0x0f, 0x85:
		return KindExtended
	}
	return KindOther
}

// scanMBR accepts the four primary entries. A sector whose boot indicators
// are not 0x00 or 0x80 is not a partition table, and neither is one with no
// used entry: either way the whole device is left to the filesystem. Used
// entries must start after LBA 0, lie inside the device, and not overlap.
func scanMBR(entries *[4]mbrEntry, table *Table) Status {
	used := 0
	for slot := range entries {
		switch entries[slot].kind {
		case mbrInvalid:
			return StatusOK
		case mbrUsed:
			used++
		}
	}
	if used == 0 {
		return StatusOK
	}
	blocks := table.Capacity / uint64(table.BlockSize)
	for slot := range entries {
		entry := &entries[slot]
		if entry.kind != mbrUsed {
			continue
		}
		start, count := uint64(entry.start), uint64(entry.count)
		if start == 0 || count == 0 || start > blocks || count > blocks-start {
			return StatusCorrupt
		}
		for other := 0; other < slot; other++ {
			previous := &entries[other]
			if previous.kind == mbrUsed && overlaps(start, count, uint64(previous.start), uint64(previous.count)) {
				return StatusCorrupt
			}
		}
		partition, _ := table.add()
		partition.Index = uint32(slot + 1)
		partition.Start, partition.Length = start*uint64(table.BlockSize), count*uint64(table.BlockSize)
		partition.MBRType, partition.Kind = entry.kind8, mbrTypeKind(entry.kind8)
	}
	table.Scheme = SchemeMBR
	return StatusOK
}

// overlaps reports whether two block ranges share a block.
func overlaps(start, count, otherStart, otherCount uint64) bool {
	return start < otherStart+otherCount && otherStart < start+count
}
