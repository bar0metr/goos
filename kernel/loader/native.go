package loader

import (
	"encoding/binary"
	"goos/kernel/mm/vm"
)

const (
	NativeVersion     = uint16(1)
	NativeHeaderSize  = uint16(32)
	SegmentHeaderSize = uint64(32)
	MaxSegments       = 8
	MaxImagePages     = 256
	SegmentReadable   = uint32(1 << 0)
	SegmentWritable   = uint32(1 << 1)
	SegmentExecutable = uint32(1 << 2)
	// StackGuardSize is the unmapped gap every image leaves directly below its
	// stack segment (ADR 0083, ADR 0088). Userspace code has no stack probes,
	// so the gap has to be wider than any frame: tools/stackframes rejects a
	// program with a frame that could step over it, and
	// userspace/runtime/amd64.ld leaves exactly this much below the stack.
	StackGuardSize = uint64(64 << 10)
)

var nativeMagic = [8]byte{'G', 'O', 'O', 'S', 'N', 'A', 'T', 0}

type Segment struct {
	VirtualAddress uint64
	FileOffset     uint64
	FileSize       uint64
	MemorySize     uint64
	Flags          uint32
}

type Image struct {
	Entry        uint64
	StackTop     uint64
	Segments     [MaxSegments]Segment
	SegmentCount int
}

// maxHeader is the most bytes the envelope and its segment table occupy.
const maxHeader = uint64(NativeHeaderSize) + MaxSegments*SegmentHeaderSize

// ParseNative validates the versioned native image envelope. The bootstrap
// loader bounds the total mapped image size so malformed inputs cannot force
// unbounded physical allocation.
func ParseNative(data []byte, pageSize uint64, userStart uint64, userEnd uint64) (Image, bool) {
	return ParseSource(Contiguous(data), pageSize, userStart, userEnd)
}

// ParseSource validates the native image envelope of an executable held in
// any source. Only the envelope and segment table are read here; segment
// bytes are checked against the source's length.
func ParseSource(source Source, pageSize uint64, userStart uint64, userEnd uint64) (Image, bool) {
	var scratch [maxHeader]byte
	length := source.Len()
	if length > maxHeader {
		length = maxHeader
	}
	header, ok := source.Read(0, length, scratch[:])
	if !ok {
		return Image{}, false
	}
	return parseNative(header, source.Len(), pageSize, userStart, userEnd)
}

// parseNative validates an image of size bytes whose first bytes, at least
// its envelope and segment table when the image is well formed, are data.
func parseNative(data []byte, size uint64, pageSize uint64, userStart uint64, userEnd uint64) (Image, bool) {
	var image Image
	if len(data) < int(NativeHeaderSize) || pageSize == 0 || userStart >= userEnd {
		return image, false
	}
	for index := range nativeMagic {
		if data[index] != nativeMagic[index] {
			return image, false
		}
	}
	if binary.LittleEndian.Uint16(data[8:]) != NativeVersion ||
		binary.LittleEndian.Uint16(data[10:]) != NativeHeaderSize || binary.LittleEndian.Uint16(data[14:]) != 0 {
		return image, false
	}
	count := int(binary.LittleEndian.Uint16(data[12:]))
	segmentTableEnd := uint64(NativeHeaderSize) + uint64(count)*SegmentHeaderSize
	if count == 0 || count > MaxSegments || segmentTableEnd > uint64(len(data)) {
		return image, false
	}
	image.Entry = binary.LittleEndian.Uint64(data[16:])
	image.StackTop = binary.LittleEndian.Uint64(data[24:])
	for index := 0; index < count; index++ {
		offset := uint64(NativeHeaderSize) + uint64(index)*SegmentHeaderSize
		header := data[offset : offset+SegmentHeaderSize]
		segment := Segment{
			VirtualAddress: binary.LittleEndian.Uint64(header[0:]),
			FileOffset:     uint64(binary.LittleEndian.Uint32(header[8:])),
			FileSize:       uint64(binary.LittleEndian.Uint32(header[12:])),
			MemorySize:     uint64(binary.LittleEndian.Uint32(header[16:])),
			Flags:          binary.LittleEndian.Uint32(header[20:]),
		}
		end := segment.FileOffset + segment.FileSize
		validFlags := segment.Flags&SegmentReadable != 0 && segment.Flags&^(SegmentReadable|SegmentWritable|SegmentExecutable) == 0
		memoryEnd := segment.VirtualAddress + segment.MemorySize
		if segment.VirtualAddress%pageSize != 0 || segment.VirtualAddress < userStart || segment.VirtualAddress >= userEnd ||
			segment.MemorySize == 0 || segment.MemorySize > pageSize*MaxImagePages || memoryEnd < segment.VirtualAddress || memoryEnd > userEnd || segment.FileSize > segment.MemorySize ||
			segment.FileOffset < segmentTableEnd || end < segment.FileOffset || end > size || !validFlags ||
			segment.Flags&SegmentWritable != 0 && segment.Flags&SegmentExecutable != 0 || binary.LittleEndian.Uint64(header[24:]) != 0 {
			return Image{}, false
		}
		for previous := 0; previous < index; previous++ {
			previousSegment := image.Segments[previous]
			previousEnd := previousSegment.VirtualAddress + previousSegment.MemorySize
			if segment.VirtualAddress < previousEnd && previousSegment.VirtualAddress < memoryEnd {
				return Image{}, false
			}
		}
		image.Segments[index] = segment
	}
	totalPages := uint64(0)
	for index := 0; index < count; index++ {
		totalPages += (image.Segments[index].MemorySize + pageSize - 1) / pageSize
		if totalPages > MaxImagePages {
			return Image{}, false
		}
	}
	image.SegmentCount = count
	if !image.entryIsExecutable() || !image.stackIsGuarded(pageSize, userStart) {
		return Image{}, false
	}
	return image, true
}

func (image Image) entryIsExecutable() bool {
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		if segment.Flags&SegmentExecutable != 0 && image.Entry >= segment.VirtualAddress && image.Entry < segment.VirtualAddress+segment.MemorySize {
			return true
		}
	}
	return false
}

// StackSegment returns the writable, non-executable segment that holds the
// initial stack pointer.
func (image Image) StackSegment() (Segment, bool) {
	if image.StackTop == 0 || image.StackTop%16 != 0 {
		return Segment{}, false
	}
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		if segment.Flags&SegmentWritable != 0 && segment.Flags&SegmentExecutable == 0 &&
			image.StackTop > segment.VirtualAddress && image.StackTop <= segment.VirtualAddress+segment.MemorySize {
			return segment, true
		}
	}
	return Segment{}, false
}

// StackGuard returns the range [start, end) directly below the stack segment
// that must stay unmapped. ok is false when the image has no valid stack.
func (image Image) StackGuard() (start uint64, end uint64, ok bool) {
	stack, found := image.StackSegment()
	if !found || stack.VirtualAddress < StackGuardSize {
		return 0, 0, false
	}
	return stack.VirtualAddress - StackGuardSize, stack.VirtualAddress, true
}

// stackIsGuarded requires the StackGuardSize bytes directly below the stack
// segment to lie in the user range and to be covered by no segment, so a stack
// that overflows faults on an unmapped page instead of overwriting what lies
// below it.
func (image Image) stackIsGuarded(pageSize uint64, userStart uint64) bool {
	start, end, ok := image.StackGuard()
	if !ok || StackGuardSize%pageSize != 0 || start < userStart {
		return false
	}
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		if segment.VirtualAddress < end && segment.VirtualAddress+segment.MemorySize > start {
			return false
		}
	}
	return true
}

// KernelMapsUserRange reports whether the kernel may map any page of
// [start, end) into a process after its image is loaded: the probe and startup
// pages below the image, and the shared-memory, DMA and system image windows.
// A stack guard must avoid all of them (ADR 0083, ADR 0088).
func KernelMapsUserRange(start uint64, end uint64) bool {
	return start < vm.UserCodeAddress && end > vm.UserProbeAddress ||
		start < vm.UserImageLimit && end > vm.UserSharedAddress
}
