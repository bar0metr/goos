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
	"goos/kernel/stream"
)

type linuxCallHandler struct {
	root uint64
	task sched.TaskID
}

var linuxHandler linuxCallHandler
var linuxIOBuffer [stream.AtomicWrite]byte

func (handler *linuxCallHandler) Read(request linuxcompat.Request) linuxcompat.Result {
	if errno := handler.consumeWake(request); errno != 0 {
		return linuxcompat.Result{Errno: errno}
	}
	endpoint, streamID, errno := handler.endpoint(int64(request.Arguments[0]), false)
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
	if !vm.CanCopyOut(handler.root, address, limit) {
		return linuxcompat.Result{Errno: linuxabi.EFAULT}
	}
	for attempt := 0; attempt < 2; attempt++ {
		count, status := endpoint.Read(linuxIOBuffer[:min64(limit, stream.AtomicWrite)])
		switch status {
		case stream.StatusOK:
			if !vm.CopyOut(handler.root, address, linuxIOBuffer[:count]) {
				return linuxcompat.Result{Errno: linuxabi.EFAULT}
			}
			wakeStreamWaiters(streamID, 1, ipc.WaitMessage)
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

func (handler *linuxCallHandler) Write(request linuxcompat.Request) linuxcompat.Result {
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
	written := uint64(0)
	for written < limit {
		chunk := min64(limit-written, stream.AtomicWrite)
		if !vm.CopyIn(handler.root, address+written, linuxIOBuffer[:chunk]) {
			if written != 0 {
				return linuxcompat.Result{Value: written}
			}
			return linuxcompat.Result{Errno: linuxabi.EFAULT}
		}
		count, status := endpoint.Write(linuxIOBuffer[:chunk])
		if status == stream.StatusOK {
			written += uint64(count)
			wakeStreamWaiters(streamID, 0, ipc.WaitMessage)
			continue
		}
		if written != 0 {
			return linuxcompat.Result{Value: written}
		}
		if status == stream.StatusWouldBlock {
			blocked, retry := handler.blockStream(request, endpoint, streamID, 1, int(chunk))
			if blocked.Action == linuxabi.ActionBlock || !retry {
				return blocked
			}
			continue
		}
		return linuxcompat.Result{Errno: linuxcompat.StreamWriteErrno(status)}
	}
	return linuxcompat.Result{Value: written}
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
	state, found := linuxcompat.SyscallContinuations.Lookup(handler.task)
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
