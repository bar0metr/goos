package ipc

import (
	"goos/kernel/lock"
	"goos/kernel/objects"
)

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

// Registry is the table of live channels. It locks itself with a short
// slots lock that covers only finding, adding, and removing a channel; each
// channel has a lock of its own for its queues and endpoint state (ADR 0187).
// Releasing a channel takes its lock after the slot is cleared, so a caller
// that found the channel under the slots lock finishes before the memory
// goes back.
type Registry struct {
	slots    lock.Spin
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

func (registry *Registry) lock() {
	registry.slots.SetLevel(lock.LevelChannelSlots)
	registry.slots.Lock()
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

// Create records owner as the process accountable for the channel. The
// caller locks the channel before it uses it.
func (registry *Registry) Create(owner uint64) (*Channel, uint64, bool) {
	registry.lock()
	for index := range registry.ids {
		if registry.ids[index] != 0 {
			continue
		}
		channel, allocated := registry.memory().New()
		if !allocated {
			break
		}
		channel.Reset()
		registry.next++
		id := channelNamespace | registry.next<<slotBits | uint64(index)
		registry.channels[index], registry.ids[index], registry.owners[index] = channel, id, owner
		registry.slots.Unlock()
		return channel, id, true
	}
	registry.slots.Unlock()
	return nil, 0, false
}

// slot returns the registry slot a channel ID names, if it is live. The
// slots lock is held.
func (registry *Registry) slot(id uint64) (int, bool) {
	index := id & slotMask
	if id&channelNamespace == 0 || id&endpointSideBit != 0 || index >= MaxChannels || registry.ids[index] != id {
		return 0, false
	}
	return int(index), true
}

// Acquire returns the live channel id names with its lock held. The caller
// unlocks it.
func (registry *Registry) Acquire(id uint64) (*Channel, bool) {
	registry.lock()
	index, found := registry.slot(id)
	if !found {
		registry.slots.Unlock()
		return nil, false
	}
	channel := registry.channels[index]
	channel.Lock()
	registry.slots.Unlock()
	return channel, true
}

// Live reports whether id names a live channel.
func (registry *Registry) Live(id uint64) bool {
	registry.lock()
	_, found := registry.slot(id)
	registry.slots.Unlock()
	return found
}

// Close closes one side of the channel id names and reports whether this
// call closed it, so a peer's death is published once. The call that closes
// the second side removes the channel and returns its memory to the storage,
// and reports the process that was charged for it, which may already have
// exited.
func (registry *Registry) Close(id uint64, side uint8) (closed bool, owner uint64, released bool) {
	registry.lock()
	index, found := registry.slot(id)
	if !found {
		registry.slots.Unlock()
		return false, 0, false
	}
	channel := registry.channels[index]
	channel.Lock()
	closed = channel.Close(side)
	if !closed || channel.alive[0] || channel.alive[1] {
		channel.Unlock()
		registry.slots.Unlock()
		return closed, 0, false
	}
	owner = registry.owners[index]
	registry.ids[index], registry.owners[index], registry.channels[index] = 0, 0, nil
	channel.Unlock()
	registry.slots.Unlock()
	registry.memory().Free(channel)
	return true, owner, true
}

// AcquireAt returns the live channel in slot index, if any, with its lock
// held. The caller unlocks it.
func (registry *Registry) AcquireAt(index int) (uint64, *Channel, bool) {
	registry.lock()
	if index < 0 || index >= MaxChannels || registry.ids[index] == 0 {
		registry.slots.Unlock()
		return 0, nil, false
	}
	id, channel := registry.ids[index], registry.channels[index]
	channel.Lock()
	registry.slots.Unlock()
	return id, channel, true
}
