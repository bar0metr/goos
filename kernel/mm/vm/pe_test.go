package vm

import (
	"encoding/binary"
	"testing"
)

func validPE(characteristics uint32) []byte {
	data := make([]byte, 0x3000)
	data[0], data[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(data[0x3c:], 0x80)
	binary.LittleEndian.PutUint32(data[0x80:], peSignature)
	binary.LittleEndian.PutUint16(data[0x86:], 1)
	binary.LittleEndian.PutUint16(data[0x94:], 0xf0)
	binary.LittleEndian.PutUint16(data[0x98:], pe32PlusMagic)
	binary.LittleEndian.PutUint32(data[0x98+56:], uint32(len(data)))
	section := 0x98 + 0xf0
	binary.LittleEndian.PutUint32(data[section+8:], 0x1000)
	binary.LittleEndian.PutUint32(data[section+12:], 0x1000)
	binary.LittleEndian.PutUint32(data[section+36:], characteristics)
	return data
}

func TestParsePESectionPermissions(t *testing.T) {
	image, ok := ParsePE(validPE(sectionExecutable), KernelImageBase)
	if !ok || image.SectionCount != 1 {
		t.Fatal("valid PE rejected")
	}
	section := image.Sections[0]
	if section.Address != KernelImageBase+0x1000 || section.Size != 0x1000 || !section.Permissions.Executable || section.Permissions.Writable {
		t.Fatalf("unexpected section: %#v", section)
	}
}

func TestParsePERejectsWritableExecutableSection(t *testing.T) {
	if _, ok := ParsePE(validPE(sectionExecutable|sectionWritable), KernelImageBase); ok {
		t.Fatal("accepted writable executable PE section")
	}
}

func TestParsePERejectsMalformedInput(t *testing.T) {
	for _, data := range [][]byte{nil, make([]byte, 64), validPE(0)} {
		if len(data) != 0 && data[0] == 'M' {
			binary.LittleEndian.PutUint32(data[0x3c:], uint32(len(data)))
		}
		if _, ok := ParsePE(data, KernelImageBase); ok {
			t.Fatal("accepted malformed PE")
		}
	}
}
