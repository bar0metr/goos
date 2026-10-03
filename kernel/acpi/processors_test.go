package acpi

import (
	"encoding/binary"
	"testing"
)

func madt(entries ...[]byte) []byte {
	body := make([]byte, 8)
	for _, entry := range entries {
		body = append(body, entry...)
	}
	return table("APIC", body)
}

func lapic(uid, id byte, flags uint32) []byte {
	entry := make([]byte, 8)
	entry[0], entry[1], entry[2], entry[3] = 0, 8, uid, id
	binary.LittleEndian.PutUint32(entry[4:], flags)
	return entry
}

func x2apic(uid, id, flags uint32) []byte {
	entry := make([]byte, 16)
	entry[0], entry[1] = 9, 16
	binary.LittleEndian.PutUint32(entry[4:], id)
	binary.LittleEndian.PutUint32(entry[8:], flags)
	binary.LittleEndian.PutUint32(entry[12:], uid)
	return entry
}

func processorSystem(entries ...[]byte) fakeMemory {
	memory := system(2, []byte{0x08, '_', 'S', '5', '_', 0x12, 0x06, 0x02, 0x01, 0x01})
	memory[0x6000] = madt(entries...)
	return memory
}

func TestFindProcessors(t *testing.T) {
	processors, ok := FindProcessors(processorSystem(
		lapic(7, 2, 1), lapic(8, 3, 0), x2apic(42, 0x1234, 1),
	), 0x1000)
	if !ok || processors.Count != 2 {
		t.Fatalf("processors: %+v %v", processors, ok)
	}
	if first := processors.Entries[0]; first.APICID != 2 || first.UID != 7 || first.X2APIC {
		t.Fatalf("lapic: %+v", first)
	}
	if second := processors.Entries[1]; second.APICID != 0x1234 || second.UID != 42 || !second.X2APIC {
		t.Fatalf("x2apic: %+v", second)
	}
}

func TestFindProcessorsRejectsMalformedInventory(t *testing.T) {
	for name, entries := range map[string][][]byte{
		"duplicate":   {lapic(1, 2, 1), x2apic(3, 2, 1)},
		"zero length": {{0, 0}},
		"short known": {{0, 4, 0, 0}},
		"truncated":   {{9, 16, 0, 0}},
	} {
		t.Run(name, func(t *testing.T) {
			if processors, ok := FindProcessors(processorSystem(entries...), 0x1000); ok {
				t.Fatalf("accepted %+v", processors)
			}
		})
	}
}

func TestFindProcessorsRejectsOverflow(t *testing.T) {
	entries := make([][]byte, MaxProcessors+1)
	for index := range entries {
		entries[index] = x2apic(uint32(index), uint32(index), 1)
	}
	if processors, ok := FindProcessors(processorSystem(entries...), 0x1000); ok {
		t.Fatalf("accepted %+v", processors)
	}
}
