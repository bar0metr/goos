package stream

import "goos/kernel/objects"

// MaxStreams is the system-wide stream limit. A stream is one page, its ring
// and counters; the registry holds a pointer per slot and takes the page when
// the stream is created and returns it when the stream is released (ADR
// 0111). PID 1 needs four streams for each job slot plus the terminal's
// output stream; the rest are the shares of ordinary processes (ADR 0089).
const (
	MaxStreams        = 256
	streamNamespace   = uint64(0x1000000000000000)
	writerEndpointBit = uint64(0x0800000000000000)
	// A stream ID carries its slot in its low bits and a creation count above
	// them, as a channel ID does.
	slotBits = 9
	slotMask = uint64(1)<<slotBits - 1
)

type Registry struct {
	streams [MaxStreams]*Stream
	ids     [MaxStreams]uint64
	// owners records which process is charged for each live stream. A stream
	// outlives the handles its creator held, because both endpoints can be
	// transferred, so the charge is returned on release rather than on close.
	owners  [MaxStreams]uint64
	next    uint64
	storage objects.Storage[Stream]
}

// SetStorage installs where streams are allocated; see ipc.Registry.
func (registry *Registry) SetStorage(storage objects.Storage[Stream]) { registry.storage = storage }

func (registry *Registry) memory() objects.Storage[Stream] { return objects.Or(registry.storage) }

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
		stream, allocated := registry.memory().New()
		if !allocated {
			return nil, 0, false
		}
		stream.Reset()
		registry.next++
		id := streamNamespace | registry.next<<slotBits | uint64(index)
		registry.streams[index], registry.ids[index], registry.owners[index] = stream, id, owner
		return stream, id, true
	}
	return nil, 0, false
}

func (registry *Registry) slot(id uint64) (int, bool) {
	index := id & slotMask
	if id&streamNamespace == 0 || id&writerEndpointBit != 0 || index >= MaxStreams || registry.ids[index] != id {
		return 0, false
	}
	return int(index), true
}

func (registry *Registry) Lookup(id uint64) (*Stream, bool) {
	index, found := registry.slot(id)
	if !found {
		return nil, false
	}
	return registry.streams[index], true
}

// Release frees the stream once both endpoints are closed and reports the
// process that was charged for it, which may already have exited. Its page
// goes back to the storage.
func (registry *Registry) Release(id uint64) (uint64, bool) {
	index, found := registry.slot(id)
	if !found || !registry.streams[index].Closed() {
		return 0, false
	}
	owner, stream := registry.owners[index], registry.streams[index]
	registry.ids[index], registry.owners[index], registry.streams[index] = 0, 0, nil
	registry.memory().Free(stream)
	return owner, true
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
