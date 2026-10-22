//go:build !goos_userspace || goos_goroutines

package main

import (
	"unsafe"

	"goos/kernel/ipc"
	"goos/kernel/vfs"
	vfsservice "goos/userspace/vfs"
)

type driverRequestKind uint8

const (
	driverRead driverRequestKind = iota + 1
	driverServe
	driverControl
	driverClose
	driverOffline
	driverRecovery
)

// driverRequest holds the wire message and resources the owner must retain
// while the pool holds its ticket. Workers never access this table.
type driverRequest struct {
	ticket           uint64
	kind             driverRequestKind
	index            int
	session          uint64
	generation       uint64
	message          ipc.Message
	operation        vfs.Operation
	file             vfs.Handle
	mapBase, mapSize uint64
}

type driverRequests [vfsservice.ReadQueueCapacity]driverRequest

func (requests *driverRequests) put(request driverRequest) bool {
	if request.ticket == 0 || requests.find(request.ticket) != nil {
		return false
	}
	for index := range requests {
		if requests[index].ticket == 0 {
			requests[index] = request
			return true
		}
	}
	return false
}

func (requests *driverRequests) find(ticket uint64) *driverRequest {
	if ticket != 0 {
		for index := range requests {
			if requests[index].ticket == ticket {
				return &requests[index]
			}
		}
	}
	return nil
}

func (requests *driverRequests) release(ticket uint64) bool {
	if request := requests.find(ticket); request != nil {
		*request = driverRequest{}
		return true
	}
	return false
}

// readCompletionFault checks a delivered read before its reply copies Data:
// the pool's completion must belong to the request's session lifetime, and
// its bytes must lie in the buffer the request owns. owner is the session
// slot's owner, current its live generation; a stale but consistent
// completion is valid and its reply is dropped.
func readCompletionFault(request *driverRequest, completion vfsservice.ReadCompletion, owner vfs.Owner, current uint64) (vfsservice.CopyFault, bool) {
	valid := completion.Ticket == request.ticket && completion.Session.Owner == owner && completion.Session.Generation == request.generation && !completion.Barrier
	data := completion.Data
	switch request.operation {
	case vfs.OperationRead:
		valid = valid && len(data) == completion.Count && completion.Count <= vfs.MaxReplyData &&
			(len(data) == 0 || unsafe.SliceData(data) != nil)
	case vfs.OperationReadInto:
		decoded, decodedOK := vfs.DecodeRequest(request.message)
		end, windowOK := bulkReadWindow(request.mapSize, decoded.Offset, decoded.Size)
		valid = valid && decodedOK && windowOK && end >= decoded.Offset && request.mapBase != 0 &&
			completion.Count >= 0 && uint64(completion.Count) <= uint64(decoded.Size) && len(data) == completion.Count &&
			(completion.Count == 0 || uintptr(unsafe.Pointer(unsafe.SliceData(data))) == uintptr(request.mapBase+decoded.Offset))
	default:
		valid = false
	}
	if valid {
		return vfsservice.CopyFault{}, false
	}
	return vfsservice.CopyFault{
		Op: "reply", Src: uintptr(unsafe.Pointer(unsafe.SliceData(data))), Dst: uintptr(request.mapBase), Len: completion.Count,
		Session: completion.Session, Current: current, Ticket: completion.Ticket, State: "delivered", Worker: vfsservice.CopyWorkerOwner,
	}, true
}
