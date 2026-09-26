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

func (service *Service) Dispatch(request Request) Reply {
	reply := Reply{Operation: request.Operation}
	if service == nil || service.backend == nil {
		reply.Status = StatusIOError
		return reply
	}
	switch request.Operation {
	case OperationInfo:
		reply.Capacity, reply.BlockSize = service.backend.Capacity(), service.backend.BlockSize()
	case OperationRead:
		if !within(service.backend.Capacity(), request.Offset, uint64(request.Length)) {
			reply.Status = StatusOutOfRange
			break
		}
		reply.Status = service.backend.ReadAt(request.Offset, service.buffer[:request.Length])
		if reply.Status == StatusOK {
			reply.Data = service.buffer[:request.Length]
		}
	case OperationWrite:
		if !within(service.backend.Capacity(), request.Offset, uint64(len(request.Data))) {
			reply.Status = StatusOutOfRange
			break
		}
		reply.Status = service.backend.WriteAt(request.Offset, request.Data)
	case OperationFlush:
		reply.Status = service.backend.Flush()
	case OperationReadBulk, OperationWriteBulk:
		if !within(service.backend.Capacity(), request.Offset, uint64(request.Length)) {
			reply.Status = StatusOutOfRange
			break
		}
		reply.Status = service.backend.Transfer(request)
	default:
		reply.Status = StatusInvalidArgument
	}
	return reply
}

func within(capacity, offset, length uint64) bool {
	return length != 0 && offset <= capacity && length <= capacity-offset
}
