package acpi

import "encoding/binary"

const MaxProcessors = 256

type Processor struct {
	APICID uint32
	UID    uint32
	X2APIC bool
}

type Processors struct {
	Entries [MaxProcessors]Processor
	Count   uint16
}

// FindProcessors returns the enabled processors described by the MADT. The
// table is rejected in full when an entry is malformed, an APIC ID is
// duplicated, or the fixed inventory bound would be exceeded.
func FindProcessors(memory Memory, root uint64) (Processors, bool) {
	var result Processors
	ok := FindProcessorsInto(&result, memory, root)
	return result, ok
}

// FindProcessorsInto is the stack-bounded kernel form of FindProcessors.
//
//go:noinline
func FindProcessorsInto(result *Processors, memory Memory, root uint64) bool {
	*result = Processors{}
	table, ok := findTable(memory, root, "APIC")
	if !ok || len(table) < headerSize+8 {
		return false
	}
	for offset := headerSize + 8; offset < len(table); {
		if offset+2 > len(table) {
			return false
		}
		entryType, length := table[offset], int(table[offset+1])
		if length < 2 || offset+length < offset || offset+length > len(table) {
			return false
		}
		var processor Processor
		var enabled, known bool
		switch entryType {
		case 0:
			if length < 8 {
				return false
			}
			known = true
			processor.UID = uint32(table[offset+2])
			processor.APICID = uint32(table[offset+3])
			enabled = binary.LittleEndian.Uint32(table[offset+4:])&1 != 0
		case 9:
			if length < 16 {
				return false
			}
			known = true
			processor.APICID = binary.LittleEndian.Uint32(table[offset+4:])
			enabled = binary.LittleEndian.Uint32(table[offset+8:])&1 != 0
			processor.UID = binary.LittleEndian.Uint32(table[offset+12:])
			processor.X2APIC = true
		}
		if known && enabled {
			if result.Count >= MaxProcessors || containsAPIC(*result, processor.APICID) {
				return false
			}
			result.Entries[result.Count] = processor
			result.Count++
		}
		offset += length
	}
	return true
}

func containsAPIC(processors Processors, apicID uint32) bool {
	for index := uint16(0); index < processors.Count; index++ {
		if processors.Entries[index].APICID == apicID {
			return true
		}
	}
	return false
}
