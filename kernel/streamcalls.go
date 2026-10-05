//go:build uefi

package main

// Byte-stream native calls and their waiters (ADR 0040).

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/stream"
	kernelwait "goos/kernel/wait"
)

// releaseStreamSlot does the same for a stream. A stream outlives the handles
// its creator held, because both endpoints may be transferred, so the charge is
// returned when the object is gone rather than when a handle closes.
func releaseStreamSlot(streamID uint64) {
	if owner, released := streamRegistry.Release(streamID); released && owner != 0 {
		processRegistry.ReleaseProcess(process.ID(owner), process.ResourceStreams, 1)
	}
}

// dropStreamCapability forgets the reference of a stream handle that is being
// discarded, as process teardown does for every handle it visits.
func dropStreamCapability(cap capability.Capability) bool {
	streamID, writer, closed, ok := streamRegistry.Drop(cap.Object())
	if ok {
		finishStreamClosure(stream.Closure{Stream: streamID, Writer: writer, Closed: closed})
	}
	return ok
}

// finishStreamClosure acts on an end whose last handle has gone: the peer's
// waiters observe the closure and the stream is freed once both ends are.
func finishStreamClosure(closure stream.Closure) {
	if !closure.Closed {
		return
	}
	side := uint8(1)
	if closure.Writer {
		side = 0
	}
	wakeStreamWaiters(closure.Stream, side, ipc.WaitPeerDied)
	releaseStreamSlot(closure.Stream)
}

func (handler nativeCallHandler) StreamCreate(address uint64, length uint64) kcall.Status {
	local := thisCPU()
	owner, found := processRegistry.ByRoot(handler.root)
	if !found || length != 16 || owner.Handles().Available() < 2 {
		return kcall.StatusPermissionDenied
	}
	// Any process may create a stream, so the quota is what stops one of them
	// draining the registry and breaking every later pipeline. It is charged
	// before a slot is taken, so a refusal leaves nothing to unwind.
	if !owner.Charge(process.ResourceStreams, 1) {
		return kcall.StatusResourceExhausted
	}
	endpoint, id, created := streamRegistry.Create(uint64(owner.ID()))
	if !created {
		owner.Release(process.ResourceStreams, 1)
		return kcall.StatusResourceExhausted
	}
	readerObject, _ := stream.ReaderObject(id)
	writerObject, _ := stream.WriterObject(id)
	reader, readerOK := owner.Handles().Insert(handle.TypeStream, capability.New(readerObject, capability.Read|capability.Wait|capability.Transfer))
	writer, writerOK := owner.Handles().Insert(handle.TypeStream, capability.New(writerObject, capability.Write|capability.Wait|capability.Transfer))
	if !readerOK || !writerOK {
		if readerOK {
			owner.Handles().Close(reader)
		}
		if writerOK {
			owner.Handles().Close(writer)
		}
		endpoint.CloseReader()
		endpoint.CloseWriter()
		releaseStreamSlot(id)
		return kcall.StatusResourceExhausted
	}
	putUint64(local.streamCreateWire[0:8], uint64(reader))
	putUint64(local.streamCreateWire[8:16], uint64(writer))
	if !vm.CopyOut(handler.root, address, local.streamCreateWire[:]) {
		owner.Handles().Close(reader)
		owner.Handles().Close(writer)
		endpoint.CloseReader()
		endpoint.CloseWriter()
		releaseStreamSlot(id)
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) streamEndpoint(id uint64, required capability.Rights, writer bool) (*stream.Stream, kcall.Status) {
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return nil, kcall.StatusPermissionDenied
	}
	cap, allowed := owner.Handles().Lookup(handle.ID(id), handle.TypeStream, required)
	streamID, isWriter, valid := stream.DecodeEndpointObject(cap.Object())
	endpoint, exists := streamRegistry.Lookup(streamID)
	if !allowed || !valid || !exists || isWriter != writer {
		return nil, kcall.StatusPermissionDenied
	}
	return endpoint, kcall.StatusOK
}

func streamStatus(status stream.Status) kcall.Status {
	switch status {
	case stream.StatusOK:
		return kcall.StatusOK
	case stream.StatusWouldBlock:
		return kcall.StatusResourceExhausted
	case stream.StatusPeerClosed:
		return kcall.StatusPeerDied
	default:
		return kcall.StatusInvalidArgument
	}
}

func streamDescriptor(root, address, length uint64) (uint64, uint64, bool) {
	local := thisCPU()
	if length != kcall.StreamDescriptorSize {
		return 0, 0, false
	}
	if !vm.CopyIn(root, address, local.streamDescriptorWire[:]) {
		return 0, 0, false
	}
	buffer, size := decodeUint64(local.streamDescriptorWire[0:8]), decodeUint64(local.streamDescriptorWire[8:16])
	if size == 0 || size > kcall.MaxStreamIO || decodeUint64(local.streamDescriptorWire[16:24]) != 0 {
		return 0, 0, false
	}
	return buffer, size, true
}

func writeStreamResult(root, descriptor, size uint64) bool {
	local := thisCPU()
	putUint64(local.streamResultWire[:], size)
	return vm.CopyOut(root, descriptor+16, local.streamResultWire[:])
}

func (handler nativeCallHandler) StreamRead(id uint64, descriptor uint64, length uint64) kcall.Status {
	local := thisCPU()
	endpoint, status := handler.streamEndpoint(id, capability.Read, false)
	buffer, size, valid := streamDescriptor(handler.root, descriptor, length)
	if status != kcall.StatusOK || !valid {
		if status != kcall.StatusOK {
			return status
		}
		return kcall.StatusInvalidArgument
	}
	if !vm.CanCopyOut(handler.root, buffer, size) || !vm.CanCopyOut(handler.root, descriptor+16, 8) {
		return kcall.StatusInvalidArgument
	}
	count, result := endpoint.Read(local.streamCallData[:size])
	if result != stream.StatusOK {
		return streamStatus(result)
	}
	if !vm.CopyOut(handler.root, buffer, local.streamCallData[:count]) || !writeStreamResult(handler.root, descriptor, uint64(count)) {
		return kcall.StatusInvalidArgument
	}
	// Writers wait for room for a whole atomic write, so they are woken only
	// once there is that much; waking them for less would have them refused
	// and wait again.
	if endpoint.WriteStatus() != stream.StatusWouldBlock {
		cap, _ := processRegistry.ByRoot(handler.root)
		streamCap, _ := cap.Handles().Lookup(handle.ID(id), handle.TypeStream, capability.Read)
		streamID, _, _ := stream.DecodeEndpointObject(streamCap.Object())
		wakeStreamWaiters(streamID, 1, ipc.WaitMessage)
	}
	return kcall.StatusOK
}

func (handler nativeCallHandler) StreamWrite(id uint64, descriptor uint64, length uint64) kcall.Status {
	local := thisCPU()
	endpoint, status := handler.streamEndpoint(id, capability.Write, true)
	buffer, size, valid := streamDescriptor(handler.root, descriptor, length)
	if status != kcall.StatusOK || !valid {
		if status != kcall.StatusOK {
			return status
		}
		return kcall.StatusInvalidArgument
	}
	if !vm.CopyIn(handler.root, buffer, local.streamCallData[:size]) {
		return kcall.StatusInvalidArgument
	}
	count, result := endpoint.Write(local.streamCallData[:size])
	if result != stream.StatusOK {
		return streamStatus(result)
	}
	if !writeStreamResult(handler.root, descriptor, uint64(count)) {
		return kcall.StatusInvalidArgument
	}
	owner, _ := processRegistry.ByRoot(handler.root)
	streamCap, _ := owner.Handles().Lookup(handle.ID(id), handle.TypeStream, capability.Write)
	streamID, _, _ := stream.DecodeEndpointObject(streamCap.Object())
	wakeStreamWaiters(streamID, 0, ipc.WaitMessage)
	return kcall.StatusOK
}

func wakeStreamWaiters(streamID uint64, side uint8, reason ipc.WaitReason) {
	local := thisCPU()
	event := kernelwait.Readable
	if side == 1 {
		event = kernelwait.Writable
	}
	if reason == ipc.WaitPeerDied {
		event = kernelwait.PeerClosed
	}
	publishWaitAny(kernelwait.Key{Kind: kernelwait.KindStream, Object: streamID, Side: side}, event)
	count := streamWaitSet.Wake(streamID, side, reason, local.streamWakeResults[:])
	for index := 0; index < count; index++ {
		if state, found := linuxcompat.SyscallContinuations.Pending(local.streamWakeResults[index].Task); found {
			wake := linuxcompat.WakeMessage
			if reason == ipc.WaitPeerDied {
				wake = linuxcompat.WakePeerDied
			}
			if state.Correlation != streamID || !resumeLinuxSyscall(local.streamWakeResults[index].Task, streamID, wake) {
				cancelTaskWaits(local.streamWakeResults[index].Task)
				amd64.TerminateTask(uint64(local.streamWakeResults[index].Task), uint64(1)<<63|5)
			}
			local.streamWakeResults[index] = ipc.WaitResult{}
			continue
		}
		task, _, valid := local.streamWakeResults[index].Task.Parts()
		if !valid {
			local.streamWakeResults[index] = ipc.WaitResult{}
			continue
		}
		if reason == ipc.WaitPeerDied {
			streamWaitStatus[task] = kcall.StatusPeerDied
		}
		if !amd64.WakeTaskID(uint64(local.streamWakeResults[index].Task)) {
			panicSerial("stale stream wake task")
		}
		local.streamWakeResults[index] = ipc.WaitResult{}
	}
}

func expireStreamWaiters() {
	local := thisCPU()
	count := streamWaitSet.Expire(amd64.TimerTicks(), local.streamWakeResults[:])
	for index := 0; index < count; index++ {
		task, _, valid := local.streamWakeResults[index].Task.Parts()
		if !valid {
			local.streamWakeResults[index] = ipc.WaitResult{}
			continue
		}
		streamWaitStatus[task] = kcall.StatusTimeout
		if !amd64.WakeTaskID(uint64(local.streamWakeResults[index].Task)) {
			panicSerial("stale stream timeout task")
		}
		local.streamWakeResults[index] = ipc.WaitResult{}
	}
}

func waitStream(handler nativeCallHandler, id, timeout, task uint64) (kcall.Status, bool) {
	if task >= sched.MaxTasks || timeout == 0 {
		return kcall.StatusInvalidArgument, false
	}
	taskID := sched.TaskID(amd64.TaskIDForSlot(task))
	if _, _, valid := taskID.Parts(); !valid {
		return kcall.StatusInvalidArgument, false
	}
	if streamWaitStatus[task] != kcall.StatusOK {
		status := streamWaitStatus[task]
		streamWaitStatus[task] = kcall.StatusOK
		return status, false
	}
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied, false
	}
	cap, allowed := owner.Handles().Lookup(handle.ID(id), handle.TypeStream, capability.Wait)
	streamID, writer, valid := stream.DecodeEndpointObject(cap.Object())
	endpoint, exists := streamRegistry.Lookup(streamID)
	if !allowed || !valid || !exists {
		return kcall.StatusPermissionDenied, false
	}
	status := endpoint.ReadStatus()
	side := uint8(0)
	if writer {
		status, side = endpoint.WriteStatus(), 1
	}
	if status != stream.StatusWouldBlock {
		return streamStatus(status), false
	}
	now := amd64.TimerTicks()
	deadline := now + timeout
	if deadline < now || !streamWaitSet.Add(taskID, streamID, side, deadline) {
		return kcall.StatusResourceExhausted, false
	}
	// Re-check after registering: data, buffer space, or peer closure arriving
	// in between would wake nobody and strand this task until its deadline.
	if writer {
		status = endpoint.WriteStatus()
	} else {
		status = endpoint.ReadStatus()
	}
	if status != stream.StatusWouldBlock {
		streamWaitSet.Cancel(taskID)
		streamWaitStatus[task] = kcall.StatusOK
		return streamStatus(status), false
	}
	return kcall.StatusNotFound, true
}

//export goos_dispatch_stream_wait
func dispatchStreamWait(id, timeout, rootAndTask uint64) uint64 {
	status, blocked := waitStream(nativeCallHandler{root: rootAndTask &^ 0xfff}, id, timeout, rootAndTask&0xfff)
	action := kcall.ActionReturn
	if blocked {
		action = kcall.ActionBlock
	}
	return uint64(action)<<32 | uint64(status)
}
