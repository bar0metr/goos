package stream

// MaxStreams sizes the registry. Each stream embeds its 4 KiB ring, so the
// table is 64 x 4 KiB = 256 KiB of static kernel memory; it is a fixed array
// because the kernel heap never frees (ADR 0065). PID 1 needs at most 17 at
// once — four per job for its four job slots plus the terminal's output
// stream — and its quota of 24 leaves 40, ten ordinary processes at their
// full share of four (ADR 0089).
const (
	MaxStreams        = 64
	streamNamespace   = uint64(0x1000000000000000)
	writerEndpointBit = uint64(0x0800000000000000)
)

type Registry struct {
	streams [MaxStreams]Stream
	ids     [MaxStreams]uint64
	// owners records which process is charged for each live stream. A stream
	// outlives the handles its creator held, because both endpoints can be
	// transferred, so the charge is returned on release rather than on close.
	owners [MaxStreams]uint64
	next   uint64
}

func ReaderObject(id uint64) (uint64, bool) {
	return endpointObject(id, false)
}

func WriterObject(id uint64) (uint64, bool) {
	return endpointObject(id, true)
}

func endpointObject(id uint64, writer bool) (uint64, bool) {
	if id&streamNamespace == 0 || id&writerEndpointBit != 0 {
		return 0, false
	}
	if writer {
		id |= writerEndpointBit
	}
	return id, true
}

func DecodeEndpointObject(object uint64) (uint64, bool, bool) {
	if object&streamNamespace == 0 {
		return 0, false, false
	}
	writer := object&writerEndpointBit != 0
	return object &^ writerEndpointBit, writer, true
}

// Create records owner as the process accountable for the stream.
func (registry *Registry) Create(owner uint64) (*Stream, uint64, bool) {
	for index := range registry.ids {
		if registry.ids[index] != 0 {
			continue
		}
		registry.next++
		id := streamNamespace | registry.next
		registry.ids[index] = id
		registry.owners[index] = owner
		registry.streams[index].Reset()
		return &registry.streams[index], id, true
	}
	return nil, 0, false
}

func (registry *Registry) Lookup(id uint64) (*Stream, bool) {
	for index, candidate := range registry.ids {
		if candidate == id {
			return &registry.streams[index], true
		}
	}
	return nil, false
}

// Release frees the slot once both endpoints are closed and reports the process
// that was charged for it, which may already have exited.
func (registry *Registry) Release(id uint64) (uint64, bool) {
	for index, candidate := range registry.ids {
		if candidate == id && registry.streams[index].Closed() {
			owner := registry.owners[index]
			registry.ids[index], registry.owners[index] = 0, 0
			registry.streams[index] = Stream{}
			return owner, true
		}
	}
	return 0, false
}

// endpoint resolves an endpoint object to its live stream and direction.
func (registry *Registry) endpoint(object uint64) (*Stream, bool, bool) {
	id, writer, valid := DecodeEndpointObject(object)
	if !valid {
		return nil, false, false
	}
	stream, found := registry.Lookup(id)
	return stream, writer, found
}

// Retain counts one more handle naming the endpoint object. Every path that
// places a stream capability into a handle table calls it, except creation,
// which starts each end at one.
func (registry *Registry) Retain(object uint64) bool {
	stream, writer, found := registry.endpoint(object)
	return found && stream.Retain(writer)
}

// CanRetain reports whether extra more handles may name the endpoint object,
// so a caller can refuse before it installs anything rather than unwind after.
func (registry *Registry) CanRetain(object uint64, extra int) bool {
	stream, writer, found := registry.endpoint(object)
	return found && stream.CanRetain(writer, extra)
}

// Drop forgets one handle naming the endpoint object. It returns the stream ID
// and direction, whether that was the end's last handle, and false when the
// object names no live stream or the end has no handle counted.
func (registry *Registry) Drop(object uint64) (id uint64, writer bool, closed bool, ok bool) {
	stream, writer, found := registry.endpoint(object)
	if !found {
		return 0, false, false, false
	}
	closed, ok = stream.Drop(writer)
	id, _, _ = DecodeEndpointObject(object)
	return id, writer, closed, ok
}
