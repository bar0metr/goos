// Package elf validates the bounded static ELF64 subset accepted by the GoOS
// Linux x86_64 compatibility layer. It produces metadata only; mapping the
// validated plan belongs to the later Linux loader phase.
package elf

import "encoding/binary"

const (
	HeaderSize        = uint64(64)
	ProgramHeaderSize = uint64(56)
	MaxProgramHeaders = 64
	MaxLoadSegments   = 16
	MaxReservedRanges = 16

	class64    = 2
	littleData = 1
	version    = 1
	typeExec   = 2
	machineX64 = 62

	programNull     = uint32(0)
	programLoad     = uint32(1)
	programDynamic  = uint32(2)
	programInterp   = uint32(3)
	programPHDR     = uint32(6)
	programTLS      = uint32(7)
	programGNUStack = uint32(0x6474e551)
	programGNURelro = uint32(0x6474e552)

	FlagExecutable = uint32(1)
	FlagWritable   = uint32(2)
	FlagReadable   = uint32(4)
	validFlags     = FlagExecutable | FlagWritable | FlagReadable
)

// Source is the stable executable-source view needed by the parser. The
// existing loader.Source implements it without exposing backing storage.
type Source interface {
	Len() uint64
	Read(offset uint64, length uint64, scratch []byte) ([]byte, bool)
}

type bytesSource []byte

func (source bytesSource) Len() uint64 { return uint64(len(source)) }

func (source bytesSource) Read(offset uint64, length uint64, _ []byte) ([]byte, bool) {
	if offset > uint64(len(source)) || length > uint64(len(source))-offset {
		return nil, false
	}
	return source[offset : offset+length], true
}

// Range is a half-open virtual-address interval.
type Range struct {
	Start uint64
	End   uint64
}

// Config supplies address-space policy without coupling the pure parser to a
// particular launch layout. Reserved ranges are checked page-wise because the
// loader cannot safely assign two meanings or protections to one page.
type Config struct {
	PageSize      uint64
	UserStart     uint64
	UserEnd       uint64
	Reserved      [MaxReservedRanges]Range
	ReservedCount int
}

// Error identifies why an image was rejected without allocating an error
// string in the kernel.
type Error uint8

const (
	OK Error = iota
	InvalidConfig
	SourceReadFailed
	BadMagic
	UnsupportedClass
	UnsupportedEncoding
	UnsupportedVersion
	UnsupportedType
	UnsupportedMachine
	BadHeaderSize
	BadProgramHeaderSize
	BadProgramHeaderCount
	BadProgramHeaderTable
	UnsupportedProgramHeader
	InterpreterPresent
	DynamicLinkingPresent
	BadFileRange
	BadMemoryRange
	BadFlags
	WriteExecute
	BadAlignment
	BadCongruence
	TooManyLoadSegments
	EmptyLoadSegment
	FileLargerThanMemory
	OverlappingLoadSegments
	ReservedMappingCollision
	EntryNotExecutable
	ExecutableStack
	DuplicateProgramHeader
	MalformedProgramHeader
	MalformedTLS
	MalformedRELRO
	ProgramHeadersNotMapped
)

type Segment struct {
	VirtualAddress uint64
	FileOffset     uint64
	FileSize       uint64
	MemorySize     uint64
	Alignment      uint64
	MapStart       uint64
	MapEnd         uint64
	FileMapOffset  uint64
	Flags          uint32
}

func (segment Segment) Readable() bool   { return segment.Flags&FlagReadable != 0 }
func (segment Segment) Writable() bool   { return segment.Flags&FlagWritable != 0 }
func (segment Segment) Executable() bool { return segment.Flags&FlagExecutable != 0 }

type TLS struct {
	VirtualAddress uint64
	FileOffset     uint64
	FileSize       uint64
	MemorySize     uint64
	Alignment      uint64
	Present        bool
}

type Image struct {
	Entry                uint64
	ProgramHeaderOffset  uint64
	ProgramHeaderAddress uint64
	ProgramHeaderCount   uint16
	Segments             [MaxLoadSegments]Segment
	SegmentCount         int
	TLS                  TLS
	RELRO                Range
	RELROPresent         bool
	GNUStackPresent      bool
	GNUStackSize         uint64
	relroFileOffset      uint64
	relroFileSize        uint64
}

type programHeader struct {
	typeValue  uint32
	flags      uint32
	offset     uint64
	virtual    uint64
	fileSize   uint64
	memorySize uint64
	alignment  uint64
}

// Parse validates a contiguous ELF image.
func Parse(data []byte, config Config) (Image, Error) {
	return ParseSource(bytesSource(data), config)
}

// ParseSource reads only the ELF header and bounded program-header table.
// Segment contents remain in the stable source for the later mapping phase.
func ParseSource(source Source, config Config) (Image, Error) {
	var image Image
	if !config.valid() {
		return image, InvalidConfig
	}
	sourceSize := source.Len()
	var headerScratch [HeaderSize]byte
	header, ok := source.Read(0, HeaderSize, headerScratch[:])
	if !ok || uint64(len(header)) != HeaderSize {
		return image, SourceReadFailed
	}
	if header[0] != 0x7f || header[1] != 'E' || header[2] != 'L' || header[3] != 'F' {
		return image, BadMagic
	}
	if header[4] != class64 {
		return image, UnsupportedClass
	}
	if header[5] != littleData {
		return image, UnsupportedEncoding
	}
	if header[6] != version || binary.LittleEndian.Uint32(header[20:]) != version {
		return image, UnsupportedVersion
	}
	if binary.LittleEndian.Uint16(header[16:]) != typeExec {
		return image, UnsupportedType
	}
	if binary.LittleEndian.Uint16(header[18:]) != machineX64 {
		return image, UnsupportedMachine
	}
	if binary.LittleEndian.Uint16(header[52:]) != uint16(HeaderSize) {
		return image, BadHeaderSize
	}
	if binary.LittleEndian.Uint16(header[54:]) != uint16(ProgramHeaderSize) {
		return image, BadProgramHeaderSize
	}
	count := binary.LittleEndian.Uint16(header[56:])
	if count == 0 || count > MaxProgramHeaders {
		return image, BadProgramHeaderCount
	}
	phoff := binary.LittleEndian.Uint64(header[32:])
	tableSize := uint64(count) * ProgramHeaderSize
	tableEnd, valid := add(phoff, tableSize)
	if !valid || phoff < HeaderSize || tableEnd > sourceSize {
		return image, BadProgramHeaderTable
	}
	var tableScratch [MaxProgramHeaders * ProgramHeaderSize]byte
	table, ok := source.Read(phoff, tableSize, tableScratch[:])
	if !ok || uint64(len(table)) != tableSize {
		return image, SourceReadFailed
	}
	image.Entry = binary.LittleEndian.Uint64(header[24:])
	image.ProgramHeaderOffset = phoff
	image.ProgramHeaderCount = count

	var explicitPHDR bool
	for index := 0; index < int(count); index++ {
		offset := uint64(index) * ProgramHeaderSize
		raw := table[offset : offset+ProgramHeaderSize]
		entry := programHeader{
			typeValue:  binary.LittleEndian.Uint32(raw[0:]),
			flags:      binary.LittleEndian.Uint32(raw[4:]),
			offset:     binary.LittleEndian.Uint64(raw[8:]),
			virtual:    binary.LittleEndian.Uint64(raw[16:]),
			fileSize:   binary.LittleEndian.Uint64(raw[32:]),
			memorySize: binary.LittleEndian.Uint64(raw[40:]),
			alignment:  binary.LittleEndian.Uint64(raw[48:]),
		}
		if entry.flags&^validFlags != 0 {
			return Image{}, BadFlags
		}
		switch entry.typeValue {
		case programNull:
			continue
		case programInterp:
			return Image{}, InterpreterPresent
		case programDynamic:
			return Image{}, DynamicLinkingPresent
		case programLoad:
			if failure := image.addLoad(entry, sourceSize, config); failure != OK {
				return Image{}, failure
			}
		case programPHDR:
			if explicitPHDR {
				return Image{}, DuplicateProgramHeader
			}
			explicitPHDR = true
			if entry.offset != phoff || entry.fileSize != tableSize || entry.memorySize != tableSize || entry.flags != FlagReadable ||
				!fileRange(entry.offset, entry.fileSize, sourceSize) || !memoryRange(entry.virtual, entry.memorySize, config) ||
				!aligned(entry.alignment) || !congruent(entry, config.PageSize) {
				return Image{}, MalformedProgramHeader
			}
			image.ProgramHeaderAddress = entry.virtual
		case programTLS:
			if image.TLS.Present {
				return Image{}, DuplicateProgramHeader
			}
			if entry.memorySize == 0 || entry.fileSize > entry.memorySize || entry.flags&FlagExecutable != 0 || entry.flags&FlagReadable == 0 ||
				!fileRange(entry.offset, entry.fileSize, sourceSize) || !memoryRange(entry.virtual, entry.memorySize, config) ||
				!aligned(entry.alignment) || !congruent(entry, config.PageSize) {
				return Image{}, MalformedTLS
			}
			image.TLS = TLS{VirtualAddress: entry.virtual, FileOffset: entry.offset, FileSize: entry.fileSize, MemorySize: entry.memorySize, Alignment: entry.alignment, Present: true}
		case programGNUStack:
			if image.GNUStackPresent {
				return Image{}, DuplicateProgramHeader
			}
			if entry.flags&FlagExecutable != 0 {
				return Image{}, ExecutableStack
			}
			if entry.flags&FlagReadable == 0 || entry.offset != 0 || entry.virtual != 0 || entry.fileSize != 0 || !aligned(entry.alignment) {
				return Image{}, MalformedProgramHeader
			}
			image.GNUStackPresent = true
			image.GNUStackSize = entry.memorySize
		case programGNURelro:
			if image.RELROPresent {
				return Image{}, DuplicateProgramHeader
			}
			if entry.memorySize == 0 || entry.fileSize > entry.memorySize || entry.flags&FlagExecutable != 0 ||
				!fileRange(entry.offset, entry.fileSize, sourceSize) || !memoryRange(entry.virtual, entry.memorySize, config) ||
				!aligned(entry.alignment) || !congruent(entry, config.PageSize) {
				return Image{}, MalformedRELRO
			}
			end, _ := add(entry.virtual, entry.memorySize)
			image.RELRO, image.RELROPresent = Range{Start: entry.virtual, End: end}, true
			image.relroFileOffset, image.relroFileSize = entry.offset, entry.fileSize
		default:
			return Image{}, UnsupportedProgramHeader
		}
	}
	if !image.entryExecutable() {
		return Image{}, EntryNotExecutable
	}
	if failure := image.validateMetadata(phoff, tableEnd, tableSize, explicitPHDR); failure != OK {
		return Image{}, failure
	}
	return image, OK
}

func (config Config) valid() bool {
	if config.PageSize == 0 || config.PageSize&(config.PageSize-1) != 0 || config.UserStart >= config.UserEnd ||
		config.UserStart%config.PageSize != 0 || config.UserEnd%config.PageSize != 0 ||
		config.ReservedCount < 0 || config.ReservedCount > MaxReservedRanges {
		return false
	}
	for index := 0; index < config.ReservedCount; index++ {
		reserved := config.Reserved[index]
		if reserved.Start >= reserved.End || reserved.Start < config.UserStart || reserved.End > config.UserEnd ||
			reserved.Start%config.PageSize != 0 || reserved.End%config.PageSize != 0 {
			return false
		}
	}
	return true
}

func (image *Image) addLoad(header programHeader, sourceSize uint64, config Config) Error {
	if image.SegmentCount == MaxLoadSegments {
		return TooManyLoadSegments
	}
	if header.memorySize == 0 {
		return EmptyLoadSegment
	}
	if header.fileSize > header.memorySize {
		return FileLargerThanMemory
	}
	if header.flags&FlagReadable == 0 {
		return BadFlags
	}
	if header.flags&(FlagWritable|FlagExecutable) == FlagWritable|FlagExecutable {
		return WriteExecute
	}
	if !fileRange(header.offset, header.fileSize, sourceSize) {
		return BadFileRange
	}
	if !memoryRange(header.virtual, header.memorySize, config) {
		return BadMemoryRange
	}
	if !aligned(header.alignment) {
		return BadAlignment
	}
	if !congruent(header, config.PageSize) {
		return BadCongruence
	}
	memoryEnd, _ := add(header.virtual, header.memorySize)
	mapStart := header.virtual &^ (config.PageSize - 1)
	mapEnd, ok := roundUp(memoryEnd, config.PageSize)
	if !ok || mapEnd > config.UserEnd {
		return BadMemoryRange
	}
	fileDelta := header.virtual - mapStart
	if header.offset < fileDelta {
		return BadCongruence
	}
	segment := Segment{
		VirtualAddress: header.virtual,
		FileOffset:     header.offset,
		FileSize:       header.fileSize,
		MemorySize:     header.memorySize,
		Alignment:      header.alignment,
		MapStart:       mapStart,
		MapEnd:         mapEnd,
		FileMapOffset:  header.offset - fileDelta,
		Flags:          header.flags,
	}
	for index := 0; index < image.SegmentCount; index++ {
		previous := image.Segments[index]
		if segment.MapStart < previous.MapEnd && previous.MapStart < segment.MapEnd {
			return OverlappingLoadSegments
		}
	}
	for index := 0; index < config.ReservedCount; index++ {
		reserved := config.Reserved[index]
		if segment.MapStart < reserved.End && reserved.Start < segment.MapEnd {
			return ReservedMappingCollision
		}
	}
	image.Segments[image.SegmentCount] = segment
	image.SegmentCount++
	return OK
}

func (image Image) entryExecutable() bool {
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		end, _ := add(segment.VirtualAddress, segment.MemorySize)
		if segment.Executable() && image.Entry >= segment.VirtualAddress && image.Entry < end {
			return true
		}
	}
	return false
}

func (image *Image) validateMetadata(phoff uint64, tableEnd uint64, tableSize uint64, explicitPHDR bool) Error {
	var derivedAddress uint64
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		segmentFileEnd, _ := add(segment.FileOffset, segment.FileSize)
		if phoff >= segment.FileOffset && tableEnd <= segmentFileEnd {
			candidate, ok := add(segment.VirtualAddress, phoff-segment.FileOffset)
			if ok {
				candidateEnd, valid := add(candidate, tableSize)
				segmentMemoryEnd, _ := add(segment.VirtualAddress, segment.MemorySize)
				if valid && candidateEnd <= segmentMemoryEnd {
					derivedAddress = candidate
					break
				}
			}
		}
	}
	if derivedAddress == 0 || explicitPHDR && image.ProgramHeaderAddress != derivedAddress {
		return ProgramHeadersNotMapped
	}
	if !explicitPHDR {
		image.ProgramHeaderAddress = derivedAddress
	}
	if image.TLS.Present && !image.metadataInsideLoad(image.TLS.VirtualAddress, image.TLS.MemorySize, image.TLS.FileOffset, image.TLS.FileSize) {
		return MalformedTLS
	}
	if image.RELROPresent && !image.metadataInsideLoad(image.RELRO.Start, image.RELRO.End-image.RELRO.Start, image.relroFileOffset, image.relroFileSize) {
		return MalformedRELRO
	}
	return OK
}

func (image Image) metadataInsideLoad(address uint64, size uint64, offset uint64, fileSize uint64) bool {
	addressEnd, addressOK := add(address, size)
	fileEnd, fileOK := add(offset, fileSize)
	if !addressOK || !fileOK {
		return false
	}
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		memoryEnd, _ := add(segment.VirtualAddress, segment.MemorySize)
		segmentFileEnd, _ := add(segment.FileOffset, segment.FileSize)
		if address >= segment.VirtualAddress && addressEnd <= memoryEnd && offset >= segment.FileOffset && fileEnd <= segmentFileEnd &&
			address-segment.VirtualAddress == offset-segment.FileOffset {
			return true
		}
	}
	return false
}

func fileRange(offset uint64, size uint64, sourceSize uint64) bool {
	return offset <= sourceSize && size <= sourceSize-offset
}

func memoryRange(address uint64, size uint64, config Config) bool {
	end, ok := add(address, size)
	return size != 0 && ok && address >= config.UserStart && end <= config.UserEnd
}

func aligned(alignment uint64) bool {
	return alignment <= 1 || alignment&(alignment-1) == 0
}

func congruent(header programHeader, pageSize uint64) bool {
	if header.virtual%pageSize != header.offset%pageSize {
		return false
	}
	return header.alignment <= 1 || header.virtual%header.alignment == header.offset%header.alignment
}

func add(left uint64, right uint64) (uint64, bool) {
	result := left + right
	return result, result >= left
}

func roundUp(value uint64, alignment uint64) (uint64, bool) {
	added, ok := add(value, alignment-1)
	if !ok {
		return 0, false
	}
	return added &^ (alignment - 1), true
}
