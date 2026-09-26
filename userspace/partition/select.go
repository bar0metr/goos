package partition

// SelectorKind says how a volume is chosen from a table.
type SelectorKind uint8

const (
	// SelectAuto takes the whole device when it has no table, the first
	// MBR partition of a FAT32 type, or the first GPT basic-data partition.
	// An EFI system partition is never chosen automatically: it belongs to
	// the firmware.
	SelectAuto SelectorKind = iota
	// SelectWhole takes the whole device, table or not, as a checker of a
	// superfloppy volume needs.
	SelectWhole
	// SelectIndex takes the partition with the given number, from 1.
	SelectIndex
	// SelectGUID takes the GPT partition with the given unique GUID.
	SelectGUID
	// SelectName takes the first GPT partition with the given name.
	SelectName
)

// Selector names the volume to use.
type Selector struct {
	Kind     SelectorKind
	Index    uint32
	GUID     GUID
	Name     [MaxName]byte
	NameSize uint8
}

// Volume is a byte range of the device holding one volume.
type Volume struct {
	Start  uint64
	Length uint64
	// Index is the partition number, or zero for the whole device.
	Index uint32
}

// Select resolves a selector against a scanned table. Selecting by number,
// GUID or name takes any partition of any type — the filesystem decides
// whether it can use it — except an MBR extended container. Logical
// partitions are normal selectable entries numbered from five.
func Select(table *Table, selector *Selector) (Volume, Status) {
	whole := Volume{Start: 0, Length: table.Capacity}
	switch selector.Kind {
	case SelectWhole:
		return whole, StatusOK
	case SelectAuto:
		if table.Scheme == SchemeNone || table.Scheme == SchemeRaw {
			return whole, StatusOK
		}
		for index := 0; index < table.Count; index++ {
			if table.Partitions[index].Kind == KindData {
				return volumeOf(&table.Partitions[index])
			}
		}
		return Volume{}, StatusNotFound
	case SelectIndex:
		for index := 0; index < table.Count; index++ {
			if table.Partitions[index].Index == selector.Index {
				return volumeOf(&table.Partitions[index])
			}
		}
		return Volume{}, StatusNotFound
	case SelectGUID:
		for index := 0; index < table.Count && table.Scheme == SchemeGPT; index++ {
			if table.Partitions[index].Unique == selector.GUID {
				return volumeOf(&table.Partitions[index])
			}
		}
		return Volume{}, StatusNotFound
	case SelectName:
		if selector.NameSize == 0 || int(selector.NameSize) > MaxName {
			return Volume{}, StatusInvalidArgument
		}
		for index := 0; index < table.Count && table.Scheme == SchemeGPT; index++ {
			partition := &table.Partitions[index]
			if partition.NameSize == selector.NameSize && sameBytes(partition.Name[:partition.NameSize], selector.Name[:selector.NameSize]) {
				return volumeOf(partition)
			}
		}
		return Volume{}, StatusNotFound
	}
	return Volume{}, StatusInvalidArgument
}

func volumeOf(partition *Partition) (Volume, Status) {
	if partition.Kind == KindExtended {
		return Volume{}, StatusUnsupported
	}
	return Volume{Start: partition.Start, Length: partition.Length, Index: partition.Index}, StatusOK
}

// NameSelector builds a selector for a GPT name.
func NameSelector(name string) (Selector, bool) {
	selector := Selector{Kind: SelectName}
	if len(name) == 0 || len(name) > MaxName {
		return selector, false
	}
	selector.NameSize = uint8(copy(selector.Name[:], name))
	return selector, true
}

func sameBytes(first, second []byte) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}
