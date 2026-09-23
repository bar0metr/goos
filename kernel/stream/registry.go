package stream

const (
	MaxStreams        = 16
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
