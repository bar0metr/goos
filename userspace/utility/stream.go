//go:build goos_userspace

package utility

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// Stream is a native byte stream whose reader and writer endpoints are both
// held by the calling process.
type Stream struct {
	Reader, Writer uint64
}

// CreateStream creates a stream charged to the calling process's quota.
func CreateStream() (Stream, bool) {
	reader, writer, status := sys.StreamCreate()
	if status != kcall.StatusOK {
		return Stream{}, false
	}
	return Stream{Reader: uint64(reader), Writer: uint64(writer)}, true
}

// Close closes both endpoints.
func (stream Stream) Close() {
	sys.HandleClose(handle.ID(stream.Reader))
	sys.HandleClose(handle.ID(stream.Writer))
}

// StreamWrite performs one native write of at most 256 bytes without waiting
// and returns its status and the bytes moved.
func StreamWrite(stream uint64, value []byte) (uint64, int) {
	written, status := sys.StreamWrite(handle.ID(stream), value)
	return uint64(status), written
}

// StreamRead performs one native read without waiting and returns its status
// and the bytes moved. An empty stream answers StatusResourceExhausted.
func StreamRead(stream uint64, target []byte) (uint64, int) {
	read, status := sys.StreamRead(handle.ID(stream), target)
	return uint64(status), read
}
