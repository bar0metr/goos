package loader

import "encoding/binary"

const (
	NativeVersion     = uint16(1)
	NativeHeaderSize  = uint16(32)
	SegmentHeaderSize = uint64(32)
	MaxSegments       = 8
	MaxImagePages     = 256
	SegmentReadable   = uint32(1 << 0)
	SegmentWritable   = uint32(1 << 1)
	SegmentExecutable = uint32(1 << 2)
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

// ParseNative validates the versioned native image envelope. The bootstrap
// loader bounds the total mapped image size so malformed inputs cannot force
// unbounded physical allocation.
func ParseNative(data []byte, pageSize uint64, userStart uint64, userEnd uint64) (Image, bool) {
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
			segment.FileOffset < segmentTableEnd || end < segment.FileOffset || end > uint64(len(data)) || !validFlags ||
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

// stackIsGuarded requires the page directly below the stack segment to lie in
// the user range and to be covered by no segment, so a stack that overflows
// faults on an unmapped page instead of overwriting what lies below it.
func (image Image) stackIsGuarded(pageSize uint64, userStart uint64) bool {
	stack, ok := image.StackSegment()
	if !ok || stack.VirtualAddress-userStart < pageSize {
		return false
	}
	guard := stack.VirtualAddress - pageSize
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		if segment.VirtualAddress < stack.VirtualAddress && segment.VirtualAddress+segment.MemorySize > guard {
			return false
		}
	}
	return true
}
