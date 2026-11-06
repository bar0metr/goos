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
	image, ok := ParsePE(validPE(sectionExecutable), KernelLinkBase)
	if !ok || image.SectionCount != 1 {
		t.Fatal("valid PE rejected")
	}
	section := image.Sections[0]
	if section.Address != KernelLinkBase+0x1000 || section.Size != 0x1000 || !section.Permissions.Executable || section.Permissions.Writable {
		t.Fatalf("unexpected section: %#v", section)
	}
}

func TestParsePERejectsWritableExecutableSection(t *testing.T) {
	if _, ok := ParsePE(validPE(sectionExecutable|sectionWritable), KernelLinkBase); ok {
		t.Fatal("accepted writable executable PE section")
	}
}

func TestParsePERejectsMalformedInput(t *testing.T) {
	for _, data := range [][]byte{nil, make([]byte, 64), validPE(0)} {
		if len(data) != 0 && data[0] == 'M' {
			binary.LittleEndian.PutUint32(data[0x3c:], uint32(len(data)))
		}
		if _, ok := ParsePE(data, KernelLinkBase); ok {
			t.Fatal("accepted malformed PE")
		}
	}
}

func TestParsePERecordsTheBaseItRunsAt(t *testing.T) {
	const relocated = uint64(0x16000000)
	image, ok := ParsePE(validPE(sectionExecutable), relocated)
	if !ok || image.Base != relocated || image.Sections[0].Address != relocated+0x1000 {
		t.Fatalf("relocated image: ok=%v base=%#x section=%#x", ok, image.Base, image.Sections[0].Address)
	}
}

func TestHeaderImageSize(t *testing.T) {
	data := validPE(sectionExecutable)
	if size, ok := HeaderImageSize(data[:PageSize]); !ok || size != uint64(len(data)) {
		t.Fatalf("valid header: size=%#x ok=%v", size, ok)
	}
	cases := map[string]func([]byte){
		"no stub":         func(d []byte) { d[0] = 0 },
		"pe past page":    func(d []byte) { binary.LittleEndian.PutUint32(d[0x3c:], uint32(PageSize-8)) },
		"no signature":    func(d []byte) { d[0x80] = 0 },
		"pe32 magic":      func(d []byte) { binary.LittleEndian.PutUint16(d[0x98:], 0x10b) },
		"zero size":       func(d []byte) { binary.LittleEndian.PutUint32(d[0x98+56:], 0) },
		"oversized image": func(d []byte) { binary.LittleEndian.PutUint32(d[0x98+56:], uint32(KernelImageMaxSize+PageSize)) },
	}
	for name, corrupt := range cases {
		data := validPE(sectionExecutable)
		corrupt(data)
		if _, ok := HeaderImageSize(data[:PageSize]); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}
