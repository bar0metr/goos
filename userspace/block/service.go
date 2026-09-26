package block

type Backend interface {
	Capacity() uint64
	BlockSize() uint32
	ReadAt(offset uint64, target []byte) Status
	WriteAt(offset uint64, data []byte) Status
	Flush() Status
	// Transfer performs a bulk request, whose data moves through the object
	// the request carries (ADR 0107). The range has been checked.
	Transfer(request Request) Status
}

type Service struct {
	backend Backend
	buffer  [MaxData]byte
}

func NewService(backend Backend) (Service, bool) {
	if backend == nil || backend.Capacity() == 0 || backend.BlockSize() == 0 {
		return Service{}, false
	}
	return Service{backend: backend}, true
}

// Slice is the part of the device one client may reach: Length bytes from
// Start, and, unless Writable, only for reading. A client addresses its slice
// from zero and cannot name a byte outside it; the whole device is the slice
// from zero to its capacity (ADR 0115).
type Slice struct {
	Start    uint64
	Length   uint64
	Writable bool
}

// Translate maps a range of a slice to the device, or refuses one that does
// not lie wholly inside it. A zero length is refused as well: no request
// moves nothing.
func (slice Slice) Translate(offset, length uint64) (uint64, bool) {
	if !within(slice.Length, offset, length) {
		return 0, false
	}
	return slice.Start + offset, true
}

// Whole is the slice covering the entire device.
func (service *Service) Whole() Slice {
	if service == nil || service.backend == nil {
		return Slice{}
	}
	return Slice{Length: service.backend.Capacity(), Writable: true}
}

// Contains reports whether a slice lies inside the device.
func (service *Service) Contains(slice Slice) bool {
	return service != nil && service.backend != nil && slice.Length != 0 &&
		within(service.backend.Capacity(), slice.Start, slice.Length)
}

// Dispatch serves a request against the whole device.
func (service *Service) Dispatch(request Request) Reply {
	return service.DispatchIn(request, service.Whole())
}

// DispatchIn serves a request against a slice: every range is checked against
// the slice and moved to it before the device sees it, Info reports the
// slice's size, and a read-only slice refuses writes.
func (service *Service) DispatchIn(request Request, slice Slice) Reply {
	reply := Reply{Operation: request.Operation}
	if service == nil || service.backend == nil || !service.Contains(slice) {
		reply.Status = StatusIOError
		return reply
	}
	switch request.Operation {
	case OperationInfo:
		reply.Capacity, reply.BlockSize = slice.Length, service.backend.BlockSize()
	case OperationRead:
		offset, ok := slice.Translate(request.Offset, uint64(request.Length))
		if !ok {
			reply.Status = StatusOutOfRange
			break
		}
		reply.Status = service.backend.ReadAt(offset, service.buffer[:request.Length])
		if reply.Status == StatusOK {
			reply.Data = service.buffer[:request.Length]
		}
	case OperationWrite:
		offset, ok := slice.Translate(request.Offset, uint64(len(request.Data)))
		switch {
		case !ok:
			reply.Status = StatusOutOfRange
		case !slice.Writable:
			reply.Status = StatusReadOnly
		default:
			reply.Status = service.backend.WriteAt(offset, request.Data)
		}
	case OperationFlush:
		reply.Status = service.backend.Flush()
	case OperationReadBulk, OperationWriteBulk:
		offset, ok := slice.Translate(request.Offset, uint64(request.Length))
		switch {
		case !ok:
			reply.Status = StatusOutOfRange
		case request.Operation == OperationWriteBulk && !slice.Writable:
			reply.Status = StatusReadOnly
		default:
			request.Offset = offset
			reply.Status = service.backend.Transfer(request)
		}
	default:
		reply.Status = StatusInvalidArgument
	}
	return reply
}

func within(capacity, offset, length uint64) bool {
	return length != 0 && offset <= capacity && length <= capacity-offset
}
