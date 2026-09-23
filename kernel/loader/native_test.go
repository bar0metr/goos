package loader

import (
	"encoding/binary"
	"testing"
)

const (
	testUserStart = uint64(0x40000000)
	testUserEnd   = uint64(0x80000000)
	testPageSize  = uint64(4096)
)

func validNativeImage(t *testing.T) []byte {
	t.Helper()
	storage := make([]byte, 512)
	data, ok := EncodeNative(storage, ImageSource{
		Entry:    0x40002000,
		StackTop: 0x40005000,
		Segments: []SegmentSource{
			{VirtualAddress: 0x40002000, MemorySize: 16, Flags: SegmentReadable | SegmentExecutable, Data: []byte{0x90, 0xcc}},
			{VirtualAddress: 0x40004000, MemorySize: 4096, Flags: SegmentReadable | SegmentWritable},
		},
	})
	if !ok {
		t.Fatal("failed to encode test image")
	}
	return data
}

func TestParseNativeImage(t *testing.T) {
	data := validNativeImage(t)
	image, ok := ParseNative(data, testPageSize, testUserStart, testUserEnd)
	if !ok || image.Entry != 0x40002000 || image.StackTop != 0x40005000 || image.SegmentCount != 2 {
		t.Fatalf("unexpected image: %#v", image)
	}
}

func TestParseNativeAcceptsMultiPageSegment(t *testing.T) {
	storage := make([]byte, 9000)
	data, ok := EncodeNative(storage, ImageSource{
		Entry: 0x40002000, StackTop: 0x40007000,
		Segments: []SegmentSource{
			{VirtualAddress: 0x40002000, MemorySize: 5000, Flags: SegmentReadable | SegmentExecutable, Data: make([]byte, 4500)},
			{VirtualAddress: 0x40006000, MemorySize: 4096, Flags: SegmentReadable | SegmentWritable},
		},
	})
	if !ok {
		t.Fatal("encode failed")
	}
	if _, ok := ParseNative(data, testPageSize, testUserStart, testUserEnd); !ok {
		t.Fatal("multi-page segment rejected")
	}
}

func TestParseNativeRejectsOverlappingAndOversizedSegments(t *testing.T) {
	storage := make([]byte, 512)
	overlapping, ok := EncodeNative(storage, ImageSource{
		Entry: 0x40002000, StackTop: 0x40006000,
		Segments: []SegmentSource{
			{VirtualAddress: 0x40002000, MemorySize: 5000, Flags: SegmentReadable | SegmentExecutable, Data: []byte{0x90}},
			{VirtualAddress: 0x40003000, MemorySize: 3 * 4096, Flags: SegmentReadable | SegmentWritable},
		},
	})
	if !ok {
		t.Fatal("encode failed")
	}
	if _, ok := ParseNative(overlapping, testPageSize, testUserStart, testUserEnd); ok {
		t.Fatal("overlapping segments accepted")
	}

	oversized := append([]byte(nil), validNativeImage(t)...)
	binary.LittleEndian.PutUint32(oversized[48:], uint32((MaxImagePages+1)*testPageSize))
	if _, ok := ParseNative(oversized, testPageSize, testUserStart, testUserEnd); ok {
		t.Fatal("oversized segment accepted")
	}
}

func TestParseNativeRejectsMalformedImages(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{name: "magic", mutate: func(data []byte) { data[0] = 0 }},
		{name: "version", mutate: func(data []byte) { binary.LittleEndian.PutUint16(data[8:], 2) }},
		{name: "too many segments", mutate: func(data []byte) { binary.LittleEndian.PutUint16(data[12:], MaxSegments+1) }},
		{name: "entry outside executable", mutate: func(data []byte) { binary.LittleEndian.PutUint64(data[16:], 0x40003000) }},
		{name: "unaligned segment", mutate: func(data []byte) { binary.LittleEndian.PutUint64(data[32:], 0x40002001) }},
		{name: "kernel segment", mutate: func(data []byte) { binary.LittleEndian.PutUint64(data[32:], 0x1000) }},
		{name: "file overflow", mutate: func(data []byte) { binary.LittleEndian.PutUint32(data[44:], 0xffffffff) }},
		{name: "memory smaller than file", mutate: func(data []byte) { binary.LittleEndian.PutUint32(data[48:], 1) }},
		{name: "write execute", mutate: func(data []byte) { binary.LittleEndian.PutUint32(data[52:], SegmentWritable|SegmentExecutable) }},
		{name: "duplicate address", mutate: func(data []byte) { binary.LittleEndian.PutUint64(data[64:], 0x40002000) }},
		{name: "unaligned stack", mutate: func(data []byte) { binary.LittleEndian.PutUint64(data[24:], 0x40004fff) }},
	}
	for _, test := range tests {
		data := append([]byte(nil), validNativeImage(t)...)
		test.mutate(data)
		if _, ok := ParseNative(data, testPageSize, testUserStart, testUserEnd); ok {
			t.Errorf("accepted malformed image: %s", test.name)
		}
	}
}

func TestParseNativeRequiresStackGuardPage(t *testing.T) {
	code := SegmentSource{VirtualAddress: 0x40002000, MemorySize: 16, Flags: SegmentReadable | SegmentExecutable, Data: []byte{0x90}}
	stack := SegmentSource{VirtualAddress: 0x40004000, MemorySize: 2 * 4096, Flags: SegmentReadable | SegmentWritable}
	tests := []struct {
		name     string
		stackTop uint64
		segments []SegmentSource
		accepted bool
	}{
		{name: "unmapped page below", stackTop: 0x40006000, segments: []SegmentSource{code, stack}, accepted: true},
		{name: "heap directly above", stackTop: 0x40006000, accepted: true, segments: []SegmentSource{code, stack,
			{VirtualAddress: 0x40006000, MemorySize: 4096, Flags: SegmentReadable | SegmentWritable}}},
		{name: "code directly below", stackTop: 0x40004000, segments: []SegmentSource{code,
			{VirtualAddress: 0x40003000, MemorySize: 4096, Flags: SegmentReadable | SegmentWritable}}},
		{name: "data in guard page", stackTop: 0x40006000, segments: []SegmentSource{code, stack,
			{VirtualAddress: 0x40003000, MemorySize: 8, Flags: SegmentReadable, Data: []byte{1}}}},
		{name: "segment tail in guard page", stackTop: 0x40006000, segments: []SegmentSource{
			{VirtualAddress: 0x40002000, MemorySize: 0x1800, Flags: SegmentReadable | SegmentExecutable, Data: []byte{0x90}}, stack}},
		{name: "no room below user start", stackTop: 0x40001000, segments: []SegmentSource{code,
			{VirtualAddress: 0x40000000, MemorySize: 4096, Flags: SegmentReadable | SegmentWritable}}},
	}
	for _, test := range tests {
		data, ok := EncodeNative(make([]byte, 512), ImageSource{Entry: 0x40002000, StackTop: test.stackTop, Segments: test.segments})
		if !ok {
			t.Fatalf("%s: encode failed", test.name)
		}
		if _, ok := ParseNative(data, testPageSize, testUserStart, testUserEnd); ok != test.accepted {
			t.Errorf("%s: accepted = %v, want %v", test.name, ok, test.accepted)
		}
	}
}

func TestEncodeNativeRejectsSmallTarget(t *testing.T) {
	if _, ok := EncodeNative(make([]byte, 32), ImageSource{Segments: []SegmentSource{{Data: make([]byte, 64)}}}); ok {
		t.Fatal("encoded image beyond target")
	}
}
