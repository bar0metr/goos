//go:build goos_userspace

package utility

import "unsafe"

//go:linkname streamCreate goos_stream_create
func streamCreate(address unsafe.Pointer, size uintptr) uint64

// Stream is a native byte stream whose reader and writer endpoints are both
// held by the calling process.
type Stream struct {
	Reader, Writer uint64
}

var streamHandles [2]uint64

// CreateStream creates a stream charged to the calling process's quota.
func CreateStream() (Stream, bool) {
	if streamCreate(unsafe.Pointer(&streamHandles[0]), 16) != 0 {
		return Stream{}, false
	}
	return Stream{Reader: streamHandles[0], Writer: streamHandles[1]}, true
}

// Close closes both endpoints.
func (stream Stream) Close() {
	handleClose(stream.Reader)
	handleClose(stream.Writer)
}

// StreamWrite performs one native write of at most 256 bytes without waiting
// and returns its status and the bytes moved.
func StreamWrite(stream uint64, value []byte) (uint64, int) {
	streamDescriptor[0] = uint64(uintptr(unsafe.Pointer(&value[0])))
	streamDescriptor[1], streamDescriptor[2] = uint64(len(value)), 0
	status := streamWrite(stream, unsafe.Pointer(&streamDescriptor[0]), 24)
	return status, int(streamDescriptor[2])
}

// StreamRead performs one native read without waiting and returns its status
// and the bytes moved. An empty stream answers StatusResourceExhausted.
func StreamRead(stream uint64, target []byte) (uint64, int) {
	streamDescriptor[0] = uint64(uintptr(unsafe.Pointer(&target[0])))
	streamDescriptor[1], streamDescriptor[2] = uint64(len(target)), 0
	status := streamRead(stream, unsafe.Pointer(&streamDescriptor[0]), 24)
	return status, int(streamDescriptor[2])
}
