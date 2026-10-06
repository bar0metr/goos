//go:build uefi

package main

// Channel native calls: creation, handle close, send, receive, and the
// waiters blocked on a channel (ADR 0014, 0015).

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	"goos/kernel/diag"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
	kernelwait "goos/kernel/wait"
)

// bootChannel creates a channel charged to PID 1. The boot endpoints belong to
// PID 1 exactly as the ones it later creates do, so they are accounted the same
// way and the charge comes back when they are released.
func bootChannel() (*ipc.Channel, uint64, bool) {
	if bootInitProcess == nil || !bootInitProcess.Charge(process.ResourceChannels, 1) {
		return nil, 0, false
	}
	channel, id, created := channelRegistry.Create(1)
	if !created {
		bootInitProcess.Release(process.ResourceChannels, 1)
	}
	return channel, id, created
}

// closeChannelSide closes one side of a channel and reports whether this
// call closed it. Closing the second side frees the channel, and the quota
// charge goes back to whichever process created it, which has often already
// exited.
func closeChannelSide(channelID uint64, side uint8) bool {
	closed, owner, released := channelRegistry.Close(channelID, side)
	if released && owner != 0 {
		processRegistry.ReleaseProcess(process.ID(owner), process.ResourceChannels, 1)
	}
	return closed
}

// channelSend queues message on a channel under the channel's lock. A
// channel that is gone reports its peer dead.
func channelSend(channelID uint64, side uint8, message *ipc.Message, source *handle.Table, target *handle.Table, transfers []ipc.Transfer) ipc.Status {
	channel, live := channelRegistry.Acquire(channelID)
	if !live {
		return ipc.StatusPeerDied
	}
	status := channel.SendFrom(side, message, source, target, transfers)
	channel.Unlock()
	return status
}

// channelReceive takes the next message from a channel into out under the
// channel's lock. A channel that is gone reports its peer dead.
func channelReceive(channelID uint64, side uint8, out *ipc.Message) ipc.Status {
	channel, live := channelRegistry.Acquire(channelID)
	if !live {
		*out = ipc.Message{}
		return ipc.StatusPeerDied
	}
	status := channel.ReceiveInto(side, out)
	channel.Unlock()
	return status
}

// channelSideOpen reports whether side of the channel is open.
func channelSideOpen(channelID uint64, side uint8) bool {
	channel, live := channelRegistry.Acquire(channelID)
	if !live {
		return false
	}
	open := channel.Alive(side)
	channel.Unlock()
	return open
}

func (handler nativeCallHandler) ChannelCreate(address uint64, length uint64) kcall.Status {
	local := thisCPU()
	owner, found := handler.self()
	if !found || owner.ID() != 1 || length != 16 || owner.Handles().Available() < 2 {
		return kcall.StatusPermissionDenied
	}
	if !owner.Charge(process.ResourceChannels, 1) {
		return kcall.StatusResourceExhausted
	}
	_, id, ok := channelRegistry.Create(uint64(owner.ID()))
	if !ok {
		owner.Release(process.ResourceChannels, 1)
		return kcall.StatusResourceExhausted
	}
	object0, _ := ipc.EndpointObject(id, 0)
	object1, _ := ipc.EndpointObject(id, 1)
	rights := capability.Read | capability.Write | capability.Wait | capability.Transfer
	handle0, ok0 := owner.Handles().Insert(handle.TypeChannel, capability.New(object0, rights))
	handle1, ok1 := owner.Handles().Insert(handle.TypeChannel, capability.New(object1, rights))
	if !ok0 || !ok1 {
		if ok0 {
			owner.Handles().Close(handle0)
		}
		if ok1 {
			owner.Handles().Close(handle1)
		}
		closeChannelSide(id, 0)
		closeChannelSide(id, 1)
		return kcall.StatusResourceExhausted
	}
	putUint64(local.channelWire[0:8], uint64(handle0))
	putUint64(local.channelWire[8:16], uint64(handle1))
	if !vm.CopyOut(handler.root, address, local.channelWire[:]) {
		owner.Handles().Close(handle0)
		owner.Handles().Close(handle1)
		closeChannelSide(id, 0)
		closeChannelSide(id, 1)
		return kcall.StatusInvalidArgument
	}
	trace(diag.ChannelCreated, owner.ID(), 0, id, uint64(handle1))
	return kcall.StatusOK
}

func (handler nativeCallHandler) HandleClose(id uint64) kcall.Status {
	owner, found := handler.self()
	if !found {
		return kcall.StatusPermissionDenied
	}
	typeID, cap, found := owner.Handles().Describe(handle.ID(id))
	if !found {
		return kcall.StatusNotFound
	}
	if typeID == handle.TypeChannel {
		channelID, side, valid := ipc.DecodeEndpointObject(cap.Object())
		if !valid || !closeChannelSide(channelID, side) {
			return kcall.StatusNotFound
		}
		trace(diag.ChannelClosed, owner.ID(), id, channelID|uint64(side)<<63, diag.CloseByHandle)
		wakeIPCWaiters(channelID, side^1, ipc.WaitPeerDied)
	} else if typeID == handle.TypeStream {
		// Closing a stream handle forgets its reference; the end itself closes
		// only with its last handle (ADR 0089).
		closure, closed := streamRegistry.CloseHandle(owner.Handles(), handle.ID(id))
		if !closed {
			return kcall.StatusNotFound
		}
		finishStreamClosure(closure)
		return kcall.StatusOK
	} else if typeID == handle.TypeMemory {
		// A memory handle is a reference to its object, which goes with the
		// last reference (ADR 0106, ADR 0107).
		if !owner.Handles().Close(handle.ID(id)) {
			return kcall.StatusNotFound
		}
		releaseMemory(cap)
		return kcall.StatusOK
	}
	if !owner.Handles().Close(handle.ID(id)) {
		return kcall.StatusNotFound
	}
	return kcall.StatusOK
}

// closeMovedHandle closes a handle whose authority has moved to another table
// — an IPC attachment or a consumed launch grant. A channel side lives on in
// its new holder, so only the handle goes; a stream handle also gives up its
// reference, which the new holder's copy has already taken.
func closeMovedHandle(table *handle.Table, id handle.ID) {
	if typeID, _, found := table.Describe(id); found && typeID == handle.TypeStream {
		if closure, closed := streamRegistry.CloseHandle(table, id); closed {
			finishStreamClosure(closure)
		}
		return
	}
	table.Close(id)
}

// endpoint resolves a channel handle of the calling process to the channel's
// ID and side, the caller, and the process holding the other side. The call
// takes the channel's lock itself when it touches the channel; a channel
// closed in between is not found then.
func (handler nativeCallHandler) endpoint(channel uint64, required capability.Rights) (uint64, uint8, *process.Process, *process.Process, kcall.Status) {
	owner, found := handler.self()
	if !found {
		return 0, 0, nil, nil, kcall.StatusPermissionDenied
	}
	cap, ok := owner.Handles().Lookup(handle.ID(channel), handle.TypeChannel, required)
	if !ok {
		return 0, 0, nil, nil, kcall.StatusPermissionDenied
	}
	channelID, side, valid := ipc.DecodeEndpointObject(cap.Object())
	if !valid {
		return 0, 0, nil, nil, kcall.StatusNotFound
	}
	peerObject, _ := ipc.EndpointObject(channelID, side^1)
	peer, _ := processRegistry.EndpointHolder(peerObject)
	return channelID, side, owner, peer, kcall.StatusOK
}

func (handler nativeCallHandler) IPCSend(channel uint64, address uint64, length uint64) kcall.Status {
	local := thisCPU()
	channelID, side, owner, peer, status := handler.endpoint(channel, capability.Write)
	if status != kcall.StatusOK {
		handler.recordInitSend(status)
		return status
	}
	if length < ipc.WireHeaderSize || length > ipc.WireMaxSize {
		handler.recordInitSend(kcall.StatusInvalidArgument)
		return kcall.StatusInvalidArgument
	}
	data := local.ipcWireData[:int(length)]
	if !vm.CopyIn(handler.root, address, data) {
		handler.recordInitSend(kcall.StatusInvalidArgument)
		return kcall.StatusInvalidArgument
	}
	message, ok := ipc.Decode(data)
	if !ok {
		handler.recordInitSend(kcall.StatusCorruptData)
		return kcall.StatusCorruptData
	}
	var peerHandles *handle.Table
	if peer != nil {
		peerHandles = peer.Handles()
	}
	var transfers [ipc.MaxAttachedHandles]ipc.Transfer
	var transferObjects [ipc.MaxAttachedHandles]uint64
	transferCount := int(message.HandleCount)
	if transferCount != 0 && peerHandles == nil {
		return kcall.StatusPeerDied
	}
	movesStream := false
	for index := 0; index < transferCount; index++ {
		typeID, attached, found := owner.Handles().Describe(message.Handles[index])
		if !found || !attached.Allows(capability.Transfer) {
			return kcall.StatusPermissionDenied
		}
		rights := attached.Rights() &^ capability.Transfer
		if rights == 0 {
			return kcall.StatusPermissionDenied
		}
		movesStream = movesStream || typeID == handle.TypeStream
		transfers[index] = ipc.Transfer{Handle: message.Handles[index], Type: typeID, Rights: rights}
		transferObjects[index] = attached.Object()
	}
	if transferCount != 0 && !wantTransferLocks(peer, movesStream) {
		return kcall.StatusOK
	}
	for index := 0; index < transferCount; index++ {
		// The receiver's copy of a stream endpoint is counted after the send,
		// so the end must have room for it now: a send that cannot be
		// accounted is refused before anything moves.
		if transfers[index].Type == handle.TypeStream && !streamRegistry.CanRetain(transferObjects[index], ipc.MaxAttachedHandles) {
			return kcall.StatusResourceExhausted
		}
	}
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	if transferCount == 0 {
		// Only a message that moves handles reaches the peer's table.
		peerHandles = nil
	}
	endpoint, live := channelRegistry.Acquire(channelID)
	if !live {
		handler.recordInitSend(kcall.StatusNotFound)
		return kcall.StatusNotFound
	}
	status = endpoint.SendFrom(side, &message, owner.Handles(), peerHandles, transfers[:transferCount])
	endpoint.Unlock()
	if status == ipc.StatusOK {
		// A receiver that registered before the send is woken now; one that
		// registers after it finds the message when it looks again.
		if !completeExecutablePage(channelID, side^1) {
			wakeIPCWaiters(channelID, side^1, ipc.WaitMessage)
		}
		for index := 0; index < transferCount; index++ {
			if diag.Enabled && peer != nil {
				trace(diag.HandleTransferred, owner.ID(), uint64(transfers[index].Handle), channel, uint64(peer.ID()))
			}
			// A transfer is a move. The receiver's copy is counted before the
			// sender's handle goes, so a stream end never closes in between,
			// and a handle attached twice is two copies and one close. The
			// check above guarantees the count has room.
			if transfers[index].Type == handle.TypeStream && !streamRegistry.Placed(transfers[index].Type, transferObjects[index]) {
				panicSerial("stream transfer count refused after check")
			}
			closeMovedHandle(owner.Handles(), transfers[index].Handle)
		}
	}
	handler.recordInitSend(status)
	return status
}

// wantTransferLocks reports whether the native call holds peer's handle
// table, and the stream lock when withStreams is set. When it does not, it
// asks for them: the call has changed nothing yet, and the dispatcher runs it
// again holding them.
func wantTransferLocks(peer *process.Process, withStreams bool) bool {
	local := thisCPU()
	held := &local.nativeCall.held
	if held.holdsPeer(peer) && (!withStreams || held.taken&streams != 0) {
		return true
	}
	local.peerWanted, local.streamsWanted = peer.ID(), withStreams
	return false
}

func (handler nativeCallHandler) recordInitSend(status kcall.Status) {
	if bootInitProcess != nil && handler.root == bootInitProcess.AddressSpace().Root {
		initSendStatus = status
		initSendObserved = 1
	}
}

func waitReceive(handler nativeCallHandler, channel uint64, descriptor uint64, size uint64, task uint64) (kcall.Status, bool) {
	local := thisCPU()
	if size != 24 || task >= sched.MaxTasks {
		return kcall.StatusInvalidArgument, false
	}
	taskID := sched.TaskID(amd64.TaskIDForSlot(task))
	if _, _, valid := taskID.Parts(); !valid {
		return kcall.StatusInvalidArgument, false
	}
	if status := ipcWaitStatus.take(task); status != kcall.StatusOK {
		return status, false
	}
	if !vm.CopyIn(handler.root, descriptor, local.receiveDescriptorWire[:]) {
		return kcall.StatusInvalidArgument, false
	}
	buffer := decodeUint64(local.receiveDescriptorWire[0:8])
	capacity := decodeUint64(local.receiveDescriptorWire[8:16])
	timeout := decodeUint64(local.receiveDescriptorWire[16:24])
	status := handler.IPCReceive(channel, buffer, capacity)
	if status != kcall.StatusNotFound {
		return status, false
	}
	if timeout == 0 {
		return kcall.StatusTimeout, false
	}
	channelID, side, _, _, endpointStatus := handler.endpoint(channel, capability.Read|capability.Wait)
	if endpointStatus != kcall.StatusOK {
		return endpointStatus, false
	}
	if !channelRegistry.Live(channelID) {
		return kcall.StatusNotFound, false
	}
	now := amd64.TimerTicks()
	deadline := now + timeout
	if deadline < now || !ipcWaitSet.Add(taskID, channelID, side, deadline) {
		return kcall.StatusResourceExhausted, false
	}
	// Re-check after registering, because a message sent between the receive
	// above and the registration would wake nobody.
	if status := handler.IPCReceive(channel, buffer, capacity); status != kcall.StatusNotFound {
		ipcWaitSet.Cancel(taskID)
		// A waker may already have removed the registration and stored a resume
		// status. It must not surface on the next wait, which this call answers.
		ipcWaitStatus.set(uint64(task), kcall.StatusOK)
		return status, false
	}
	return kcall.StatusNotFound, true
}

func (handler nativeCallHandler) IPCReceive(channel uint64, address uint64, capacity uint64) kcall.Status {
	local := thisCPU()
	channelID, side, _, _, status := handler.endpoint(channel, capability.Read)
	if status != kcall.StatusOK {
		return status
	}
	if capacity < ipc.WireMaxSize {
		return kcall.StatusInvalidArgument
	}
	endpoint, live := channelRegistry.Acquire(channelID)
	if !live {
		return kcall.StatusNotFound
	}
	var message ipc.Message
	status = endpoint.ReceiveInto(side, &message)
	endpoint.Unlock()
	if status != ipc.StatusOK {
		return status
	}
	size, ok := ipc.Encode(message, local.ipcWireData[:])
	if !ok || uint64(size) > capacity {
		return kcall.StatusResourceExhausted
	}
	if !vm.CopyOut(handler.root, address, local.ipcWireData[:size]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func wakeIPCWaiters(channel uint64, side uint8, reason ipc.WaitReason) {
	local := thisCPU()
	if channel == lifecycleChannelID && side == 0 {
		amd64.WakeTask(0)
	}
	event := kernelwait.Readable
	if reason == ipc.WaitPeerDied {
		event = kernelwait.PeerClosed
	}
	publishWaitAny(kernelwait.Key{Kind: kernelwait.KindChannel, Object: channel, Side: side}, event)
	count := ipcWaitSet.Wake(channel, side, reason, local.ipcWakeResults[:])
	for index := 0; index < count; index++ {
		task, _, valid := local.ipcWakeResults[index].Task.Parts()
		if !valid {
			local.ipcWakeResults[index] = ipc.WaitResult{}
			continue
		}
		linuxReason := linuxcompat.WakeMessage
		if reason == ipc.WaitPeerDied {
			linuxReason = linuxcompat.WakePeerDied
		}
		if resumeLinuxIPCWaiter(local.ipcWakeResults[index].Task, linuxReason) {
			local.ipcWakeResults[index] = ipc.WaitResult{}
			continue
		}
		if reason == ipc.WaitPeerDied {
			ipcWaitStatus.set(uint64(task), kcall.StatusPeerDied)
		}
		// A task another processor killed after it registered fails the
		// wake; its exit cancels what it left registered.
		amd64.WakeTaskID(uint64(local.ipcWakeResults[index].Task))
		local.ipcWakeResults[index] = ipc.WaitResult{}
	}
}

func expireIPCWaiters() {
	local := thisCPU()
	count := ipcWaitSet.Expire(amd64.TimerTicks(), local.ipcWakeResults[:])
	for index := 0; index < count; index++ {
		task, _, valid := local.ipcWakeResults[index].Task.Parts()
		if !valid {
			local.ipcWakeResults[index] = ipc.WaitResult{}
			continue
		}
		if resumeLinuxIPCWaiter(local.ipcWakeResults[index].Task, linuxcompat.WakeTimeout) {
			local.ipcWakeResults[index] = ipc.WaitResult{}
			continue
		}
		ipcWaitStatus.set(uint64(task), kcall.StatusTimeout)
		// A task another processor killed after it registered fails the
		// wake; its exit cancels what it left registered.
		amd64.WakeTaskID(uint64(local.ipcWakeResults[index].Task))
		local.ipcWakeResults[index] = ipc.WaitResult{}
	}
}

//export goos_dispatch_ipc_wait
func dispatchIPCWait(channel uint64, descriptor uint64, size uint64, rootAndTask uint64) uint64 {
	call, ok := beginNativeCall(kcall.OperationIPCWaitReceive, 0, 0)
	if !ok {
		return uint64(kcall.ActionRetry) << 32
	}
	status, blocked := waitReceive(nativeCallHandler{root: rootAndTask &^ 0xfff}, channel, descriptor, size, rootAndTask&0xfff)
	action := kcall.ActionReturn
	if blocked {
		action = kcall.ActionBlock
	}
	return call.end(uint64(action)<<32 | uint64(status))
}
