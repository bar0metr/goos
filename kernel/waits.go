//go:build uefi

package main

// Blocking waits on processes, IRQs, and several objects at once, and the
// deadlines that end them (ADR 0050, 0051).

import (
	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	"goos/kernel/deadline"
	"goos/kernel/device"
	"goos/kernel/diag"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/nativethread"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/stream"
	kernelwait "goos/kernel/wait"
)

func waitProcess(handler nativeCallHandler, processHandle uint64, resultAddress uint64, timeout uint64, task uint64) (kcall.Status, bool) {
	if task >= sched.MaxTasks || !vm.ValidateUserRange(resultAddress, 8) {
		return kcall.StatusInvalidArgument, false
	}
	taskID := sched.TaskID(amd64.TaskIDForSlot(task))
	if _, _, valid := taskID.Parts(); !valid {
		return kcall.StatusInvalidArgument, false
	}
	if status := processWaitStatus.take(task); status != kcall.StatusOK {
		return status, false
	}
	owner, found := handler.self()
	if !found {
		return kcall.StatusPermissionDenied, false
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(processHandle), handle.TypeProcess, capability.Wait)
	if !allowed {
		return kcall.StatusPermissionDenied, false
	}
	pid := process.ID(authority.Object())
	event, status := processRegistry.Wait(pid)
	switch status {
	case process.WaitExited:
		return reportProcessExit(handler, resultAddress, event), false
	case process.WaitNotFound:
		return kcall.StatusNotFound, false
	}
	if timeout == 0 {
		return kcall.StatusTimeout, false
	}
	now := amd64.TimerTicks()
	deadline := now + timeout
	if deadline < now || !processWaitSet.Add(taskID, pid, deadline) {
		return kcall.StatusResourceExhausted, false
	}
	// Re-check after registering. The process can exit between the check above
	// and the registration, and that wakeup would find no waiter and be lost,
	// leaving this task asleep until its deadline.
	if event, status := processRegistry.Wait(pid); status != process.WaitRunning {
		processWaitSet.Cancel(taskID)
		processWaitStatus.set(uint64(task), kcall.StatusOK)
		if status != process.WaitExited {
			return kcall.StatusNotFound, false
		}
		return reportProcessExit(handler, resultAddress, event), false
	}
	return kcall.StatusNotFound, true
}

func wakeProcessWaiters(pid process.ID) {
	local := thisCPU()
	publishWaitAny(kernelwait.Key{Kind: kernelwait.KindProcess, Object: uint64(pid)}, kernelwait.Exited)
	count := processWaitSet.Wake(pid, local.processWakeResults[:])
	for index := 0; index < count; index++ {
		// A task another processor killed after it registered fails the
		// wake; its exit cancels what it left registered.
		amd64.WakeTaskID(uint64(local.processWakeResults[index].Task))
		local.processWakeResults[index] = process.WaitResult{}
	}
}

// expireProcessWaiter times out task's process wait if it is still due.
func expireProcessWaiter(task sched.TaskID, now uint64) {
	result, expired := processWaitSet.ExpireTask(task, now)
	slot, _, valid := result.Task.Parts()
	if !expired || !valid {
		return
	}
	processWaitStatus.set(uint64(slot), kcall.StatusTimeout)
	// A task another processor killed after it registered fails the wake;
	// its exit cancels what it left registered.
	amd64.WakeTaskID(uint64(result.Task))
}

//export goos_dispatch_process_wait
func dispatchProcessWait(processHandle uint64, resultAddress uint64, timeout uint64, rootAndTask uint64) uint64 {
	call, ok := beginNativeCall(kcall.OperationProcessWait, 0, 0)
	if !ok {
		return uint64(kcall.ActionRetry) << 32
	}
	status, blocked := waitProcess(nativeCallHandler{root: rootAndTask &^ 0xfff}, processHandle, resultAddress, timeout, rootAndTask&0xfff)
	action := kcall.ActionReturn
	if blocked {
		action = kcall.ActionBlock
	}
	return call.end(uint64(action)<<32 | uint64(status))
}

// irqLine names the line of an IRQ capability's object. The disk's line 11
// event is taken by IRQWait, whose single waiter the interrupt wakes, or by
// IRQTake after a WaitAny reported it.
func irqLine(object uint64) device.IRQLine {
	netObject, routed := netIRQObject()
	return device.ClassifyIRQ(object, netObject, routed)
}

// irqPending reports, without consuming it, the pending event of a line
// WaitAny can watch.
func irqPending(object uint64) bool {
	switch irqLine(object) {
	case device.IRQNet:
		return amd64.NetIRQPending()
	case device.IRQDisk:
		return amd64.IRQ11Pending()
	}
	return false
}

func waitIRQ(handler nativeCallHandler, irqHandle uint64, task uint64) (kcall.Status, bool) {
	if task >= sched.MaxTasks {
		return kcall.StatusInvalidArgument, false
	}
	taskID := sched.TaskID(amd64.TaskIDForSlot(task))
	if _, _, valid := taskID.Parts(); !valid {
		return kcall.StatusInvalidArgument, false
	}
	owner, found := handler.self()
	if !found {
		return kcall.StatusPermissionDenied, false
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(irqHandle), handle.TypeIRQ, capability.Wait)
	if !allowed || authority.Object() != device.DiskIRQObject {
		return kcall.StatusPermissionDenied, false
	}
	if amd64.TakeIRQ11() {
		return kcall.StatusOK, false
	}
	amd64.SetIRQ11Waiter(task + 1)
	amd64.RearmIRQ11()
	// Re-check after registering. An interrupt delivered between the check
	// above and the registration would set the pending flag while no waiter was
	// recorded, and IRQ waits have no deadline to recover from that.
	if amd64.TakeIRQ11() {
		amd64.SetIRQ11Waiter(0)
		return kcall.StatusOK, false
	}
	return kcall.StatusNotFound, true
}

//export goos_dispatch_irq_wait
func dispatchIRQWait(irqHandle uint64, rootAndTask uint64) uint64 {
	call, ok := beginNativeCall(kcall.OperationIRQWait, 0, 0)
	if !ok {
		return uint64(kcall.ActionRetry) << 32
	}
	status, blocked := waitIRQ(nativeCallHandler{root: rootAndTask &^ 0xfff}, irqHandle, rootAndTask&0xfff)
	action := kcall.ActionReturn
	if blocked {
		action = kcall.ActionBlock
	}
	return call.end(uint64(action)<<32 | uint64(status))
}

func waitTraceObject(entries []kernelwait.Entry) uint64 {
	if len(entries) == 0 {
		return 0
	}
	return entries[0].Key.Object
}

func waitAny(handler nativeCallHandler, interestsAddress, count, resultAddress, task uint64) (kcall.Status, bool) {
	local := thisCPU()
	cursorMode := count&kcall.WaitAnyCursorFlag != 0
	count &^= kcall.WaitAnyCursorFlag
	interestSize := kcall.WaitAnyInterestSize
	if cursorMode {
		interestSize = kcall.WaitAnyCursorInterestSize
	}
	if task >= sched.MaxTasks || count > kernelwait.MaxObjects ||
		!vm.ValidateUserRange(resultAddress, kcall.WaitAnyResultSize) {
		return kcall.StatusInvalidArgument, false
	}
	taskID := sched.TaskID(amd64.TaskIDForSlot(task))
	if _, _, valid := taskID.Parts(); !valid {
		return kcall.StatusInvalidArgument, false
	}
	if completion, ready := waitAnyTable.Take(taskID); ready {
		if diag.Enabled {
			trace(diag.WaitTaken, pidOfTask(taskID), uint64(taskID), 0, uint64(completion.ReadyMask))
		}
		if completion.TimedOut {
			return kcall.StatusTimeout, false
		}
		return writeWaitAnyMask(handler.root, resultAddress, completion.ReadyMask), false
	}
	owner, found := handler.self()
	if !found {
		if diag.Enabled {
			trace(diag.WaitDenied, pidOfTask(taskID), uint64(taskID), handler.root, uint64(diag.DenyOwner))
		}
		return kcall.StatusPermissionDenied, false
	}
	wireSize := count * interestSize
	if count != 0 && (!vm.ValidateUserRange(interestsAddress, wireSize) ||
		!vm.CopyIn(handler.root, interestsAddress, local.waitAnyWire[:int(wireSize)])) {
		return kcall.StatusInvalidArgument, false
	}
	if !vm.CopyIn(handler.root, resultAddress, local.waitAnyResultWire[:]) {
		return kcall.StatusInvalidArgument, false
	}
	timeout := decodeUint64(local.waitAnyResultWire[0:8])
	for index := 0; index < int(count); index++ {
		offset := index * int(interestSize)
		handleID := handle.ID(decodeUint64(local.waitAnyWire[offset : offset+8]))
		events := kcall.WaitEvent(decodeUint64(local.waitAnyWire[offset+8 : offset+16]))
		var entry kernelwait.Entry
		var status kcall.Status
		var reason diag.Deny
		if uint64(handleID) == kcall.WaitAnyThread {
			entry, status, reason = threadWaitEntry(taskID, events)
		} else if thread, end := kcall.ThreadEndTarget(uint64(handleID)); end {
			entry, status, reason = threadEndEntry(owner, thread, events)
		} else {
			entry, status, reason = resolveWaitAny(owner, handleID, events)
		}
		if status != kcall.StatusOK {
			trace(diag.WaitDenied, owner.ID(), uint64(taskID), uint64(handleID), uint64(reason))
			if diag.Enabled {
				traceDeniedInterests(handler.root, interestsAddress, count, interestSize)
			}
			return status, false
		}
		if entry.Key.Kind == kernelwait.KindLog {
			if !cursorMode {
				return kcall.StatusUnsupported, false
			}
			entry.Cursor = decodeUint64(local.waitAnyWire[offset+16 : offset+24])
		} else if cursorMode && decodeUint64(local.waitAnyWire[offset+16:offset+24]) != 0 {
			return kcall.StatusInvalidArgument, false
		}
		local.waitAnyEntries[index] = entry
	}
	entries := local.waitAnyEntries[:int(count)]
	if !kernelwait.ValidEntries(entries) {
		return kcall.StatusResourceExhausted, false
	}
	if mask := readyWaitAny(owner, entries); mask != 0 {
		trace(diag.WaitReady, owner.ID(), uint64(taskID), entries[0].Key.Object, uint64(mask))
		return writeWaitAnyMask(handler.root, resultAddress, mask), false
	}
	if timeout == 0 {
		// A poll unmasks the lines it names too: the goroutine poller
		// polls with no wait while every thread is busy, and a line left
		// masked would hold its event back until a thread idles. An event
		// raised now stays pending for the next poll.
		rearmIRQEntries(entries)
		return kcall.StatusTimeout, false
	}
	// No interests is a timed sleep, so it needs a deadline to end on.
	if count == 0 && timeout == kcall.WaitAnyInfiniteTimeout {
		return kcall.StatusInvalidArgument, false
	}
	deadline := uint64(0)
	if timeout != kcall.WaitAnyInfiniteTimeout {
		now := amd64.TimerTicks()
		deadline = now + timeout
		if deadline < now {
			return kcall.StatusInvalidArgument, false
		}
	}
	if !waitAnyTable.Begin(taskID, entries, deadline) {
		return kcall.StatusResourceExhausted, false
	}
	trace(diag.WaitBegin, owner.ID(), uint64(taskID), waitTraceObject(entries), count)
	// A line is unmasked only once its waiter is registered, so the interrupt
	// it may raise at once finds someone to wake; the recheck below sees an
	// event that arrived before the registration.
	rearmIRQEntries(entries)
	// Registration precedes the final readiness observation: a publisher
	// changes an object's state under its lock before it publishes, so either
	// the check below sees the change or the publication sees this
	// registration. A wake before assembly commits the task to BLOCKED stays
	// pending and keeps it running.
	if mask := readyWaitAny(owner, entries); mask != 0 {
		waitAnyTable.Cancel(taskID)
		trace(diag.WaitRecheck, owner.ID(), uint64(taskID), waitTraceObject(entries), uint64(mask))
		return writeWaitAnyMask(handler.root, resultAddress, mask), false
	}
	return kcall.StatusNotFound, true
}

// rearmIRQEntries unmasks the lines a wait names. Its driver reads the
// device's interrupt status before it waits again, so the level-triggered
// line is no longer asserted for an event already taken.
func rearmIRQEntries(entries []kernelwait.Entry) {
	for index := range entries {
		if entries[index].Key.Kind != kernelwait.KindIRQ {
			continue
		}
		if irqLine(entries[index].Key.Object) == device.IRQNet {
			amd64.RearmNetIRQ()
		} else {
			amd64.RearmIRQ11()
		}
	}
}

func resolveWaitAny(owner *process.Process, id handle.ID, events kcall.WaitEvent) (kernelwait.Entry, kcall.Status, diag.Deny) {
	typeID, authority, found := owner.Handles().Describe(id)
	if !found {
		return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyHandle
	}
	switch typeID {
	case handle.TypeChannel:
		if events == 0 || events & ^(kcall.WaitReadable|kcall.WaitPeerClosed) != 0 {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyEvents
		}
		if !authority.Allows(capability.Read | capability.Wait) {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyRights
		}
		object, side, valid := ipc.DecodeEndpointObject(authority.Object())
		if !valid || !channelRegistry.Live(object) {
			return kernelwait.Entry{}, kcall.StatusNotFound, diag.DenyObject
		}
		return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindChannel, Object: object, Side: side}, Events: waitEvents(events)}, kcall.StatusOK, 0
	case handle.TypeStream:
		object, writer, valid := stream.DecodeEndpointObject(authority.Object())
		if !valid {
			return kernelwait.Entry{}, kcall.StatusNotFound, diag.DenyObject
		}
		side := uint8(0)
		allowed := kcall.WaitReadable | kcall.WaitPeerClosed
		rights := capability.Read | capability.Wait
		if writer {
			side, allowed, rights = 1, kcall.WaitWritable|kcall.WaitPeerClosed, capability.Write|capability.Wait
		}
		if events == 0 || events & ^allowed != 0 {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyEvents
		}
		if !authority.Allows(rights) {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyRights
		}
		if _, exists := streamRegistry.Lookup(object); !exists {
			return kernelwait.Entry{}, kcall.StatusNotFound, diag.DenyObject
		}
		return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindStream, Object: object, Side: side}, Events: waitEvents(events)}, kcall.StatusOK, 0
	case handle.TypeDevice:
		if authority.Object() == device.LogObject {
			if events != kcall.WaitReadable || !authority.Allows(capability.Read) {
				return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyRights
			}
			return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindLog, Object: device.LogObject}, Events: kernelwait.Readable}, kcall.StatusOK, 0
		}
		// Only the terminal consoles are waitable. Read authority is what a
		// reader already needs to take a byte, so waiting for one needs no more.
		if _, terminal := consoleInput(authority.Object()); !terminal {
			return kernelwait.Entry{}, kcall.StatusUnsupported, diag.DenyObject
		}
		if events == 0 || events & ^kcall.WaitReadable != 0 {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyEvents
		}
		if !authority.Allows(capability.Read) {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyRights
		}
		return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindConsole, Object: authority.Object()}, Events: kernelwait.Readable}, kcall.StatusOK, 0
	case handle.TypeIRQ:
		// The network and disk lines are waitable. Readiness is the pending
		// event, which the wait reports and IRQTake consumes. The disk's
		// line also keeps its IRQWait waiter, which the interrupt wakes
		// directly and which takes the event first.
		if irqLine(authority.Object()) == device.IRQOther {
			return kernelwait.Entry{}, kcall.StatusUnsupported, diag.DenyObject
		}
		if events != kcall.WaitReadable {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyEvents
		}
		if !authority.Allows(capability.Wait) {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyRights
		}
		return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindIRQ, Object: authority.Object()}, Events: kernelwait.Readable}, kcall.StatusOK, 0
	case handle.TypeProcess:
		if events != kcall.WaitCompleted {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyEvents
		}
		if !authority.Allows(capability.Wait) {
			return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyRights
		}
		pid := process.ID(authority.Object())
		if _, status := processRegistry.Wait(pid); status == process.WaitNotFound {
			return kernelwait.Entry{}, kcall.StatusNotFound, diag.DenyObject
		}
		return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindProcess, Object: uint64(pid)}, Events: kernelwait.Exited}, kcall.StatusOK, 0
	default:
		return kernelwait.Entry{}, kcall.StatusUnsupported, diag.DenyType
	}
}

// threadWaitEntry is an interest in the calling thread's wake flag
// (ADR 0204). The thread needs no handle for its own flag.
func threadWaitEntry(task sched.TaskID, events kcall.WaitEvent) (kernelwait.Entry, kcall.Status, diag.Deny) {
	if events != kcall.WaitReadable {
		return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyEvents
	}
	return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindThread, Object: uint64(task)}, Events: kernelwait.Readable}, kcall.StatusOK, 0
}

// threadEndEntry is an interest in the end of a native thread of the
// caller's process (kcall.WaitAnyThreadEnd). It names a thread ThreadJoin
// could name: one that runs or ended unjoined. The registry lock the call
// holds keeps the group from changing under it.
func threadEndEntry(owner *process.Process, thread uint64, events kcall.WaitEvent) (kernelwait.Entry, kcall.Status, diag.Deny) {
	if events != kcall.WaitCompleted {
		return kernelwait.Entry{}, kcall.StatusPermissionDenied, diag.DenyEvents
	}
	if owner.ABI() != process.ABINative {
		return kernelwait.Entry{}, kcall.StatusUnsupported, diag.DenyType
	}
	if _, found := owner.Threads().Find(sched.TaskID(thread)); !found {
		return kernelwait.Entry{}, kcall.StatusNotFound, diag.DenyObject
	}
	return kernelwait.Entry{Key: kernelwait.Key{Kind: kernelwait.KindThreadEnd, Object: thread}, Events: kernelwait.Exited}, kcall.StatusOK, 0
}

func waitEvents(events kcall.WaitEvent) kernelwait.Event {
	var result kernelwait.Event
	if events&kcall.WaitReadable != 0 {
		result |= kernelwait.Readable
	}
	if events&kcall.WaitWritable != 0 {
		result |= kernelwait.Writable
	}
	if events&kcall.WaitCompleted != 0 {
		result |= kernelwait.Exited
	}
	if events&kcall.WaitPeerClosed != 0 {
		result |= kernelwait.PeerClosed
	}
	return result
}

// readyWaitAny reports which entries are ready. owner is the waiter's
// process, whose thread group names the threads a thread-end entry watches.
func readyWaitAny(owner *process.Process, entries []kernelwait.Entry) uint64 {
	var mask uint64
	for index, entry := range entries {
		ready := false
		switch entry.Key.Kind {
		case kernelwait.KindChannel:
			channel, found := channelRegistry.Acquire(entry.Key.Object)
			if found {
				status := channel.ReceiveStatus(entry.Key.Side)
				channel.Unlock()
				ready = status == ipc.StatusOK && entry.Events&kernelwait.Readable != 0 ||
					status == ipc.StatusPeerDied && entry.Events&kernelwait.PeerClosed != 0
			}
		case kernelwait.KindStream:
			endpoint, found := streamRegistry.Lookup(entry.Key.Object)
			if found {
				status := endpoint.ReadStatus()
				if entry.Key.Side == 1 {
					status = endpoint.WriteStatus()
				}
				ready = status == stream.StatusOK || status == stream.StatusPeerClosed && entry.Events&kernelwait.PeerClosed != 0
			}
		case kernelwait.KindProcess:
			_, status := processRegistry.Wait(process.ID(entry.Key.Object))
			ready = status != process.WaitRunning
		case kernelwait.KindConsole:
			input, _ := consoleInput(entry.Key.Object)
			ready = amd64.InputPending(input)
		case kernelwait.KindIRQ:
			ready = irqPending(entry.Key.Object)
		case kernelwait.KindLog:
			ready = logfmt.SequenceBefore(entry.Cursor, systemLog.Total())
		case kernelwait.KindThread:
			ready = threadWakeSet(entry.Key.Object)
		case kernelwait.KindThreadEnd:
			state, found := owner.Threads().Find(sched.TaskID(entry.Key.Object))
			ready = !found || state != nativethread.Running
		}
		if ready {
			mask |= 1 << index
		}
	}
	return mask
}

func writeWaitAnyMask(root, resultAddress uint64, mask uint64) kcall.Status {
	local := thisCPU()
	putUint64(local.waitMaskWire[:], mask)
	if !vm.CopyOut(root, resultAddress+8, local.waitMaskWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func publishWaitAny(key kernelwait.Key, event kernelwait.Event) {
	local := thisCPU()
	count := waitAnyTable.Publish(key, event, local.waitAnyCompletions[:])
	for index := 0; index < count && index < len(local.waitAnyCompletions); index++ {
		if diag.Enabled {
			traceWaitKey(diag.WaitComplete, pidOfTask(local.waitAnyCompletions[index].Task), local.waitAnyCompletions[index].Task, key, uint64(event))
		}
		amd64.WakeTaskID(uint64(local.waitAnyCompletions[index].Task))
		local.waitAnyCompletions[index] = kernelwait.Completion{}
	}
}

// wakeConsoleWaiters publishes console readability from the kernel loop. The
// serial interrupt drains the UART so nothing is lost, and the wake travels the
// same path every other readiness does, so there is one place that completes a
// wait. It costs at most one tick of latency on a keystroke.
func wakeConsoleWaiters() {
	// Input arriving faster than it is read overruns the ring. Nothing can
	// recover the bytes, so the loss is reported rather than hidden; once is
	// enough, because a reader that has fallen behind will keep falling behind.
	for _, object := range [...]uint64{device.SerialConsoleObject, device.GraphicalConsoleObject} {
		input, _ := consoleInput(object)
		if dropped := amd64.InputDropped(input); dropped != 0 && !consoleOverrunReported {
			consoleOverrunReported = true
			kernelLogUint(logfmt.Warn, "console input dropped", "bytes", dropped)
		}
		if amd64.InputPending(input) {
			publishWaitAny(kernelwait.Key{Kind: kernelwait.KindConsole, Object: object}, kernelwait.Readable)
		}
	}
}

// wakeIRQWaiters publishes the pending events of the network and disk
// lines. Their interrupts wake the kernel loop, which runs this, so a
// driver's wait completes on the same path as every other readiness without
// a tick of delay. The disk's interrupt wakes the loop only when no IRQWait
// waiter took the event.
func wakeIRQWaiters() {
	if object, routed := netIRQObject(); routed && amd64.NetIRQPending() {
		publishWaitAny(kernelwait.Key{Kind: kernelwait.KindIRQ, Object: object}, kernelwait.Readable)
	}
	if amd64.IRQ11Pending() {
		publishWaitAny(kernelwait.Key{Kind: kernelwait.KindIRQ, Object: device.DiskIRQObject}, kernelwait.Readable)
	}
}

// IRQTake consumes the pending event of the network or disk line without
// blocking. A driver calls it after reading its device's interrupt status,
// so an event raised after that read stays pending for its next wait.
func (handler nativeCallHandler) IRQTake(irqHandle uint64) kcall.Status {
	owner, found := handler.self()
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(irqHandle), handle.TypeIRQ, capability.Wait)
	if !allowed {
		return kcall.StatusPermissionDenied
	}
	taken := false
	switch irqLine(authority.Object()) {
	case device.IRQNet:
		taken = amd64.TakeNetIRQ()
	case device.IRQDisk:
		taken = amd64.TakeIRQ11()
	default:
		return kcall.StatusPermissionDenied
	}
	if !taken {
		return kcall.StatusNotFound
	}
	return kcall.StatusOK
}

// expireWaitAnyTask times out task's multi-object wait if it is still due.
func expireWaitAnyTask(task sched.TaskID, now uint64) {
	if _, expired := waitAnyTable.ExpireTask(task, now); !expired {
		return
	}
	if diag.Enabled {
		trace(diag.WaitTimeout, pidOfTask(task), uint64(task), 0, 0)
	}
	// A sleeping Linux task resumes through its continuation, which accepts
	// only the wait it is in.
	if resumeLinuxIPCWaiter(task, linuxcompat.WakeTimeout) {
		return
	}
	amd64.WakeTaskID(uint64(task))
}

//export goos_dispatch_wait_any
func dispatchWaitAny(interestsAddress, count, resultAddress, rootAndTask uint64) uint64 {
	call, ok := beginNativeCall(kcall.OperationWaitAny, 0, 0)
	if !ok {
		return uint64(kcall.ActionRetry) << 32
	}
	status, blocked := waitAny(nativeCallHandler{root: rootAndTask &^ 0xfff}, interestsAddress, count, resultAddress, rootAndTask&0xfff)
	action := kcall.ActionReturn
	if blocked {
		action = kcall.ActionBlock
	}
	return call.end(uint64(action)<<32 | uint64(status))
}

// expireWaitDeadlines releases waiters whose deadline has passed. It is driven
// from serviceKernel: a deadline is the last line of defence behind an event
// wakeup, so it must not depend on any particular task being scheduled. The
// deadline index names the waits that are due, so a tick with none due costs
// one load. Each table then decides, under its own lock, whether the wait is
// still there to time out: a wake that came first has taken it away.
func expireWaitDeadlines() {
	local := thisCPU()
	now := amd64.TimerTicks()
	for {
		count := deadlines.Expired(now, local.expiredDeadlines[:])
		for index := 0; index < count; index++ {
			entry := local.expiredDeadlines[index]
			switch entry.Source {
			case deadline.SourceChannel:
				expireIPCWaiter(entry.Task, now)
			case deadline.SourceStream:
				expireStreamWaiter(entry.Task, now)
			case deadline.SourceProcess:
				expireProcessWaiter(entry.Task, now)
			case deadline.SourceWaitAny:
				expireWaitAnyTask(entry.Task, now)
			case deadline.SourcePaging:
				retryExecutablePage(entry.Task, now)
			}
		}
		if count < len(local.expiredDeadlines) {
			break
		}
	}
	wakeConsoleWaiters()
	wakeIRQWaiters()
	if logNotification.Take() {
		wakeLogWaiters(systemLog.Total())
	}
}

// wakeLogWaiters runs in the kernel loop, outside native/lifecycle lock sets.
// Appenders only mark the pending bit after releasing the ring lock. Do not log.
func wakeLogWaiters(total uint64) {
	local := thisCPU()
	count := waitAnyTable.PublishLog(device.LogObject, total, local.waitAnyCompletions[:])
	for index := 0; index < count && index < len(local.waitAnyCompletions); index++ {
		amd64.WakeTaskID(uint64(local.waitAnyCompletions[index].Task))
		local.waitAnyCompletions[index] = kernelwait.Completion{}
	}
}
