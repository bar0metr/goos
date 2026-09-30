// Package stream implements bounded unidirectional byte streams.
//
// Each endpoint counts the handles that refer to it, across every process
// (ADR 0089). An end closes when its last handle does, so an endpoint may be
// shared: every stage of a pipeline can hold the terminal's writing end, and
// the first to exit does not close it for the others.
package stream

// Capacity is the ring's size: with the stream's counters it fills two pages,
// which is what the registry allocates for a stream (ADR 0111). The ring holds
// more than the 4096 bytes a Linux program may write to a pipe in one piece.
const Capacity = 8176

// AtomicWrite is the largest write that is never split. A write of at most
// this many bytes lands whole or not at all, so writers sharing an endpoint
// cannot interleave inside one call. It equals the largest native stream call.
const AtomicWrite = 256

// MaxReferences bounds the handles one endpoint may have. The structural bound
// is lower: every handle lives in one of process.MaxProcesses tables of
// handle.MaxHandles slots, and a test holds that product below this. The check
// keeps the counter from wrapping even if that stops being true.
const MaxReferences = 1<<16 - 1

type Status uint8

const (
	StatusOK Status = iota
	StatusWouldBlock
	StatusPeerClosed
	StatusInvalidEndpoint
)

type Stream struct {
	data  [Capacity]byte
	head  uint16
	tail  uint16
	count uint16
	// readers and writers count the handles naming each end. They start at
	// one, for the two handles StreamCreate installs.
	readers    uint16
	writers    uint16
	readerOpen bool
	writerOpen bool
}

func New() Stream { return Stream{readers: 1, writers: 1, readerOpen: true, writerOpen: true} }

// Reset reopens a registry slot in place. Assigning New() instead would build
// the whole ring as a temporary, which the kernel's leaking heap never frees.
func (stream *Stream) Reset() {
	clear(stream.data[:])
	stream.head, stream.tail, stream.count = 0, 0, 0
	stream.readers, stream.writers = 1, 1
	stream.readerOpen, stream.writerOpen = true, true
}

// Retain counts one more handle naming an end. It refuses an end that has
// closed, whose last handle is already gone, and a count at its bound.
func (stream *Stream) Retain(writer bool) bool {
	if stream == nil {
		return false
	}
	references, open := &stream.readers, stream.readerOpen
	if writer {
		references, open = &stream.writers, stream.writerOpen
	}
	if !open || *references == 0 || *references >= MaxReferences {
		return false
	}
	*references++
	return true
}

// CanRetain reports whether extra more handles may name an end.
func (stream *Stream) CanRetain(writer bool, extra int) bool {
	if stream == nil || extra < 0 {
		return false
	}
	references, open := stream.readers, stream.readerOpen
	if writer {
		references, open = stream.writers, stream.writerOpen
	}
	return open && references != 0 && int(references)+extra <= MaxReferences
}

// Drop forgets one handle naming an end and closes the end when it was the
// last. It reports whether the end closed, and refuses, changing nothing, when
// no handle is counted: an underflow is a kernel accounting fault, and wrapping
// the counter would keep an end open for ever.
func (stream *Stream) Drop(writer bool) (closed bool, ok bool) {
	if stream == nil {
		return false, false
	}
	references := &stream.readers
	if writer {
		references = &stream.writers
	}
	if *references == 0 {
		return false, false
	}
	*references--
	if *references != 0 {
		return false, true
	}
	if writer {
		return stream.CloseWriter(), true
	}
	return stream.CloseReader(), true
}

// References reports how many handles name each end.
func (stream *Stream) References() (readers, writers uint16) {
	if stream == nil {
		return 0, 0
	}
	return stream.readers, stream.writers
}

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
	size := len(source)
	// A write that fits in one call is not split: with several writers on one
	// end, a partial write would let another writer's bytes land in the middle.
	if available == 0 || size <= AtomicWrite && size > available {
		return 0, StatusWouldBlock
	}
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
	stream.readers = 0
	stream.count = 0
	stream.head, stream.tail = 0, 0
	return true
}

func (stream *Stream) CloseWriter() bool {
	if stream == nil || !stream.writerOpen {
		return false
	}
	stream.writerOpen = false
	stream.writers = 0
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
	return stream.WriteStatusFor(AtomicWrite)
}

// WriteStatusFor reports whether a write of size bytes fits the ring whole.
func (stream *Stream) WriteStatusFor(size int) Status {
	if stream == nil || !stream.writerOpen {
		return StatusInvalidEndpoint
	}
	if size <= 0 || size > Capacity {
		return StatusInvalidEndpoint
	}
	if !stream.readerOpen {
		return StatusPeerClosed
	}
	if Capacity-int(stream.count) < size {
		return StatusWouldBlock
	}
	return StatusOK
}
