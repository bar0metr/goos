package stream

const (
	MaxStreams        = 16
	streamNamespace   = uint64(0x1000000000000000)
	writerEndpointBit = uint64(0x0800000000000000)
)

type Registry struct {
	streams [MaxStreams]Stream
	ids     [MaxStreams]uint64
	next    uint64
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

func (registry *Registry) Create() (*Stream, uint64, bool) {
	for index := range registry.ids {
		if registry.ids[index] != 0 {
			continue
		}
		registry.next++
		id := streamNamespace | registry.next
		registry.ids[index] = id
		registry.streams[index] = New()
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

func (registry *Registry) Release(id uint64) bool {
	for index, candidate := range registry.ids {
		if candidate == id && registry.streams[index].Closed() {
			registry.ids[index] = 0
			registry.streams[index] = Stream{}
			return true
		}
	}
	return false
}
