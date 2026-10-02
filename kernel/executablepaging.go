//go:build uefi

package main

import (
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	"goos/kernel/ipc"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
)

type executableFault struct {
	task        sched.TaskID
	root        uint64
	page        uint64
	object      uint64
	offset      uint64
	count       uint64
	channelID   uint64
	side        uint8
	correlation uint64
	failed      bool
	// retryAt is the tick a request the filesystem could not serve is sent
	// again, and giveUp the tick after which a refusal is final. Both are
	// zero until the first such refusal.
	retryAt uint64
	giveUp  uint64
}

// A filesystem service that dies or restarts (ADR 0096) leaves the VFS and its
// handles in place, so a page it could not read becomes readable when the
// service is back. A faulting task waits that long rather than dying with a
// service that had only to restart.
const (
	executableRetryPerSecond = 10
	executableRetrySeconds   = 10
)

var (
	executableFaults      [sched.MaxTasks]executableFault
	executableCorrelation uint64
	executableReply       ipc.Message
)

// requestExecutablePage queues one bounded random read and parks the faulting
// task. The same fault may re-enter before the next timer switch; it does not
// queue a second request.
func requestExecutablePage(owner *process.Process, entry region.Region, page uint64) (pending bool, failed bool) {
	state := amd64.ReadSchedulerState()
	if state.Current >= sched.MaxTasks {
		return false, true
	}
	slot := int(state.Current)
	current := &executableFaults[slot]
	task := sched.TaskID(amd64.TaskIDForSlot(state.Current))
	if _, _, valid := task.Parts(); !valid {
		return false, true
	}
	if current.task != 0 && current.task != task {
		*current = executableFault{}
	}
	if current.task != 0 {
		if current.failed {
			*current = executableFault{}
			return false, true
		}
		amd64.BlockCurrentTask()
		return true, false
	}
	within := page - entry.Base
	if within >= entry.Data {
		return false, false
	}
	*current = executableFault{task: task, root: owner.AddressSpace().Root, page: page, object: entry.Object,
		offset: entry.Offset + within, count: min(vm.PageSize, entry.Data-within)}
	if !sendExecutableRequest(current) {
		*current = executableFault{}
		return false, true
	}
	amd64.BlockCurrentTask()
	return true, false
}

// sendExecutableRequest asks the VFS for the page fault describes, under a
// fresh correlation.
func sendExecutableRequest(fault *executableFault) bool {
	object, found := executables.Find(fault.object)
	if !found {
		return false
	}
	channelID, side, file, source := object.Source()
	channel, live := channelRegistry.Lookup(channelID)
	if !source || !live {
		return false
	}
	executableCorrelation++
	if executableCorrelation == 0 {
		executableCorrelation++
	}
	message, encoded := vfs.EncodeRequest(vfs.Request{
		Operation: vfs.OperationReadExecutable,
		Handle:    vfs.Handle(file),
		Offset:    fault.offset,
		Size:      uint32(fault.count),
	}, executableCorrelation)
	if !encoded || channel.Send(side, message, nil, nil, nil) != ipc.StatusOK {
		return false
	}
	fault.channelID, fault.side, fault.correlation, fault.retryAt = channelID, side, executableCorrelation, 0
	wakeIPCWaiters(channelID, side^1, ipc.WaitMessage)
	return true
}

// retryExecutablePages sends again each page request whose filesystem was
// away, once its retry tick has come. A request that cannot be sent fails.
func retryExecutablePages() {
	now := amd64.TimerTicks()
	for index := range executableFaults {
		fault := &executableFaults[index]
		if fault.task == 0 || fault.failed || fault.retryAt == 0 || now < fault.retryAt {
			continue
		}
		if !sendExecutableRequest(fault) {
			fault.failed = true
			amd64.WakeTaskID(uint64(fault.task))
		}
	}
}

// executablePagePending reports whether task waits for a page of its image
// that a copy or a fault requested and that has neither arrived nor failed.
func executablePagePending(task sched.TaskID) bool {
	slot, _, valid := task.Parts()
	if !valid || int(slot) >= len(executableFaults) {
		return false
	}
	fault := &executableFaults[slot]
	return fault.task == task && !fault.failed
}

// completeExecutablePage consumes a VFS reply before another request can
// reuse the transport slot. It returns true when it consumed the endpoint's
// queued message. Processes forked from one program share its source, so
// several tasks can wait on one endpoint; the reply goes to the task whose
// request it answers, found by correlation.
func completeExecutablePage(channelID uint64, side uint8) bool {
	waiting := false
	for index := range executableFaults {
		fault := &executableFaults[index]
		if fault.task != 0 && !fault.failed && fault.channelID == channelID && fault.side == side {
			waiting = true
			break
		}
	}
	if !waiting {
		return false
	}
	channel, live := channelRegistry.Lookup(channelID)
	if !live {
		// The source's service is gone: every request on it fails.
		for index := range executableFaults {
			fault := &executableFaults[index]
			if fault.task != 0 && !fault.failed && fault.channelID == channelID && fault.side == side {
				fault.failed = true
				amd64.WakeTaskID(uint64(fault.task))
			}
		}
		return false
	}
	var status ipc.Status
	executableReply, status = channel.Receive(side)
	if status != ipc.StatusOK {
		return false
	}
	slot := -1
	for index := range executableFaults {
		fault := &executableFaults[index]
		if fault.task != 0 && !fault.failed && fault.channelID == channelID && fault.side == side && fault.correlation == executableReply.Correlation {
			slot = index
			break
		}
	}
	if slot < 0 {
		// A reply to a request whose task has gone or given up.
		return true
	}
	fault := &executableFaults[slot]
	reply, valid := vfs.DecodeReplyFrom(&executableReply)
	if valid && reply.Operation == vfs.OperationReadExecutable && (reply.Status == vfs.StatusPeerDied || reply.Status == vfs.StatusTimeout) {
		now := amd64.TimerTicks()
		if fault.giveUp == 0 {
			fault.giveUp = now + executableRetrySeconds*clock.TicksPerSecond
		}
		if now < fault.giveUp {
			// The task stays parked; retryExecutablePages asks again.
			fault.retryAt = now + max(clock.TicksPerSecond/executableRetryPerSecond, 1)
			return true
		}
	}
	valid = valid && reply.Operation == vfs.OperationReadExecutable && reply.Status == vfs.StatusOK && reply.Size == fault.count
	if valid {
		valid = installExecutablePage(*fault)
	}
	task := fault.task
	if valid {
		*fault = executableFault{}
	} else {
		fault.failed = true
	}
	amd64.WakeTaskID(uint64(task))
	return true
}

func installExecutablePage(fault executableFault) bool {
	owner, found := processRegistry.ByRoot(fault.root)
	if !found {
		return false
	}
	// Two threads that touched the same page each asked for it. The first
	// reply mapped it; the second has nothing left to do and its thread
	// retries the access against the page already there.
	if vm.UserPageMapped(fault.root, fault.page) {
		return true
	}
	if owner.Available(process.ResourcePages) < 1+faultTables {
		return false
	}
	entry, found := owner.Regions().Find(fault.page)
	if !found || entry.Backing != region.Image || entry.Flags&region.FromObject == 0 || entry.Object != fault.object {
		return false
	}
	cacheFrame, found := bootFileCache.Frame(vm.PhysicalPage, fsproto.CachePages-1)
	cache, mapped := vm.PhysicalPage(cacheFrame)
	if !found || !mapped {
		return false
	}
	physical, allocated := pageAllocator.Allocate()
	if !allocated || !vm.InitializePhysical(physical, cache[:fault.count]) {
		if allocated {
			pageAllocator.Free(physical)
		}
		return false
	}
	tables, mapped := vm.MapUserPageAt(&pageAllocator, fault.root, fault.page, physical, regionPermissions(entry.Protection), true)
	if tables != 0 {
		owner.Charge(process.ResourcePages, uint32(tables))
	}
	if !mapped {
		pageAllocator.Free(physical)
		return false
	}
	owner.Charge(process.ResourcePages, 1)
	return true
}
