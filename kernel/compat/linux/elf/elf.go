// Package elf validates and maps the bounded static ELF64 subset accepted by
// the GoOS Linux x86_64 compatibility layer: ET_EXEC images at the addresses
// they name, and static position-independent ET_DYN images at a base the
// caller chooses. Parsing remains pure; MapImage records a validated plan in
// the common lazy executable region model.
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
	typeDyn    = 3
	machineX64 = 62

	programNull     = uint32(0)
	programLoad     = uint32(1)
	programDynamic  = uint32(2)
	programInterp   = uint32(3)
	programNote     = uint32(4)
	programPHDR     = uint32(6)
	programTLS      = uint32(7)
	programGNUFrame = uint32(0x6474e550)
	programGNUStack = uint32(0x6474e551)
	programGNURelro = uint32(0x6474e552)
	programGNUProp  = uint32(0x6474e553)

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
	PageSize  uint64
	UserStart uint64
	UserEnd   uint64
	// PIEBase is where a position-independent image is loaded: the parser
	// adds it to every address the image names. Zero refuses such images.
	PIEBase       uint64
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
	MalformedDynamic
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
	// Base is the address a position-independent image was moved to, and
	// zero for an ET_EXEC image. Every other address here includes it.
	Base                 uint64
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
	// Dynamic is the PT_DYNAMIC range of a position-independent image. The
	// program's own startup code reads it to relocate itself; the kernel
	// applies no relocation.
	Dynamic           Range
	DynamicPresent    bool
	relroFileOffset   uint64
	relroFileSize     uint64
	dynamicFileOffset uint64
	dynamicFileSize   uint64
}

// LoadEnd returns the first page after the highest load segment.
func (image *Image) LoadEnd() uint64 {
	if image.SegmentCount < 0 || image.SegmentCount > len(image.Segments) {
		return 0
	}
	var end uint64
	for index := 0; index < image.SegmentCount; index++ {
		if image.Segments[index].MapEnd > end {
			end = image.Segments[index].MapEnd
		}
	}
	return end
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
// Segment contents remain in the stable source for lazy mapping by MapImage.
func ParseSource(source Source, config Config) (Image, Error) {
	var image Image
	failure := ParseInto(&image, source, config)
	return image, failure
}

// tableScratch receives the program-header table of the image being parsed.
// It is package storage because a 3.5 KiB local would land on the kernel
// heap, which never frees. Launches are serialized, so one table suffices.
var tableScratch [MaxProgramHeaders * ProgramHeaderSize]byte

// ParseInto is ParseSource for a caller that owns the Image storage. The
// kernel uses it with fixed storage; a rejected image leaves image zeroed.
func ParseInto(image *Image, source Source, config Config) Error {
	if image == nil {
		return InvalidConfig
	}
	failure := image.parse(source, config)
	if failure != OK {
		*image = Image{}
	}
	return failure
}

func (image *Image) parse(source Source, config Config) Error {
	*image = Image{}
	if !config.valid() {
		return InvalidConfig
	}
	sourceSize := source.Len()
	var headerScratch [HeaderSize]byte
	header, ok := source.Read(0, HeaderSize, headerScratch[:])
	if !ok || uint64(len(header)) != HeaderSize {
		return SourceReadFailed
	}
	if header[0] != 0x7f || header[1] != 'E' || header[2] != 'L' || header[3] != 'F' {
		return BadMagic
	}
	if header[4] != class64 {
		return UnsupportedClass
	}
	if header[5] != littleData {
		return UnsupportedEncoding
	}
	if header[6] != version || binary.LittleEndian.Uint32(header[20:]) != version {
		return UnsupportedVersion
	}
	switch binary.LittleEndian.Uint16(header[16:]) {
	case typeExec:
	case typeDyn:
		// A static position-independent executable. An ET_DYN image that
		// needs a dynamic linker names one in PT_INTERP and is refused below.
		if config.PIEBase == 0 {
			return UnsupportedType
		}
		image.Base = config.PIEBase
	default:
		return UnsupportedType
	}
	if binary.LittleEndian.Uint16(header[18:]) != machineX64 {
		return UnsupportedMachine
	}
	if binary.LittleEndian.Uint16(header[52:]) != uint16(HeaderSize) {
		return BadHeaderSize
	}
	if binary.LittleEndian.Uint16(header[54:]) != uint16(ProgramHeaderSize) {
		return BadProgramHeaderSize
	}
	count := binary.LittleEndian.Uint16(header[56:])
	if count == 0 || count > MaxProgramHeaders {
		return BadProgramHeaderCount
	}
	phoff := binary.LittleEndian.Uint64(header[32:])
	tableSize := uint64(count) * ProgramHeaderSize
	tableEnd, valid := add(phoff, tableSize)
	if !valid || phoff < HeaderSize || tableEnd > sourceSize {
		return BadProgramHeaderTable
	}
	table, ok := source.Read(phoff, tableSize, tableScratch[:])
	if !ok || uint64(len(table)) != tableSize {
		return SourceReadFailed
	}
	if image.Entry, ok = add(binary.LittleEndian.Uint64(header[24:]), image.Base); !ok {
		return EntryNotExecutable
	}
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
			return BadFlags
		}
		switch entry.typeValue {
		case programLoad, programPHDR, programTLS, programGNURelro, programDynamic:
			// These name memory, which moves with the image.
			if !entry.rebase(image.Base) {
				return BadMemoryRange
			}
		}
		switch entry.typeValue {
		case programNull:
			continue
		case programNote, programGNUFrame, programGNUProp:
			// Notes, the unwind index, and property notes describe the image
			// to tools. Nothing here acts on them; the file range still has
			// to exist.
			if !fileRange(entry.offset, entry.fileSize, sourceSize) {
				return MalformedProgramHeader
			}
		case programInterp:
			return InterpreterPresent
		case programDynamic:
			// An ET_EXEC image with a dynamic section expects a linker. A
			// static position-independent image carries one for itself.
			if image.Base == 0 {
				return DynamicLinkingPresent
			}
			if image.DynamicPresent {
				return DuplicateProgramHeader
			}
			if entry.memorySize == 0 || entry.fileSize > entry.memorySize || entry.flags&FlagExecutable != 0 || entry.flags&FlagReadable == 0 ||
				!fileRange(entry.offset, entry.fileSize, sourceSize) || !memoryRange(entry.virtual, entry.memorySize, config) ||
				!aligned(entry.alignment) || !congruent(entry, config.PageSize) {
				return MalformedDynamic
			}
			end, _ := add(entry.virtual, entry.memorySize)
			image.Dynamic, image.DynamicPresent = Range{Start: entry.virtual, End: end}, true
			image.dynamicFileOffset, image.dynamicFileSize = entry.offset, entry.fileSize
		case programLoad:
			if failure := image.addLoad(entry, sourceSize, config); failure != OK {
				return failure
			}
		case programPHDR:
			if explicitPHDR {
				return DuplicateProgramHeader
			}
			explicitPHDR = true
			if entry.offset != phoff || entry.fileSize != tableSize || entry.memorySize != tableSize || entry.flags != FlagReadable ||
				!fileRange(entry.offset, entry.fileSize, sourceSize) || !memoryRange(entry.virtual, entry.memorySize, config) ||
				!aligned(entry.alignment) || !congruent(entry, config.PageSize) {
				return MalformedProgramHeader
			}
			image.ProgramHeaderAddress = entry.virtual
		case programTLS:
			if image.TLS.Present {
				return DuplicateProgramHeader
			}
			if entry.memorySize == 0 || entry.fileSize > entry.memorySize || entry.flags&FlagExecutable != 0 || entry.flags&FlagReadable == 0 ||
				!fileRange(entry.offset, entry.fileSize, sourceSize) || !memoryRange(entry.virtual, entry.memorySize, config) ||
				!aligned(entry.alignment) || !congruent(entry, config.PageSize) {
				return MalformedTLS
			}
			image.TLS = TLS{VirtualAddress: entry.virtual, FileOffset: entry.offset, FileSize: entry.fileSize, MemorySize: entry.memorySize, Alignment: entry.alignment, Present: true}
		case programGNUStack:
			if image.GNUStackPresent {
				return DuplicateProgramHeader
			}
			if entry.flags&FlagExecutable != 0 {
				return ExecutableStack
			}
			if entry.flags&FlagReadable == 0 || entry.offset != 0 || entry.virtual != 0 || entry.fileSize != 0 || !aligned(entry.alignment) {
				return MalformedProgramHeader
			}
			image.GNUStackPresent = true
			image.GNUStackSize = entry.memorySize
		case programGNURelro:
			if image.RELROPresent {
				return DuplicateProgramHeader
			}
			if entry.memorySize == 0 || entry.fileSize > entry.memorySize || entry.flags&FlagExecutable != 0 ||
				!fileRange(entry.offset, entry.fileSize, sourceSize) || !memoryRange(entry.virtual, entry.memorySize, config) ||
				!aligned(entry.alignment) || !congruent(entry, config.PageSize) {
				return MalformedRELRO
			}
			end, _ := add(entry.virtual, entry.memorySize)
			image.RELRO, image.RELROPresent = Range{Start: entry.virtual, End: end}, true
			image.relroFileOffset, image.relroFileSize = entry.offset, entry.fileSize
		default:
			return UnsupportedProgramHeader
		}
	}
	if !image.entryExecutable() {
		return EntryNotExecutable
	}
	if failure := image.validateMetadata(phoff, tableEnd, tableSize, explicitPHDR); failure != OK {
		return failure
	}
	return OK
}

func (config Config) valid() bool {
	if config.PageSize == 0 || config.PageSize&(config.PageSize-1) != 0 || config.UserStart >= config.UserEnd ||
		config.UserStart%config.PageSize != 0 || config.UserEnd%config.PageSize != 0 ||
		config.ReservedCount < 0 || config.ReservedCount > MaxReservedRanges {
		return false
	}
	if config.PIEBase != 0 && (config.PIEBase%config.PageSize != 0 || config.PIEBase < config.UserStart || config.PIEBase >= config.UserEnd) {
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

func (image *Image) entryExecutable() bool {
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
	if image.DynamicPresent && !image.metadataInsideLoad(image.Dynamic.Start, image.Dynamic.End-image.Dynamic.Start, image.dynamicFileOffset, image.dynamicFileSize) {
		return MalformedDynamic
	}
	return OK
}

func (image *Image) metadataInsideLoad(address uint64, size uint64, offset uint64, fileSize uint64) bool {
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

// rebase moves the header's address by base and reports false when the sum
// leaves the address space.
func (header *programHeader) rebase(base uint64) bool {
	moved, ok := add(header.virtual, base)
	header.virtual = moved
	return ok
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
