package ipc

const (
	MaxChannels      = 16
	channelNamespace = uint64(0x4000000000000000)
	endpointSideBit  = uint64(0x2000000000000000)
)

type Registry struct {
	channels [MaxChannels]Channel
	ids      [MaxChannels]uint64
	next     uint64
}

func EndpointObject(channel uint64, side uint8) (uint64, bool) {
	if channel&channelNamespace == 0 || channel&endpointSideBit != 0 || side > 1 {
		return 0, false
	}
	return channel | uint64(side)*endpointSideBit, true
}

func DecodeEndpointObject(object uint64) (uint64, uint8, bool) {
	if object&channelNamespace == 0 {
		return 0, 0, false
	}
	side := uint8(0)
	if object&endpointSideBit != 0 {
		side = 1
	}
	return object &^ endpointSideBit, side, true
}

func (registry *Registry) Create() (*Channel, uint64, bool) {
	for index := range registry.ids {
		if registry.ids[index] == 0 {
			registry.next++
			id := channelNamespace | registry.next
			registry.ids[index] = id
			registry.channels[index] = NewChannel()
			return &registry.channels[index], id, true
		}
	}
	return nil, 0, false
}

func (registry *Registry) Lookup(id uint64) (*Channel, bool) {
	for index, candidate := range registry.ids {
		if candidate == id {
			return &registry.channels[index], true
		}
	}
	return nil, false
}

func (registry *Registry) Release(id uint64) bool {
	for index, candidate := range registry.ids {
		if candidate == id && !registry.channels[index].Alive(0) && !registry.channels[index].Alive(1) {
			registry.ids[index] = 0
			registry.channels[index] = Channel{}
			return true
		}
	}
	return false
}

// At returns the live channel stored in slot index, if any.
func (registry *Registry) At(index int) (uint64, *Channel, bool) {
	if index < 0 || index >= MaxChannels || registry.ids[index] == 0 {
		return 0, nil, false
	}
	return registry.ids[index], &registry.channels[index], true
}
