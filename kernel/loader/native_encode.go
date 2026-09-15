package loader

import "encoding/binary"

type SegmentSource struct {
	VirtualAddress uint64
	MemorySize     uint32
	Flags          uint32
	Data           []byte
}

type ImageSource struct {
	Entry    uint64
	StackTop uint64
	Segments []SegmentSource
}

// EncodeNative writes a native image into caller-owned storage. It is used by
// bootstrap embedded images and host-side tooling; ParseNative remains the
// authority for accepting the result.
func EncodeNative(target []byte, source ImageSource) ([]byte, bool) {
	count := len(source.Segments)
	headerEnd := uint64(NativeHeaderSize) + uint64(count)*SegmentHeaderSize
	total := headerEnd
	for _, segment := range source.Segments {
		next := total + uint64(len(segment.Data))
		if next < total {
			return nil, false
		}
		total = next
	}
	if count == 0 || count > MaxSegments || total > uint64(len(target)) || total > uint64(^uint32(0)) {
		return nil, false
	}
	for index := range target[:total] {
		target[index] = 0
	}
	copy(target[:8], nativeMagic[:])
	binary.LittleEndian.PutUint16(target[8:], NativeVersion)
	binary.LittleEndian.PutUint16(target[10:], NativeHeaderSize)
	binary.LittleEndian.PutUint16(target[12:], uint16(count))
	binary.LittleEndian.PutUint64(target[16:], source.Entry)
	binary.LittleEndian.PutUint64(target[24:], source.StackTop)
	payloadOffset := headerEnd
	for index, segment := range source.Segments {
		offset := uint64(NativeHeaderSize) + uint64(index)*SegmentHeaderSize
		header := target[offset : offset+SegmentHeaderSize]
		binary.LittleEndian.PutUint64(header[0:], segment.VirtualAddress)
		binary.LittleEndian.PutUint32(header[8:], uint32(payloadOffset))
		binary.LittleEndian.PutUint32(header[12:], uint32(len(segment.Data)))
		binary.LittleEndian.PutUint32(header[16:], segment.MemorySize)
		binary.LittleEndian.PutUint32(header[20:], segment.Flags)
		copy(target[payloadOffset:], segment.Data)
		payloadOffset += uint64(len(segment.Data))
	}
	return target[:total], true
}
