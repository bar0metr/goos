package elf

import (
	"encoding/binary"
	"math"
	"testing"

	"goos/kernel/loader"
)

const (
	testPageSize = uint64(4096)
	testUserEnd  = uint64(0x0000800000000000)
	validPHDR    = 0
	validHeader  = 1
	validCode    = 2
	validData    = 3
	validTLS     = 4
	validStack   = 5
	validRELRO   = 6
)

type testProgramHeader struct {
	typeValue  uint32
	flags      uint32
	offset     uint64
	virtual    uint64
	fileSize   uint64
	memorySize uint64
	alignment  uint64
}

type shortSource struct {
	data []byte
}

func (source shortSource) Len() uint64 { return uint64(len(source.data)) }

func (source shortSource) Read(offset uint64, length uint64, _ []byte) ([]byte, bool) {
	if length == 0 || offset >= uint64(len(source.data)) {
		return nil, true
	}
	end := offset + length - 1
	if end > uint64(len(source.data)) {
		end = uint64(len(source.data))
	}
	return source.data[offset:end], true
}

func testConfig() Config {
	return Config{PageSize: testPageSize, UserStart: testPageSize, UserEnd: testUserEnd}
}

func buildELF(t testing.TB, entry uint64, headers []testProgramHeader, size int) []byte {
	t.Helper()
	if size < int(HeaderSize)+len(headers)*int(ProgramHeaderSize) {
		t.Fatal("test ELF storage is too small")
	}
	data := make([]byte, size)
	copy(data[:4], []byte{0x7f, 'E', 'L', 'F'})
	data[4], data[5], data[6] = class64, littleData, version
	binary.LittleEndian.PutUint16(data[16:], typeExec)
	binary.LittleEndian.PutUint16(data[18:], machineX64)
	binary.LittleEndian.PutUint32(data[20:], version)
	binary.LittleEndian.PutUint64(data[24:], entry)
	binary.LittleEndian.PutUint64(data[32:], HeaderSize)
	binary.LittleEndian.PutUint16(data[52:], uint16(HeaderSize))
	binary.LittleEndian.PutUint16(data[54:], uint16(ProgramHeaderSize))
	binary.LittleEndian.PutUint16(data[56:], uint16(len(headers)))
	for index, header := range headers {
		offset := int(HeaderSize) + index*int(ProgramHeaderSize)
		raw := data[offset : offset+int(ProgramHeaderSize)]
		binary.LittleEndian.PutUint32(raw[0:], header.typeValue)
		binary.LittleEndian.PutUint32(raw[4:], header.flags)
		binary.LittleEndian.PutUint64(raw[8:], header.offset)
		binary.LittleEndian.PutUint64(raw[16:], header.virtual)
		binary.LittleEndian.PutUint64(raw[24:], header.virtual)
		binary.LittleEndian.PutUint64(raw[32:], header.fileSize)
		binary.LittleEndian.PutUint64(raw[40:], header.memorySize)
		binary.LittleEndian.PutUint64(raw[48:], header.alignment)
	}
	return data
}

func validELF(t testing.TB) []byte {
	const count = 7
	tableSize := uint64(count) * ProgramHeaderSize
	return buildELF(t, 0x401020, []testProgramHeader{
		{programPHDR, FlagReadable, HeaderSize, 0x400000 + HeaderSize, tableSize, tableSize, 8},
		{programLoad, FlagReadable, 0, 0x400000, 0x1000, 0x1000, 0x1000},
		{programLoad, FlagReadable | FlagExecutable, 0x1000, 0x401000, 0x200, 0x200, 0x1000},
		{programLoad, FlagReadable | FlagWritable, 0x2000, 0x402000, 0x100, 0x400, 0x1000},
		{programTLS, FlagReadable | FlagWritable, 0x2080, 0x402080, 0x20, 0x40, 0x10},
		{programGNUStack, FlagReadable | FlagWritable, 0, 0, 0, 0, 16},
		{programGNURelro, FlagReadable, 0x2000, 0x402000, 0x100, 0x100, 1},
	}, 0x3000)
}

func programHeaderAt(data []byte, index int) []byte {
	offset := int(HeaderSize) + index*int(ProgramHeaderSize)
	return data[offset : offset+int(ProgramHeaderSize)]
}

func TestParseStaticELF64(t *testing.T) {
	image, failure := Parse(validELF(t), testConfig())
	if failure != OK {
		t.Fatalf("valid ELF rejected: %d", failure)
	}
	if image.Entry != 0x401020 || image.ProgramHeaderAddress != 0x400040 || image.ProgramHeaderCount != 7 || image.SegmentCount != 3 {
		t.Fatalf("unexpected load plan: %+v", image)
	}
	if !image.Segments[1].Executable() || image.Segments[1].Writable() || image.Segments[2].Executable() || !image.Segments[2].Writable() {
		t.Fatalf("segment protections lost: %+v", image.Segments)
	}
	if !image.TLS.Present || image.TLS.MemorySize != 0x40 || !image.RELROPresent || !image.GNUStackPresent {
		t.Fatalf("metadata missing: %+v", image)
	}
}

func TestProgramHeaderAddressIsDerivedWithoutPTPHDR(t *testing.T) {
	data := validELF(t)
	binary.LittleEndian.PutUint32(programHeaderAt(data, validPHDR)[0:], programNull)
	image, failure := Parse(data, testConfig())
	if failure != OK || image.ProgramHeaderAddress != 0x400040 {
		t.Fatalf("derived PHDR = %#x, failure %d", image.ProgramHeaderAddress, failure)
	}
}

func TestAcceptsCongruentUnalignedLoad(t *testing.T) {
	data := validELF(t)
	header := programHeaderAt(data, validCode)
	binary.LittleEndian.PutUint64(header[8:], 0x1120)
	binary.LittleEndian.PutUint64(header[16:], 0x401120)
	binary.LittleEndian.PutUint64(header[48:], 0x10)
	binary.LittleEndian.PutUint64(data[24:], 0x401120)
	image, failure := Parse(data, testConfig())
	if failure != OK || image.Segments[1].MapStart != 0x401000 || image.Segments[1].FileMapOffset != 0x1000 {
		t.Fatalf("unaligned load failure %d: %+v", failure, image.Segments[1])
	}
}

func TestParseSourceAcrossPages(t *testing.T) {
	data := validELF(t)
	const sourcePageSize = uint64(128)
	page := func(index uint64) ([]byte, bool) {
		start := index * sourcePageSize
		if start >= uint64(len(data)) {
			return nil, false
		}
		storage := make([]byte, sourcePageSize)
		copy(storage, data[start:])
		return storage, true
	}
	source, ok := loader.Paged(page, sourcePageSize, uint64(len(data)))
	if !ok {
		t.Fatal("paged source rejected")
	}
	image, failure := ParseSource(source, testConfig())
	if failure != OK || image.SegmentCount != 3 {
		t.Fatalf("paged source failure %d: %+v", failure, image)
	}
}

func TestRejectsShortSuccessfulSourceRead(t *testing.T) {
	if _, failure := ParseSource(shortSource{data: validELF(t)}, testConfig()); failure != SourceReadFailed {
		t.Fatalf("short source read failure = %d", failure)
	}
}

func TestRejectsELFHeaderFailures(t *testing.T) {
	tests := []struct {
		name string
		want Error
		edit func([]byte)
	}{
		{"magic", BadMagic, func(data []byte) { data[0] = 0 }},
		{"class", UnsupportedClass, func(data []byte) { data[4] = 1 }},
		{"encoding", UnsupportedEncoding, func(data []byte) { data[5] = 2 }},
		{"ident version", UnsupportedVersion, func(data []byte) { data[6] = 0 }},
		{"header version", UnsupportedVersion, func(data []byte) { binary.LittleEndian.PutUint32(data[20:], 0) }},
		{"type", UnsupportedType, func(data []byte) { binary.LittleEndian.PutUint16(data[16:], 3) }},
		{"machine", UnsupportedMachine, func(data []byte) { binary.LittleEndian.PutUint16(data[18:], 3) }},
		{"header size", BadHeaderSize, func(data []byte) { binary.LittleEndian.PutUint16(data[52:], 63) }},
		{"program header size", BadProgramHeaderSize, func(data []byte) { binary.LittleEndian.PutUint16(data[54:], 55) }},
		{"zero headers", BadProgramHeaderCount, func(data []byte) { binary.LittleEndian.PutUint16(data[56:], 0) }},
		{"too many headers", BadProgramHeaderCount, func(data []byte) { binary.LittleEndian.PutUint16(data[56:], MaxProgramHeaders+1) }},
		{"table before header", BadProgramHeaderTable, func(data []byte) { binary.LittleEndian.PutUint64(data[32:], 1) }},
		{"table wrap", BadProgramHeaderTable, func(data []byte) { binary.LittleEndian.PutUint64(data[32:], math.MaxUint64-8) }},
	}
	for _, test := range tests {
		data := validELF(t)
		test.edit(data)
		if _, failure := Parse(data, testConfig()); failure != test.want {
			t.Errorf("%s: failure %d, want %d", test.name, failure, test.want)
		}
	}
	if _, failure := Parse(validELF(t)[:40], testConfig()); failure != SourceReadFailed {
		t.Fatalf("truncated header failure = %d", failure)
	}
}

func TestRejectsUnsupportedProgramHeaders(t *testing.T) {
	tests := []struct {
		name      string
		typeValue uint32
		want      Error
	}{
		{"interpreter", programInterp, InterpreterPresent},
		{"dynamic", programDynamic, DynamicLinkingPresent},
		{"unknown", 0x70000001, UnsupportedProgramHeader},
		{"shared library list", 5, UnsupportedProgramHeader},
	}
	for _, test := range tests {
		data := validELF(t)
		binary.LittleEndian.PutUint32(programHeaderAt(data, validTLS), test.typeValue)
		if _, failure := Parse(data, testConfig()); failure != test.want {
			t.Errorf("%s: failure %d, want %d", test.name, failure, test.want)
		}
	}
}

func TestRejectsMalformedLoadSegments(t *testing.T) {
	tests := []struct {
		name string
		want Error
		edit func([]byte)
	}{
		{"empty", EmptyLoadSegment, func(data []byte) { binary.LittleEndian.PutUint64(programHeaderAt(data, validCode)[40:], 0) }},
		{"file larger than memory", FileLargerThanMemory, func(data []byte) { binary.LittleEndian.PutUint64(programHeaderAt(data, validCode)[32:], 0x201) }},
		{"file out of source", BadFileRange, func(data []byte) { binary.LittleEndian.PutUint64(programHeaderAt(data, validCode)[8:], math.MaxUint64) }},
		{"memory wrap", BadMemoryRange, func(data []byte) {
			binary.LittleEndian.PutUint64(programHeaderAt(data, validCode)[16:], math.MaxUint64-0xff)
		}},
		{"not readable", BadFlags, func(data []byte) { binary.LittleEndian.PutUint32(programHeaderAt(data, validCode)[4:], FlagExecutable) }},
		{"write execute", WriteExecute, func(data []byte) {
			binary.LittleEndian.PutUint32(programHeaderAt(data, validCode)[4:], FlagReadable|FlagWritable|FlagExecutable)
		}},
		{"bad alignment", BadAlignment, func(data []byte) { binary.LittleEndian.PutUint64(programHeaderAt(data, validCode)[48:], 3) }},
		{"bad page congruence", BadCongruence, func(data []byte) { binary.LittleEndian.PutUint64(programHeaderAt(data, validCode)[8:], 0x1001) }},
		{"overlap", OverlappingLoadSegments, func(data []byte) {
			header := programHeaderAt(data, validData)
			binary.LittleEndian.PutUint64(header[8:], 0x1100)
			binary.LittleEndian.PutUint64(header[16:], 0x401100)
		}},
		{"entry outside executable", EntryNotExecutable, func(data []byte) { binary.LittleEndian.PutUint64(data[24:], 0x402000) }},
	}
	for _, test := range tests {
		data := validELF(t)
		test.edit(data)
		if _, failure := Parse(data, testConfig()); failure != test.want {
			t.Errorf("%s: failure %d, want %d", test.name, failure, test.want)
		}
	}
}

func TestRejectsExecutableStackAndReservedCollision(t *testing.T) {
	data := validELF(t)
	binary.LittleEndian.PutUint32(programHeaderAt(data, validStack)[4:], FlagReadable|FlagWritable|FlagExecutable)
	if _, failure := Parse(data, testConfig()); failure != ExecutableStack {
		t.Fatalf("executable stack failure = %d", failure)
	}

	config := testConfig()
	config.Reserved[0] = Range{Start: 0x401000, End: 0x402000}
	config.ReservedCount = 1
	if _, failure := Parse(validELF(t), config); failure != ReservedMappingCollision {
		t.Fatalf("reserved collision failure = %d", failure)
	}
}

func TestRecordsGNUStackSizeHint(t *testing.T) {
	data := validELF(t)
	binary.LittleEndian.PutUint64(programHeaderAt(data, validStack)[40:], 8<<20)
	image, failure := Parse(data, testConfig())
	if failure != OK || image.GNUStackSize != 8<<20 {
		t.Fatalf("GNU stack hint = %d, failure %d", image.GNUStackSize, failure)
	}
}

func TestRejectsMalformedMetadata(t *testing.T) {
	tests := []struct {
		name string
		want Error
		edit func([]byte)
	}{
		{"PHDR mismatch", MalformedProgramHeader, func(data []byte) { binary.LittleEndian.PutUint64(programHeaderAt(data, validPHDR)[8:], 0) }},
		{"TLS outside load", MalformedTLS, func(data []byte) { binary.LittleEndian.PutUint64(programHeaderAt(data, validTLS)[16:], 0x403080) }},
		{"RELRO outside load", MalformedRELRO, func(data []byte) { binary.LittleEndian.PutUint64(programHeaderAt(data, validRELRO)[16:], 0x403000) }},
		{"duplicate stack", DuplicateProgramHeader, func(data []byte) {
			binary.LittleEndian.PutUint32(programHeaderAt(data, validRELRO)[0:], programGNUStack)
		}},
		{"headers not mapped", ProgramHeadersNotMapped, func(data []byte) { binary.LittleEndian.PutUint64(programHeaderAt(data, validHeader)[8:], 0x1000) }},
	}
	for _, test := range tests {
		data := validELF(t)
		test.edit(data)
		if _, failure := Parse(data, testConfig()); failure != test.want {
			t.Errorf("%s: failure %d, want %d", test.name, failure, test.want)
		}
	}
}

func TestRejectsMoreThanSixteenLoadSegments(t *testing.T) {
	headers := make([]testProgramHeader, MaxLoadSegments+1)
	for index := range headers {
		offset := uint64(index) * testPageSize
		flags := FlagReadable
		if index == 0 {
			flags |= FlagExecutable
		}
		headers[index] = testProgramHeader{programLoad, flags, offset, 0x400000 + offset, testPageSize, testPageSize, testPageSize}
	}
	data := buildELF(t, 0x400000, headers, (MaxLoadSegments+1)*int(testPageSize))
	if _, failure := Parse(data, testConfig()); failure != TooManyLoadSegments {
		t.Fatalf("load segment bound failure = %d", failure)
	}
}

func TestAcceptsExactProgramAndLoadHeaderBounds(t *testing.T) {
	headers := make([]testProgramHeader, MaxProgramHeaders)
	for index := 0; index < MaxLoadSegments; index++ {
		offset := uint64(index) * testPageSize
		flags := FlagReadable
		if index == 0 {
			flags |= FlagExecutable
		}
		headers[index] = testProgramHeader{programLoad, flags, offset, 0x400000 + offset, testPageSize, testPageSize, testPageSize}
	}
	data := buildELF(t, 0x400000, headers, MaxLoadSegments*int(testPageSize))
	image, failure := Parse(data, testConfig())
	if failure != OK || image.ProgramHeaderCount != MaxProgramHeaders || image.SegmentCount != MaxLoadSegments {
		t.Fatalf("exact bounds failure %d: headers=%d loads=%d", failure, image.ProgramHeaderCount, image.SegmentCount)
	}
}

func TestRejectsInvalidConfig(t *testing.T) {
	configs := []Config{
		{},
		{PageSize: 3000, UserStart: 0x1000, UserEnd: testUserEnd},
		{PageSize: testPageSize, UserStart: 0x1001, UserEnd: testUserEnd},
		{PageSize: testPageSize, UserStart: testPageSize, UserEnd: testUserEnd, ReservedCount: MaxReservedRanges + 1},
		{PageSize: testPageSize, UserStart: testPageSize, UserEnd: testUserEnd, Reserved: [MaxReservedRanges]Range{{Start: 0x401001, End: 0x402000}}, ReservedCount: 1},
	}
	for index, config := range configs {
		if _, failure := Parse(validELF(t), config); failure != InvalidConfig {
			t.Errorf("config %d failure = %d", index, failure)
		}
	}
}

func FuzzParse(fuzz *testing.F) {
	fuzz.Add(validELF(fuzz))
	fuzz.Add([]byte{0x7f, 'E', 'L', 'F'})
	fuzz.Fuzz(func(t *testing.T, data []byte) {
		Parse(data, testConfig())
	})
}

// pieELF is a static position-independent image: ET_DYN, addresses from
// zero, a dynamic section in the data segment, and the descriptive headers
// common toolchains add.
func pieELF(t testing.TB) []byte {
	const count = 11
	tableSize := uint64(count) * ProgramHeaderSize
	data := buildELF(t, 0x1020, []testProgramHeader{
		{programPHDR, FlagReadable, HeaderSize, HeaderSize, tableSize, tableSize, 8},
		{programLoad, FlagReadable, 0, 0, 0x1000, 0x1000, 0x1000},
		{programLoad, FlagReadable | FlagExecutable, 0x1000, 0x1000, 0x200, 0x200, 0x1000},
		{programLoad, FlagReadable | FlagWritable, 0x2000, 0x2000, 0x200, 0x400, 0x1000},
		{programTLS, FlagReadable | FlagWritable, 0x2080, 0x2080, 0x20, 0x40, 0x10},
		{programGNUStack, FlagReadable | FlagWritable, 0, 0, 0, 0, 16},
		{programGNURelro, FlagReadable, 0x2000, 0x2000, 0x200, 0x200, 1},
		{programDynamic, FlagReadable | FlagWritable, 0x2100, 0x2100, 0x100, 0x100, 8},
		{programNote, FlagReadable, 0x300, 0x300, 0x20, 0x20, 4},
		{programGNUProp, FlagReadable, 0x300, 0x300, 0x20, 0x20, 8},
		{programGNUFrame, FlagReadable, 0x320, 0x320, 0x20, 0x20, 4},
	}, 0x3000)
	binary.LittleEndian.PutUint16(data[16:], typeDyn)
	return data
}

const pieDynamic = 7

func pieConfig() Config {
	config := testConfig()
	config.PIEBase = 0x40000000
	return config
}

func TestParseStaticPIEMovesEveryAddress(t *testing.T) {
	image, failure := Parse(pieELF(t), pieConfig())
	if failure != OK {
		t.Fatalf("static PIE rejected: %d", failure)
	}
	const base = 0x40000000
	if image.Base != base || image.Entry != base+0x1020 || image.ProgramHeaderAddress != base+HeaderSize || image.SegmentCount != 3 {
		t.Fatalf("unexpected load plan: %+v", image)
	}
	for index, want := range []uint64{base, base + 0x1000, base + 0x2000} {
		segment := image.Segments[index]
		if segment.VirtualAddress != want || segment.MapStart != want || segment.FileMapOffset != want-base {
			t.Errorf("segment %d: %+v", index, segment)
		}
	}
	if image.TLS.VirtualAddress != base+0x2080 || image.RELRO.Start != base+0x2000 || image.RELRO.End != base+0x2200 ||
		!image.DynamicPresent || image.Dynamic.Start != base+0x2100 || image.Dynamic.End != base+0x2200 {
		t.Fatalf("metadata did not move: %+v", image)
	}
	if image.LoadEnd() != base+0x3000 {
		t.Fatalf("load end = %#x", image.LoadEnd())
	}
}

func TestStaticPIENeedsABase(t *testing.T) {
	if _, failure := Parse(pieELF(t), testConfig()); failure != UnsupportedType {
		t.Fatalf("PIE without a base: failure %d", failure)
	}
	for _, base := range []uint64{0x40000001, testUserEnd, 0x800} {
		config := testConfig()
		config.PIEBase = base
		if _, failure := Parse(pieELF(t), config); failure != InvalidConfig {
			t.Errorf("base %#x: failure %d", base, failure)
		}
	}
}

func TestStaticPIERejections(t *testing.T) {
	tests := []struct {
		name string
		want Error
		edit func([]byte)
	}{
		{"interpreter", InterpreterPresent, func(data []byte) {
			binary.LittleEndian.PutUint32(programHeaderAt(data, 8), programInterp)
		}},
		{"second dynamic section", DuplicateProgramHeader, func(data []byte) {
			copy(programHeaderAt(data, 8), programHeaderAt(data, pieDynamic))
		}},
		{"dynamic section outside any load", MalformedDynamic, func(data []byte) {
			binary.LittleEndian.PutUint64(programHeaderAt(data, pieDynamic)[16:], 0x5100)
		}},
		{"dynamic section past its segment's file bytes", MalformedDynamic, func(data []byte) {
			binary.LittleEndian.PutUint64(programHeaderAt(data, pieDynamic)[8:], 0x2180)
			binary.LittleEndian.PutUint64(programHeaderAt(data, pieDynamic)[16:], 0x2180)
		}},
		{"executable dynamic section", MalformedDynamic, func(data []byte) {
			binary.LittleEndian.PutUint32(programHeaderAt(data, pieDynamic)[4:], FlagReadable|FlagExecutable)
		}},
		{"empty dynamic section", MalformedDynamic, func(data []byte) {
			binary.LittleEndian.PutUint64(programHeaderAt(data, pieDynamic)[40:], 0)
			binary.LittleEndian.PutUint64(programHeaderAt(data, pieDynamic)[32:], 0)
		}},
		{"address that wraps", BadMemoryRange, func(data []byte) {
			binary.LittleEndian.PutUint64(programHeaderAt(data, validData)[16:], math.MaxUint64-0xfff)
		}},
		{"entry that wraps", EntryNotExecutable, func(data []byte) {
			binary.LittleEndian.PutUint64(data[24:], math.MaxUint64)
		}},
		{"entry outside the image", EntryNotExecutable, func(data []byte) {
			binary.LittleEndian.PutUint64(data[24:], 0)
		}},
		{"note past the file", MalformedProgramHeader, func(data []byte) {
			binary.LittleEndian.PutUint64(programHeaderAt(data, 8)[8:], 0x2ff0)
		}},
		{"image above the limit", BadMemoryRange, func(data []byte) {
			binary.LittleEndian.PutUint64(programHeaderAt(data, validData)[16:], testUserEnd)
			binary.LittleEndian.PutUint64(programHeaderAt(data, validData)[8:], 0x2000)
		}},
	}
	for _, test := range tests {
		data := pieELF(t)
		test.edit(data)
		if _, failure := Parse(data, pieConfig()); failure != test.want {
			t.Errorf("%s: failure %d, want %d", test.name, failure, test.want)
		}
	}
}

func TestDescriptiveHeadersAreIgnoredInAnExecutable(t *testing.T) {
	data := validELF(t)
	// The TLS header becomes a note; the RELRO header a property note.
	binary.LittleEndian.PutUint32(programHeaderAt(data, validTLS), programNote)
	binary.LittleEndian.PutUint32(programHeaderAt(data, validRELRO), programGNUProp)
	image, failure := Parse(data, testConfig())
	if failure != OK || image.TLS.Present || image.RELROPresent || image.SegmentCount != 3 || image.Base != 0 {
		t.Fatalf("failure=%d image=%+v", failure, image)
	}
}

func FuzzParseStaticPIE(fuzz *testing.F) {
	fuzz.Add(pieELF(fuzz))
	fuzz.Fuzz(func(t *testing.T, data []byte) {
		image, failure := Parse(data, pieConfig())
		if failure != OK {
			return
		}
		// Whatever the parser accepts lies inside the user range and keeps
		// its entry in an executable segment.
		for index := 0; index < image.SegmentCount; index++ {
			segment := image.Segments[index]
			if segment.MapStart < testPageSize || segment.MapEnd > testUserEnd || segment.MapStart >= segment.MapEnd {
				t.Fatalf("segment %d escapes: %+v", index, segment)
			}
			if image.Base != 0 && segment.MapStart < image.Base {
				t.Fatalf("segment %d below the base: %+v", index, segment)
			}
		}
		if !image.entryExecutable() {
			t.Fatal("accepted image has no executable entry")
		}
	})
}
