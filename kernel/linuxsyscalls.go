//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	"goos/kernel/sched"
	"goos/kernel/shmem"
	"goos/kernel/stream"
)

type linuxCallHandler struct {
	root uint64
	task sched.TaskID
}

var linuxHandler linuxCallHandler
var linuxIOBuffer [stream.AtomicWrite]byte
var linuxIOVecWire [linuxcompat.MaxIOVec * linuxabi.IOVecSize]byte

// fileDescription returns the description behind fd when it names a VFS
// file or directory. Stream descriptors keep their own path through endpoint.
func (handler *linuxCallHandler) fileDescription(fd uint64) (*linuxcompat.FileDescription, bool) {
	description, found := linuxcompat.DescriptionOf(handler.root, fd)
	if !found || description.Kind != linuxcompat.FDVFSFile && description.Kind != linuxcompat.FDVFSDirectory {
		return nil, false
	}
	return description, true
}

func (handler *linuxCallHandler) Read(request linuxcompat.Request) linuxcompat.Result {
	return handler.read(request, false)
}

// Readv reads into a bounded vector. A stream fills the elements in order
// with what one read delivers; a file does the same with one VFS read.
func (handler *linuxCallHandler) Readv(request linuxcompat.Request) linuxcompat.Result {
	return handler.read(request, true)
}

func (handler *linuxCallHandler) read(request linuxcompat.Request, vector bool) linuxcompat.Result {
	if description, file := handler.fileDescription(request.Arguments[0]); file {
		if description.Kind == linuxcompat.FDVFSDirectory {
			return linuxcompat.Result{Errno: linuxabi.EISDIR}
		}
		return handler.readFile(request, description, vector)
	}
	if errno := handler.consumeWake(request); errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	endpoint, streamID, errno := handler.endpoint(int64(request.Arguments[0]), false)
	if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	limit, errno := handler.readLimit(request, vector, stream.AtomicWrite)
	if errno != 0 || limit == 0 {
		return linuxcompat.Result{Errno: errno}
	}
	for attempt := 0; attempt < 2; attempt++ {
		count, status := endpoint.Read(linuxIOBuffer[:limit])
		switch status {
		case stream.StatusOK:
			wakeStreamWaiters(streamID, 1, ipc.WaitMessage)
			if !handler.deliverRead(request, vector, linuxIOBuffer[:count], nil, uint64(count)) {
				return linuxcompat.Result{Errno: linuxabi.EFAULT}
			}
			return linuxcompat.Result{Value: uint64(count)}
		case stream.StatusPeerClosed:
			return linuxcompat.Result{}
		case stream.StatusWouldBlock:
			blocked, retry := handler.blockStream(request, endpoint, streamID, 0, 0)
			if blocked.Action == linuxabi.ActionBlock || !retry {
				return blocked
			}
		default:
			return linuxcompat.Result{Errno: linuxcompat.StreamReadErrno(status)}
		}
	}
	return linuxcompat.Result{Errno: linuxabi.EIO}
}

// readLimit checks where a read or readv will put its bytes and returns how
// many one call may deliver, at most bound. The destination is checked
// before any byte leaves its source, so a bad pointer costs the caller no
// data.
func (handler *linuxCallHandler) readLimit(request linuxcompat.Request, vector bool, bound uint64) (uint64, linuxabi.Errno) {
	if !vector {
		address, limit := request.Arguments[1], request.Arguments[2]
		if limit > bound {
			limit = bound
		}
		if limit != 0 && !vm.CanCopyOut(handler.root, address, limit) {
			return 0, linuxabi.EFAULT
		}
		return limit, 0
	}
	wire, errno := handler.copyIOVec(request)
	if errno != 0 {
		return 0, errno
	}
	total := uint64(0)
	for index := 0; index < len(wire)/linuxabi.IOVecSize && total < bound; index++ {
		base, length := linuxcompat.IOVecEntry(wire, index)
		if length > bound-total {
			length = bound - total
		}
		if length != 0 && !vm.CanCopyOut(handler.root, base, length) {
			return 0, linuxabi.EFAULT
		}
		total += length
	}
	return total, 0
}

// copyIOVec copies and validates the vector of a readv or writev.
func (handler *linuxCallHandler) copyIOVec(request linuxcompat.Request) ([]byte, linuxabi.Errno) {
	count := request.Arguments[2]
	if count > linuxcompat.MaxIOVec {
		return nil, linuxabi.EINVAL
	}
	wire := linuxIOVecWire[:count*linuxabi.IOVecSize]
	if count != 0 && !vm.CopyIn(handler.root, request.Arguments[1], wire) {
		return nil, linuxabi.EFAULT
	}
	if errno := linuxcompat.ValidateIOVec(wire, vm.UserAddressLimit); errno != 0 {
		return nil, errno
	}
	return wire, 0
}

// deliverRead copies what a read produced to the caller: to one address, or
// across the vector's elements in order. The bytes come from data, or from
// the I/O object when data is nil; size is how many there are.
func (handler *linuxCallHandler) deliverRead(request linuxcompat.Request, vector bool, data []byte, object *shmem.Object, size uint64) bool {
	if !vector {
		return handler.deliver(data, object, 0, request.Arguments[1], size)
	}
	wire, errno := handler.copyIOVec(request)
	if errno != 0 {
		return false
	}
	delivered := uint64(0)
	for index := 0; index < len(wire)/linuxabi.IOVecSize && delivered < size; index++ {
		base, length := linuxcompat.IOVecEntry(wire, index)
		if length > size-delivered {
			length = size - delivered
		}
		if !handler.deliver(data, object, delivered, base, length) {
			return false
		}
		delivered += length
	}
	return delivered == size
}

func (handler *linuxCallHandler) deliver(data []byte, object *shmem.Object, offset uint64, address uint64, length uint64) bool {
	if length == 0 {
		return true
	}
	if object != nil {
		return handler.ioTransfer(object, offset, address, length, true)
	}
	return vm.CopyOut(handler.root, address, data[offset:offset+length])
}

func (handler *linuxCallHandler) Write(request linuxcompat.Request) linuxcompat.Result {
	if description, file := handler.fileDescription(request.Arguments[0]); file {
		if description.Kind == linuxcompat.FDVFSDirectory {
			return linuxcompat.Result{Errno: linuxabi.EBADF}
		}
		return handler.writeFile(request, description, false)
	}
	if errno := handler.consumeWake(request); errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	endpoint, streamID, errno := handler.endpoint(int64(request.Arguments[0]), true)
	if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	address, requested := request.Arguments[1], request.Arguments[2]
	if requested == 0 {
		return linuxcompat.Result{}
	}
	limit := requested
	if limit > stream.Capacity {
		limit = stream.Capacity
	}
	if !vm.ValidateUserRange(address, limit) {
		return linuxcompat.Result{Errno: linuxabi.EFAULT}
	}
	written, result, final := handler.writeRange(request, endpoint, streamID, address, limit, false)
	if final {
		return result
	}
	return linuxcompat.Result{Value: written}
}

// writeRange copies one validated user range to a stream in atomic chunks.
// final reports that result ends the syscall: an error, or a block taken
// because nothing had been written. progressed says an earlier vector element
// already wrote bytes, so this range must report a stall instead of blocking.
func (handler *linuxCallHandler) writeRange(request linuxcompat.Request, endpoint *stream.Stream, streamID uint64, address uint64, limit uint64, progressed bool) (uint64, linuxcompat.Result, bool) {
	written := uint64(0)
	for written < limit {
		chunk := min64(limit-written, stream.AtomicWrite)
		if !vm.CopyIn(handler.root, address+written, linuxIOBuffer[:chunk]) {
			if written != 0 || progressed {
				return written, linuxcompat.Result{}, false
			}
			return 0, linuxcompat.Result{Errno: linuxabi.EFAULT}, true
		}
		count, status := endpoint.Write(linuxIOBuffer[:chunk])
		if status == stream.StatusOK {
			written += uint64(count)
			wakeStreamWaiters(streamID, 0, ipc.WaitMessage)
			continue
		}
		if written != 0 || progressed {
			return written, linuxcompat.Result{}, false
		}
		if status == stream.StatusWouldBlock {
			blocked, retry := handler.blockStream(request, endpoint, streamID, 1, int(chunk))
			if blocked.Action == linuxabi.ActionBlock || !retry {
				return 0, blocked, true
			}
			continue
		}
		return 0, linuxcompat.Result{Errno: linuxcompat.StreamWriteErrno(status)}, true
	}
	return written, linuxcompat.Result{}, false
}

// Writev writes a bounded vector to a stream or a file. Every element is
// validated before the first byte moves, as Linux does. A stalled or
// oversized vector returns the bytes already written; the caller continues
// from there.
func (handler *linuxCallHandler) Writev(request linuxcompat.Request) linuxcompat.Result {
	if description, file := handler.fileDescription(request.Arguments[0]); file {
		if description.Kind == linuxcompat.FDVFSDirectory {
			return linuxcompat.Result{Errno: linuxabi.EBADF}
		}
		return handler.writeFile(request, description, true)
	}
	if errno := handler.consumeWake(request); errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	endpoint, streamID, errno := handler.endpoint(int64(request.Arguments[0]), true)
	if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	count := request.Arguments[2]
	if count > linuxcompat.MaxIOVec {
		return linuxcompat.Result{Errno: linuxabi.EINVAL}
	}
	if count == 0 {
		return linuxcompat.Result{}
	}
	wire := linuxIOVecWire[:count*linuxabi.IOVecSize]
	if !vm.CopyIn(handler.root, request.Arguments[1], wire) {
		return linuxcompat.Result{Errno: linuxabi.EFAULT}
	}
	if errno := linuxcompat.ValidateIOVec(wire, vm.UserAddressLimit); errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	total := uint64(0)
	for index := uint64(0); index < count && total < linuxcompat.MaxVectorBytes; index++ {
		base, length := linuxcompat.IOVecEntry(wire, int(index))
		if length > linuxcompat.MaxVectorBytes-total {
			length = linuxcompat.MaxVectorBytes - total
		}
		if length == 0 {
			continue
		}
		written, result, final := handler.writeRange(request, endpoint, streamID, base, length, total != 0)
		if final {
			return result
		}
		total += written
		if written < length {
			break
		}
	}
	return linuxcompat.Result{Value: total}
}

// SetTidAddress records the clear-child-tid address and returns the caller's
// thread ID, which for a single-task process is its compatibility PID.
func (handler *linuxCallHandler) SetTidAddress(request linuxcompat.Request) linuxcompat.Result {
	state, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxcompat.Result{Errno: linuxabi.EINVAL}
	}
	state.ClearChildTID = request.Arguments[0]
	return linuxcompat.Result{Value: uint64(state.PID)}
}

func (handler *linuxCallHandler) Exit(request linuxcompat.Request, group bool) linuxcompat.Result {
	code := uint64(uint8(request.Arguments[0]))
	linuxcompat.SyscallContinuations.Cancel(handler.task)
	if group {
		if owner, found := processRegistry.ByRoot(handler.root); found {
			for _, task := range owner.Tasks() {
				if task == handler.task {
					continue
				}
				cancelTaskWaits(task)
				amd64.TerminateTask(uint64(task), code)
			}
		}
	}
	amd64.SetCurrentTaskExitCode(code)
	return linuxcompat.Result{Action: linuxabi.ActionExit}
}

func (handler *linuxCallHandler) endpoint(fd int64, writer bool) (*stream.Stream, uint64, linuxabi.Errno) {
	if fd < 0 || fd >= linuxcompat.LinuxMaxFD {
		return nil, 0, linuxabi.EBADF
	}
	description, found := linuxcompat.LookupFD(handler.root, int(fd))
	wantKind, rights := linuxcompat.FDStreamReader, capability.Read
	if writer {
		wantKind, rights = linuxcompat.FDStreamWriter, capability.Write
	}
	if !found || description.Kind != wantKind {
		return nil, 0, linuxabi.EBADF
	}
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return nil, 0, linuxabi.EBADF
	}
	cap, allowed := owner.Handles().Lookup(handle.ID(description.Object), handle.TypeStream, rights)
	streamID, isWriter, valid := stream.DecodeEndpointObject(cap.Object())
	endpoint, exists := streamRegistry.Lookup(streamID)
	if !allowed || !valid || !exists || isWriter != writer {
		return nil, 0, linuxabi.EBADF
	}
	return endpoint, streamID, 0
}

func (handler *linuxCallHandler) consumeWake(request linuxcompat.Request) linuxabi.Errno {
	state, found := linuxcompat.SyscallContinuations.Pending(handler.task)
	if !found {
		return 0
	}
	if state.Number != request.Number || state.Arguments != request.Arguments || state.Phase != linuxcompat.ContinuationReady || state.Wake == 0 {
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		streamWaitSet.Cancel(handler.task)
		return linuxabi.EINVAL
	}
	if !linuxcompat.SyscallContinuations.Finish(handler.task) {
		return linuxabi.EIO
	}
	return 0
}

func (handler *linuxCallHandler) blockStream(request linuxcompat.Request, endpoint *stream.Stream, streamID uint64, side uint8, writeSize int) (linuxcompat.Result, bool) {
	if !linuxcompat.SyscallContinuations.Begin(handler.task, request, nil) ||
		!linuxcompat.SyscallContinuations.Issue(handler.task, 1, streamID) ||
		!streamWaitSet.Add(handler.task, streamID, side, ^uint64(0)) {
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		streamWaitSet.Cancel(handler.task)
		return linuxcompat.Result{Errno: linuxabi.ENOMEM}, false
	}
	status := endpoint.ReadStatus()
	if side == 1 {
		status = endpoint.WriteStatusFor(writeSize)
	}
	if status != stream.StatusWouldBlock {
		streamWaitSet.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		return linuxcompat.Result{}, true
	}
	if !linuxcompat.SyscallContinuations.Block(handler.task, 1) {
		streamWaitSet.Cancel(handler.task)
		linuxcompat.SyscallContinuations.Cancel(handler.task)
		return linuxcompat.Result{Errno: linuxabi.EIO}, false
	}
	return linuxcompat.Result{Action: linuxabi.ActionBlock}, false
}

func min64(value uint64, bound int) int {
	if value > uint64(bound) {
		return bound
	}
	return int(value)
}
