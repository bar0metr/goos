package ipc

const (
	MaxChannels      = 16
	channelNamespace = uint64(0x4000000000000000)
)

type Registry struct {
	channels [MaxChannels]Channel
	ids      [MaxChannels]uint64
	next     uint64
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
