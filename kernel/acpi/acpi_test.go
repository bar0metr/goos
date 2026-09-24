package acpi

import (
	"encoding/binary"
	"testing"
)

// fakeMemory is a sparse physical memory of byte slices at base addresses.
type fakeMemory map[uint64][]byte

func (memory fakeMemory) Read(address uint64, target []byte) bool {
	for base, data := range memory {
		if address >= base && address+uint64(len(target)) <= base+uint64(len(data)) {
			copy(target, data[address-base:])
			return true
		}
	}
	return false
}

func table(signature string, body []byte) []byte {
	data := make([]byte, headerSize+len(body))
	copy(data, signature)
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)))
	copy(data[headerSize:], body)
	fix(data, 9)
	return data
}

// fix sets the byte at index so that data sums to zero.
func fix(data []byte, index int) {
	data[index] = 0
	var sum byte
	for _, value := range data {
		sum += value
	}
	data[index] = -sum
}

func rsdp(revision byte, root uint64) []byte {
	data := make([]byte, 36)
	copy(data, "RSD PTR ")
	data[15] = revision
	if revision >= 2 {
		binary.LittleEndian.PutUint32(data[20:], 36)
		binary.LittleEndian.PutUint64(data[24:], root)
		fix(data[:20], 8)
		fix(data, 32)
		return data
	}
	binary.LittleEndian.PutUint32(data[16:], uint32(root))
	fix(data[:20], 8)
	return data[:20]
}

func fadt(port uint32, dsdt uint32) []byte {
	body := make([]byte, 244-headerSize)
	binary.LittleEndian.PutUint32(body[40-headerSize:], dsdt)
	binary.LittleEndian.PutUint32(body[64-headerSize:], port)
	return table("FACP", body)
}

// qemuS5 is how QEMU's DSDT declares \_S5: Name (_S5, Package (4) {0, 0, 0, 0})
// with the first value replaced by value.
func dsdt(s5 []byte) []byte {
	body := append([]byte{0x10, 0x05, '\\', 0x00, 0x5b, 0x80}, s5...)
	return table("DSDT", body)
}

func system(revision byte, s5 []byte) fakeMemory {
	memory := fakeMemory{0x9000: dsdt(s5), 0x8000: fadt(0x604, 0x9000), 0x6000: table("APIC", make([]byte, 8))}
	if revision >= 2 {
		entries := make([]byte, 16)
		binary.LittleEndian.PutUint64(entries, 0x6000)
		binary.LittleEndian.PutUint64(entries[8:], 0x8000)
		memory[0x7000] = table("XSDT", entries)
	} else {
		entries := make([]byte, 8)
		binary.LittleEndian.PutUint32(entries, 0x6000)
		binary.LittleEndian.PutUint32(entries[4:], 0x8000)
		memory[0x7000] = table("RSDT", entries)
	}
	memory[0x1000] = rsdp(revision, 0x7000)
	return memory
}

func TestFindPowerOff(t *testing.T) {
	for _, c := range []struct {
		name     string
		revision byte
		s5       []byte
		value    uint16
	}{
		{"acpi2-byte", 2, []byte{0x08, '_', 'S', '5', '_', 0x12, 0x0a, 0x04, 0x0a, 0x05, 0x0a, 0x05, 0x00, 0x00}, 5<<10 | 1<<13},
		{"acpi1-zero", 0, []byte{0x08, '\\', '_', 'S', '5', '_', 0x12, 0x06, 0x04, 0x00, 0x00, 0x00, 0x00}, 1 << 13},
		{"one", 2, []byte{0x08, '_', 'S', '5', '_', 0x12, 0x06, 0x02, 0x01, 0x01}, 1<<10 | 1<<13},
	} {
		off, ok := FindPowerOff(system(c.revision, c.s5), 0x1000)
		if !ok || off.Port != 0x604 || off.Value != c.value {
			t.Errorf("%s: %+v %v", c.name, off, ok)
		}
	}
}

func TestFindPowerOffRefusesMalformedTables(t *testing.T) {
	good := []byte{0x08, '_', 'S', '5', '_', 0x12, 0x06, 0x04, 0x0a, 0x05}
	cases := map[string]func(fakeMemory){
		"rsdp checksum": func(m fakeMemory) { m[0x1000][8]++ },
		"rsdp signature": func(m fakeMemory) {
			m[0x1000][0] = 'X'
		},
		"xsdt checksum": func(m fakeMemory) { m[0x7000][40]++ },
		"fadt missing":  func(m fakeMemory) { delete(m, 0x8000) },
		"fadt checksum": func(m fakeMemory) { m[0x8000][50]++ },
		"dsdt checksum": func(m fakeMemory) { m[0x9000][40]++ },
		"dsdt length": func(m fakeMemory) {
			binary.LittleEndian.PutUint32(m[0x9000][4:], maxTable+1)
		},
		"dsdt truncated": func(m fakeMemory) { m[0x9000] = m[0x9000][:headerSize+4] },
		"no port": func(m fakeMemory) {
			binary.LittleEndian.PutUint32(m[0x8000][64:], 0)
			fix(m[0x8000], 9)
		},
		"no s5": func(m fakeMemory) {
			m[0x9000] = dsdt([]byte{0x08, '_', 'S', '4', '_', 0x12, 0x06, 0x04, 0x0a, 0x05})
		},
		"s5 not a package": func(m fakeMemory) {
			m[0x9000] = dsdt([]byte{0x08, '_', 'S', '5', '_', 0x0a, 0x05})
		},
		"s5 cut short": func(m fakeMemory) {
			m[0x9000] = dsdt([]byte{0x08, '_', 'S', '5', '_', 0x12, 0x06})
		},
	}
	for name, corrupt := range cases {
		memory := system(2, good)
		corrupt(memory)
		if off, ok := FindPowerOff(memory, 0x1000); ok {
			t.Errorf("%s: accepted %+v", name, off)
		}
	}
	if _, ok := FindPowerOff(system(2, good), 0); ok {
		t.Error("accepted a zero RSDP")
	}
}

func TestParseFADTPrefersExtendedFields(t *testing.T) {
	data := fadt(0x604, 0x9000)
	binary.LittleEndian.PutUint64(data[140:], 0x19000)
	data[172] = 1
	binary.LittleEndian.PutUint64(data[176:], 0xb004)
	port, dsdt, ok := parseFADT(data)
	if !ok || port != 0xb004 || dsdt != 0x19000 {
		t.Fatalf("%#x %#x %v", port, dsdt, ok)
	}
	// A control block outside I/O space is not used.
	data[172] = 0
	if port, _, _ := parseFADT(data); port != 0x604 {
		t.Fatalf("port %#x", port)
	}
}
