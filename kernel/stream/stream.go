// Package stream implements bounded unidirectional byte streams.
package stream

const Capacity = 4096

type Status uint8

const (
	StatusOK Status = iota
	StatusWouldBlock
	StatusPeerClosed
	StatusInvalidEndpoint
)

type Stream struct {
	data       [Capacity]byte
	head       uint16
	tail       uint16
	count      uint16
	readerOpen bool
	writerOpen bool
}

func New() Stream { return Stream{readerOpen: true, writerOpen: true} }

func (stream *Stream) Read(target []byte) (int, Status) {
	if stream == nil || !stream.readerOpen || len(target) == 0 {
		return 0, StatusInvalidEndpoint
	}
	if stream.count == 0 {
		if !stream.writerOpen {
			return 0, StatusPeerClosed
		}
		return 0, StatusWouldBlock
	}
	size := len(target)
	if size > int(stream.count) {
		size = int(stream.count)
	}
	for index := 0; index < size; index++ {
		target[index] = stream.data[stream.head]
		stream.head = (stream.head + 1) % Capacity
	}
	stream.count -= uint16(size)
	return size, StatusOK
}

func (stream *Stream) Write(source []byte) (int, Status) {
	if stream == nil || !stream.writerOpen || len(source) == 0 {
		return 0, StatusInvalidEndpoint
	}
	if !stream.readerOpen {
		return 0, StatusPeerClosed
	}
	available := Capacity - int(stream.count)
	if available == 0 {
		return 0, StatusWouldBlock
	}
	size := len(source)
	if size > available {
		size = available
	}
	for index := 0; index < size; index++ {
		stream.data[stream.tail] = source[index]
		stream.tail = (stream.tail + 1) % Capacity
	}
	stream.count += uint16(size)
	return size, StatusOK
}

func (stream *Stream) CloseReader() bool {
	if stream == nil || !stream.readerOpen {
		return false
	}
	stream.readerOpen = false
	stream.count = 0
	stream.head, stream.tail = 0, 0
	return true
}

func (stream *Stream) CloseWriter() bool {
	if stream == nil || !stream.writerOpen {
		return false
	}
	stream.writerOpen = false
	return true
}

func (stream *Stream) Closed() bool { return stream == nil || !stream.readerOpen && !stream.writerOpen }

func (stream *Stream) ReadStatus() Status {
	if stream == nil || !stream.readerOpen {
		return StatusInvalidEndpoint
	}
	if stream.count != 0 {
		return StatusOK
	}
	if !stream.writerOpen {
		return StatusPeerClosed
	}
	return StatusWouldBlock
}

func (stream *Stream) WriteStatus() Status {
	if stream == nil || !stream.writerOpen {
		return StatusInvalidEndpoint
	}
	if !stream.readerOpen {
		return StatusPeerClosed
	}
	if stream.count == Capacity {
		return StatusWouldBlock
	}
	return StatusOK
}
