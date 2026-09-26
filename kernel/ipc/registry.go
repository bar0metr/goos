package ipc

import "goos/kernel/objects"

// MaxChannels is the system-wide channel limit. A channel embeds two
// 16-message queues, about 10.5 KiB; the registry holds only a pointer per
// slot and takes a channel's pages when it is created and returns them when
// it is released, so a running system pays for the channels it has (ADR
// 0111). PID 1, the only process that creates channels, holds fourteen at
// rest and one more per VFS session it opens for a command; 256 is room for
// every job slot's pipeline and every service session at once.
const (
	MaxChannels      = 256
	channelNamespace = uint64(0x4000000000000000)
	endpointSideBit  = uint64(0x2000000000000000)
	// A channel ID carries its registry slot in its low bits, so a lookup is
	// one comparison, and a creation count above them, so an ID is never
	// reused and a stale one finds nothing.
	slotBits = 9
	slotMask = uint64(1)<<slotBits - 1
)

type Registry struct {
	channels [MaxChannels]*Channel
	ids      [MaxChannels]uint64
	// owners records which process is charged for each live channel, so the
	// charge is returned when the channel really goes away rather than when
	// one holder happens to close its endpoint.
	owners  [MaxChannels]uint64
	next    uint64
	storage objects.Storage[Channel]
}

// SetStorage installs where channels are allocated. The kernel installs page
// storage at boot; without it, channels come from the Go heap.
func (registry *Registry) SetStorage(storage objects.Storage[Channel]) { registry.storage = storage }

func (registry *Registry) memory() objects.Storage[Channel] { return objects.Or(registry.storage) }

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

// Create records owner as the process accountable for the channel.
func (registry *Registry) Create(owner uint64) (*Channel, uint64, bool) {
	for index := range registry.ids {
		if registry.ids[index] != 0 {
			continue
		}
		channel, allocated := registry.memory().New()
		if !allocated {
			return nil, 0, false
		}
		channel.Reset()
		registry.next++
		id := channelNamespace | registry.next<<slotBits | uint64(index)
		registry.channels[index], registry.ids[index], registry.owners[index] = channel, id, owner
		return channel, id, true
	}
	return nil, 0, false
}

// slot returns the registry slot a channel ID names, if it is live.
func (registry *Registry) slot(id uint64) (int, bool) {
	index := id & slotMask
	if id&channelNamespace == 0 || id&endpointSideBit != 0 || index >= MaxChannels || registry.ids[index] != id {
		return 0, false
	}
	return int(index), true
}

func (registry *Registry) Lookup(id uint64) (*Channel, bool) {
	index, found := registry.slot(id)
	if !found {
		return nil, false
	}
	return registry.channels[index], true
}

// Release frees the channel once both endpoints are closed and reports the
// process that was charged for it, which may already have exited. Its memory
// goes back to the storage; nothing may use the channel afterwards.
func (registry *Registry) Release(id uint64) (uint64, bool) {
	index, found := registry.slot(id)
	if !found || registry.channels[index].Alive(0) || registry.channels[index].Alive(1) {
		return 0, false
	}
	owner, channel := registry.owners[index], registry.channels[index]
	registry.ids[index], registry.owners[index], registry.channels[index] = 0, 0, nil
	registry.memory().Free(channel)
	return owner, true
}

// At returns the live channel stored in slot index, if any.
func (registry *Registry) At(index int) (uint64, *Channel, bool) {
	if index < 0 || index >= MaxChannels || registry.ids[index] == 0 {
		return 0, nil, false
	}
	return registry.ids[index], registry.channels[index], true
}
