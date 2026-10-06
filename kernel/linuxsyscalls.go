//go:build uefi

package main

import (
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	"goos/kernel/sched"
	"goos/kernel/shmem"
	"goos/kernel/stream"
)

type linuxCallHandler struct {
	root uint64
	task sched.TaskID
	// frame is the entry frame of the call being served, with the caller's
	// saved registers above it.
	frame *linuxabi.Frame
	// state and tid are the caller's Linux state and thread ID, from the
	// per-task cache. A call of the fast class reads only what never changes
	// in them: the PID and the TID.
	state *linuxcompat.ProcessState
	tid   int32
}

// Read serves read and pread64.
func (handler *linuxCallHandler) Read(request linuxcompat.Request) linuxcompat.Result {
	if request.Number == linuxabi.SysPread64 {
		return handler.positioned(request, false)
	}
	return handler.read(request, false)
}

// Readv reads into a bounded vector: a stream's waiting bytes or one VFS
// read of a file, delivered to the elements in order.
func (handler *linuxCallHandler) Readv(request linuxcompat.Request) linuxcompat.Result {
	return handler.read(request, true)
}

// read sends a descriptor to what serves its kind: the VFS for a file, the
// kernel itself for a device, and the stream code below for everything else.
func (handler *linuxCallHandler) read(request linuxcompat.Request, vector bool) linuxcompat.Result {
	local := thisCPU()
	if description, found := linuxcompat.DescriptionOf(handler.root, request.Arguments[0]); found {
		switch description.Kind {
		case linuxcompat.FDVFSFile:
			return handler.readFile(request, description, vector)
		case linuxcompat.FDVFSDirectory:
			return linuxcompat.Result{Errno: linuxabi.EISDIR}
		case linuxcompat.FDSynthetic:
			return handler.deviceRead(request, description, vector)
		}
	}
	if errno := handler.consumeWake(request); errno == linuxabi.EINTR {
		return handler.interrupted(true)
	} else if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	endpoint, streamID, errno := handler.endpoint(int64(request.Arguments[0]), false)
	if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	limit, wire, errno := handler.readLimit(request, vector, stream.Capacity)
	if errno != 0 || limit == 0 {
		return linuxcompat.Result{Errno: errno}
	}
	// One call delivers what the stream holds now, up to the request. It
	// waits only while it has nothing to return.
	delivered := uint64(0)
	for retries := 0; delivered < limit && retries < 2; {
		count, status := endpoint.Read(local.linuxIOBuffer[:min64(limit-delivered, stream.AtomicWrite)])
		switch status {
		case stream.StatusOK:
			wakeStreamWaiters(streamID, 1, ipc.WaitMessage)
			if !handler.deliver(request, wire, delivered, local.linuxIOBuffer[:count], nil, uint64(count)) {
				return linuxcompat.Result{Errno: linuxabi.EFAULT}
			}
			delivered += uint64(count)
		case stream.StatusPeerClosed:
			return linuxcompat.Result{Value: delivered}
		case stream.StatusWouldBlock:
			if delivered != 0 {
				return linuxcompat.Result{Value: delivered}
			}
			blocked, retry := handler.blockStream(request, endpoint, streamID, 0, 0)
			if blocked.Action == linuxabi.ActionBlock || !retry {
				return blocked
			}
			retries++
		default:
			if delivered != 0 {
				return linuxcompat.Result{Value: delivered}
			}
			return linuxcompat.Result{Errno: linuxcompat.StreamReadErrno(status)}
		}
	}
	if delivered == 0 {
		return linuxcompat.Result{Errno: linuxabi.EIO}
	}
	return linuxcompat.Result{Value: delivered}
}

// readLimit checks where a read or readv will put its bytes and returns how
// many one call may deliver, at most bound, with the copied vector of a
// readv. The destination is checked before any byte leaves its source, so a
// bad pointer costs the caller no data.
func (handler *linuxCallHandler) readLimit(request linuxcompat.Request, vector bool, bound uint64) (uint64, []byte, linuxabi.Errno) {
	if !vector {
		address, limit := request.Arguments[1], request.Arguments[2]
		if !userRange(address, limit) {
			return 0, nil, linuxabi.EFAULT
		}
		if limit > bound {
			limit = bound
		}
		if limit == 0 {
			return 0, nil, 0
		}
		if limit = handler.accessible(address, limit, true); limit == 0 {
			return 0, nil, linuxabi.EFAULT
		}
		return limit, nil, 0
	}
	wire, errno := handler.copyIOVec(request)
	if errno != 0 {
		return 0, nil, errno
	}
	total := uint64(0)
	for index := 0; index < len(wire)/linuxabi.IOVecSize && total < bound; index++ {
		base, length := linuxcompat.IOVecEntry(wire, index)
		if length > bound-total {
			length = bound - total
		}
		if length == 0 {
			continue
		}
		writable := handler.accessible(base, length, true)
		total += writable
		if writable < length {
			// The read ends where the destination does.
			if total == 0 {
				return 0, nil, linuxabi.EFAULT
			}
			break
		}
	}
	return total, wire, 0
}

// userRange is the check Linux makes on a read or write before anything
// else: the whole requested range lies in the user half and does not wrap.
// It looks at the length the program asked for, so a length that runs past
// the user half is EFAULT even when the first bytes are mapped and the call
// would have moved only those.
func userRange(address uint64, length uint64) bool {
	end := address + length
	return end >= address && end <= vm.UserAddressLimit
}

// accessible returns how many of length bytes at address the process can
// read, or write when write is set, counted from the first byte to the first
// page it cannot use. Linux moves bytes up to a fault and reports what it
// moved, so a transfer that runs off the end of a mapping is short, and one
// that starts outside any is EFAULT. Probing a page faults it in through the
// ordinary demand-paging path.
func (handler *linuxCallHandler) accessible(address uint64, length uint64, write bool) uint64 {
	local := thisCPU()
	if length == 0 || !vm.ValidateUserRange(address, length) {
		return 0
	}
	reached := uint64(0)
	for reached < length {
		chunk := vm.PageSize - (address+reached)%vm.PageSize
		if chunk > length-reached {
			chunk = length - reached
		}
		if write && !vm.CanCopyOut(handler.root, address+reached, chunk) ||
			!write && !vm.CopyIn(handler.root, address+reached, local.linuxProbe[:]) {
			break
		}
		reached += chunk
	}
	return reached
}

// copyIOVec copies and validates the vector of a readv or writev.
func (handler *linuxCallHandler) copyIOVec(request linuxcompat.Request) ([]byte, linuxabi.Errno) {
	local := thisCPU()
	count := request.Arguments[2]
	if count > linuxcompat.MaxIOVec {
		return nil, linuxabi.EINVAL
	}
	wire := local.linuxIOVecWire[:count*linuxabi.IOVecSize]
	if count != 0 && !vm.CopyIn(handler.root, request.Arguments[1], wire) {
		return nil, linuxabi.EFAULT
	}
	if errno := linuxcompat.ValidateIOVec(wire, vm.UserAddressLimit); errno != 0 {
		return nil, errno
	}
	return wire, 0
}

// deliver copies length bytes of a read to the caller, starting at byte
// position of the destination: the address a read names, or the elements of
// a readv vector in order when wire is not nil. The bytes come from data, or
// from the I/O object at the same position when object is not nil.
func (handler *linuxCallHandler) deliver(request linuxcompat.Request, wire []byte, position uint64, data []byte, object *shmem.Object, length uint64) bool {
	for moved := uint64(0); moved < length; {
		address, room := request.Arguments[1]+position+moved, length-moved
		if wire != nil {
			piece, remaining, found := linuxcompat.VectorPiece(wire, position+moved)
			if !found {
				return false
			}
			address = piece
			if remaining < room {
				room = remaining
			}
		}
		if object != nil {
			if !handler.ioTransfer(object, position+moved, address, room, true) {
				return false
			}
		} else if !vm.CopyOut(handler.root, address, data[moved:moved+room]) {
			return false
		}
		moved += room
	}
	return true
}

// Write serves write and pwrite64.
func (handler *linuxCallHandler) Write(request linuxcompat.Request) linuxcompat.Result {
	if request.Number == linuxabi.SysPwrite64 {
		return handler.positioned(request, true)
	}
	if description, found := linuxcompat.DescriptionOf(handler.root, request.Arguments[0]); found {
		switch description.Kind {
		case linuxcompat.FDVFSFile:
			return handler.writeFile(request, description, false)
		case linuxcompat.FDVFSDirectory:
			return linuxcompat.Result{Errno: linuxabi.EBADF}
		case linuxcompat.FDSynthetic:
			return handler.deviceWrite(request, *description, false)
		}
	}
	if errno := handler.consumeWake(request); errno == linuxabi.EINTR {
		return handler.interrupted(true)
	} else if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	endpoint, streamID, errno := handler.endpoint(int64(request.Arguments[0]), true)
	if errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	address, requested := request.Arguments[1], request.Arguments[2]
	if !userRange(address, requested) {
		return linuxcompat.Result{Errno: linuxabi.EFAULT}
	}
	if requested == 0 {
		return linuxcompat.Result{}
	}
	limit := requested
	if limit > stream.Capacity {
		limit = stream.Capacity
	}
	if limit = handler.accessible(address, limit, false); limit == 0 {
		return linuxcompat.Result{Errno: linuxabi.EFAULT}
	}
	if limit == requested {
		if result, ready := handler.awaitWholeWrite(request, endpoint, streamID, limit); !ready {
			return result
		}
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
	local := thisCPU()
	written := uint64(0)
	for written < limit {
		chunk := min64(limit-written, stream.AtomicWrite)
		if !vm.CopyIn(handler.root, address+written, local.linuxIOBuffer[:chunk]) {
			if written != 0 || progressed {
				return written, linuxcompat.Result{}, false
			}
			return 0, linuxcompat.Result{Errno: linuxabi.EFAULT}, true
		}
		count, status := endpoint.Write(local.linuxIOBuffer[:chunk])
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
	local := thisCPU()
	if description, found := linuxcompat.DescriptionOf(handler.root, request.Arguments[0]); found {
		switch description.Kind {
		case linuxcompat.FDVFSFile:
			return handler.writeFile(request, description, true)
		case linuxcompat.FDVFSDirectory:
			return linuxcompat.Result{Errno: linuxabi.EBADF}
		case linuxcompat.FDSynthetic:
			return handler.deviceWrite(request, *description, true)
		}
	}
	if errno := handler.consumeWake(request); errno == linuxabi.EINTR {
		return handler.interrupted(true)
	} else if errno != 0 {
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
	wire := local.linuxIOVecWire[:count*linuxabi.IOVecSize]
	if !vm.CopyIn(handler.root, request.Arguments[1], wire) {
		return linuxcompat.Result{Errno: linuxabi.EFAULT}
	}
	if errno := linuxcompat.ValidateIOVec(wire, vm.UserAddressLimit); errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	// A vector of at most PIPE_BUF readable bytes is one write and lands
	// whole, as a single write of that size does.
	whole := uint64(0)
	for index := uint64(0); index < count && whole <= linuxcompat.LinuxPipeBuf; index++ {
		base, length := linuxcompat.IOVecEntry(wire, int(index))
		if length > linuxcompat.LinuxPipeBuf-whole || handler.accessible(base, length, false) != length {
			whole = linuxcompat.LinuxPipeBuf + 1
			break
		}
		whole += length
	}
	if result, ready := handler.awaitWholeWrite(request, endpoint, streamID, whole); !ready {
		return result
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
		readable := handler.accessible(base, length, false)
		if readable == 0 {
			if total == 0 {
				return linuxcompat.Result{Errno: linuxabi.EFAULT}
			}
			break
		}
		written, result, final := handler.writeRange(request, endpoint, streamID, base, readable, total != 0)
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
	if thread, isThread := linuxcompat.FindThread(handler.task); isThread {
		thread.ClearChildTID = request.Arguments[0]
		return linuxcompat.Result{Value: uint64(thread.TID)}
	}
	state.ClearChildTID = request.Arguments[0]
	return linuxcompat.Result{Value: uint64(state.PID)}
}

func (handler *linuxCallHandler) Exit(request linuxcompat.Request, group bool) linuxcompat.Result {
	code := uint64(uint8(request.Arguments[0]))
	linuxcompat.SyscallContinuations.Cancel(handler.task)
	if group {
		if owner, found := handler.self(); found {
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
	wantKind := linuxcompat.FDStreamReader
	if writer {
		wantKind = linuxcompat.FDStreamWriter
	}
	if !found || description.Kind != wantKind {
		return nil, 0, linuxabi.EBADF
	}
	streamID, isWriter, valid := stream.DecodeEndpointObject(description.Object)
	endpoint, exists := streamRegistry.Lookup(streamID)
	if !valid || !exists || isWriter != writer {
		return nil, 0, linuxabi.EBADF
	}
	return endpoint, streamID, 0
}

// consumeWake ends the stream wait a retried call was woken from. EINTR means
// a signal ended the wait before any byte moved.
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
	if state.Wake == linuxcompat.WakeInterrupted {
		return linuxabi.EINTR
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
