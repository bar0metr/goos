//go:build uefi

package main

import (
	"goos/kernel/arch/amd64"
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
}

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
	object, found := executables.Find(entry.Object)
	if !found {
		return false, true
	}
	channelID, side, file, source := object.Source()
	channel, live := channelRegistry.Lookup(channelID)
	if !source || !live {
		return false, true
	}
	count := min(vm.PageSize, entry.Data-within)
	offset := entry.Offset + within
	executableCorrelation++
	if executableCorrelation == 0 {
		executableCorrelation++
	}
	message, encoded := vfs.EncodeRequest(vfs.Request{
		Operation: vfs.OperationReadExecutable,
		Handle:    vfs.Handle(file),
		Offset:    offset,
		Size:      uint32(count),
	}, executableCorrelation)
	if !encoded || channel.Send(side, message, nil, nil, nil) != ipc.StatusOK {
		return false, true
	}
	*current = executableFault{task: task, root: owner.AddressSpace().Root, page: page, object: entry.Object,
		offset: offset, count: count, channelID: channelID, side: side, correlation: executableCorrelation}
	wakeIPCWaiters(channelID, side^1, ipc.WaitMessage)
	amd64.BlockCurrentTask()
	return true, false
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
// queued message.
func completeExecutablePage(channelID uint64, side uint8) bool {
	slot := -1
	for index := range executableFaults {
		fault := &executableFaults[index]
		if fault.task != 0 && fault.channelID == channelID && fault.side == side {
			slot = index
			break
		}
	}
	if slot < 0 {
		return false
	}
	fault := &executableFaults[slot]
	channel, live := channelRegistry.Lookup(channelID)
	if !live {
		fault.failed = true
		amd64.WakeTaskID(uint64(fault.task))
		return false
	}
	var status ipc.Status
	executableReply, status = channel.Receive(side)
	reply, valid := vfs.DecodeReplyFrom(&executableReply)
	valid = status == ipc.StatusOK && valid && executableReply.Correlation == fault.correlation &&
		reply.Operation == vfs.OperationReadExecutable && reply.Status == vfs.StatusOK && reply.Size == fault.count
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
	if !found || owner.Available(process.ResourcePages) < 1+faultTables || vm.UserPageMapped(fault.root, fault.page) {
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
