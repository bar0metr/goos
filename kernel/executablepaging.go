//go:build uefi

package main

import (
	"sync/atomic"

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
	// physical is the page the reply filled, which the task maps when its
	// access runs again; zero until the reply comes.
	physical uint64
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

// executableFaults holds each task's outstanding page request. The paging
// lock guards the table and executableCorrelation. A request is recorded
// and sent under it, so the reply, which takes it too, always finds its
// record.
var (
	executableFaults      [sched.MaxTasks]executableFault
	executableCorrelation uint64
	// executableWanted is the page, plus one, a copy of the task's call needed
	// from an executable object and could not take. The task writes it and
	// takes it back itself.
	executableWanted [sched.MaxTasks]atomic.Uint64
)

// The ways executableStep ends.
type pageResult uint8

const (
	// pageMapped: the page is mapped; the access or the call runs again.
	pageMapped pageResult = iota + 1
	// pageParked: the request is out and the task waits for the reply.
	pageParked
	// pageRetry: the task is being stopped; nothing was done.
	pageRetry
	// pageFailed: no page can come.
	pageFailed
)

// wantExecutablePage records the page a copy of the calling task's call
// needs from an executable object.
func wantExecutablePage(page uint64) {
	if slot, _, valid := amd64.CurrentTask().Parts(); valid && int(slot) < len(executableWanted) {
		executableWanted[slot].Store(page + 1)
	}
}

// takeExecutableWant returns and clears the page the calling task's call
// wanted, if any.
func takeExecutableWant(task sched.TaskID) (uint64, bool) {
	slot, _, valid := task.Parts()
	if !valid || int(slot) >= len(executableWanted) {
		return 0, false
	}
	wanted := executableWanted[slot].Swap(0)
	return wanted - 1, wanted != 0
}

// executableStep supplies a page an executable object holds: an object in
// memory fills it at once, a file-backed object through a request to the
// VFS that parks the task until the reply. It takes the address space for
// reading and the executable lock, and gives up when the task is being
// stopped. The caller holds no lock.
func executableStep(owner *process.Process, page uint64, write bool, fetch bool) pageResult {
	stop := amd64.StopWord()
	space := owner.Space()
	if !space.RLockUnless(stop) {
		return pageRetry
	}
	if !executableLock.LockUnless(stop) {
		space.RUnlock()
		return pageRetry
	}
	result := executableStepLocked(owner, page, write, fetch)
	executableLock.Unlock()
	space.RUnlock()
	return result
}

func executableStepLocked(owner *process.Process, page uint64, write bool, fetch bool) pageResult {
	root := owner.AddressSpace().Root
	if vm.UserPageMapped(root, page) {
		return pageMapped
	}
	entry, kind := demandRegion(owner, root, page, write, fetch)
	if kind == faultMapped {
		return pageMapped
	}
	if kind != 0 {
		return pageFailed
	}
	if entry.Backing != region.Image || entry.Flags&region.FromObject == 0 {
		if resolved, _ := demandFault(owner, root, page, write, fetch); resolved {
			return pageMapped
		}
		return pageFailed
	}
	object, found := executables.Find(entry.Object)
	if !found {
		return pageFailed
	}
	if !object.FileBacked() {
		physical, allocated := pageAllocator.Allocate()
		if !allocated {
			return pageFailed
		}
		if !vm.InitializePhysical(physical, nil) || !fillFromOrigin(entry, page, physical) {
			pageAllocator.Free(physical)
			return pageFailed
		}
		if resolved, _ := mapDemandPage(owner, root, page, physical, entry); resolved {
			return pageMapped
		}
		return pageFailed
	}
	pagingLock.Lock()
	result := requestExecutablePage(owner, entry, page)
	pagingLock.Unlock()
	return result
}

// requestExecutablePage maps the page a reply delivered, or queues one
// bounded read and parks the faulting task. A task has one request out at a
// time; an access that runs again before the reply parks again. The paging
// lock is held.
func requestExecutablePage(owner *process.Process, entry region.Region, page uint64) pageResult {
	task := amd64.CurrentTask()
	slot, _, valid := task.Parts()
	if !valid {
		return pageFailed
	}
	current := &executableFaults[slot]
	if current.task != 0 && current.task != task {
		// A request of a task that held the slot before.
		clearExecutableFault(current)
	}
	if current.task != 0 {
		switch {
		case current.failed:
			clearExecutableFault(current)
			return pageFailed
		case current.physical == 0:
			amd64.BlockCurrentTask()
			return pageParked
		case current.page == page:
			physical := current.physical
			current.physical = 0
			clearExecutableFault(current)
			if resolved, _ := mapDemandPage(owner, owner.AddressSpace().Root, page, physical, entry); resolved {
				return pageMapped
			}
			return pageFailed
		}
		// The reply was for a page the task no longer touches.
		clearExecutableFault(current)
	}
	within := page - entry.Base
	if within >= entry.Data {
		// Past the segment's file bytes the page is zero.
		if resolved, _ := demandZeroImagePage(owner, page, entry); resolved {
			return pageMapped
		}
		return pageFailed
	}
	*current = executableFault{task: task, root: owner.AddressSpace().Root, page: page, object: entry.Object,
		offset: entry.Offset + within, count: min(vm.PageSize, entry.Data-within)}
	if !sendExecutableRequest(current) {
		*current = executableFault{}
		return pageFailed
	}
	amd64.BlockCurrentTask()
	return pageParked
}

// demandZeroImagePage maps a zeroed page of an image region past its file
// bytes.
func demandZeroImagePage(owner *process.Process, page uint64, entry region.Region) (bool, uint64) {
	physical, allocated := pageAllocator.Allocate()
	if !allocated {
		return false, faultQuota
	}
	if !vm.InitializePhysical(physical, nil) {
		pageAllocator.Free(physical)
		return false, faultQuota
	}
	return mapDemandPage(owner, owner.AddressSpace().Root, page, physical, entry)
}

// clearExecutableFault forgets a request and frees a page its reply filled
// that nobody mapped.
func clearExecutableFault(fault *executableFault) {
	if fault.physical != 0 {
		pageAllocator.Free(fault.physical)
	}
	*fault = executableFault{}
}

// cancelExecutableFault forgets the request of a task that has exited.
func cancelExecutableFault(task sched.TaskID) {
	slot, _, valid := task.Parts()
	if !valid || int(slot) >= len(executableFaults) {
		return
	}
	pagingLock.Lock()
	if executableFaults[slot].task == task {
		clearExecutableFault(&executableFaults[slot])
	}
	pagingLock.Unlock()
	executableWanted[slot].Store(0)
}

// sendExecutableRequest asks the VFS for the page fault describes, under a
// fresh correlation. The executable and paging locks are held.
func sendExecutableRequest(fault *executableFault) bool {
	object, found := executables.Find(fault.object)
	if !found {
		return false
	}
	channelID, side, file, source := object.Source()
	if !source {
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
	if !encoded || channelSend(channelID, side, &message, nil, nil, nil) != ipc.StatusOK {
		return false
	}
	fault.channelID, fault.side, fault.correlation, fault.retryAt = channelID, side, executableCorrelation, 0
	wakeIPCWaiters(channelID, side^1, ipc.WaitMessage)
	return true
}

// retryExecutablePages sends again each page request whose filesystem was
// away, once its retry tick has come. A request that cannot be sent fails.
// Housekeeping calls it holding the executable and paging locks.
func retryExecutablePages() {
	now := amd64.TimerTicks()
	for index := range executableFaults {
		fault := &executableFaults[index]
		if fault.task == 0 || fault.failed || fault.physical != 0 || fault.retryAt == 0 || now < fault.retryAt {
			continue
		}
		if !sendExecutableRequest(fault) {
			fault.failed = true
			amd64.WakeTaskID(uint64(fault.task))
		}
	}
}

// completeExecutablePage consumes a VFS reply before another request can
// reuse the transport slot. It returns true when it consumed the endpoint's
// queued message. Processes forked from one program share its source, so
// several tasks can wait on one endpoint; the reply goes to the task whose
// request it answers, found by correlation. The page's bytes are copied out
// of the file cache at once, since the next reply reuses it; the task maps
// the copy when its access runs again. The caller holds the shared-memory
// lock.
func completeExecutablePage(channelID uint64, side uint8) bool {
	pagingLock.Lock()
	completed := completeExecutablePageLocked(channelID, side)
	pagingLock.Unlock()
	return completed
}

func completeExecutablePageLocked(channelID uint64, side uint8) bool {
	local := thisCPU()
	waiting := false
	for index := range executableFaults {
		fault := &executableFaults[index]
		if fault.task != 0 && !fault.failed && fault.physical == 0 && fault.channelID == channelID && fault.side == side {
			waiting = true
			break
		}
	}
	if !waiting {
		return false
	}
	status := channelReceive(channelID, side, &local.executableReply)
	if status == ipc.StatusPeerDied {
		// The source's service is gone: every request on it fails.
		for index := range executableFaults {
			fault := &executableFaults[index]
			if fault.task != 0 && !fault.failed && fault.physical == 0 && fault.channelID == channelID && fault.side == side {
				fault.failed = true
				amd64.WakeTaskID(uint64(fault.task))
			}
		}
		return false
	}
	if status != ipc.StatusOK {
		return false
	}
	slot := -1
	for index := range executableFaults {
		fault := &executableFaults[index]
		if fault.task != 0 && !fault.failed && fault.physical == 0 && fault.channelID == channelID && fault.side == side && fault.correlation == local.executableReply.Correlation {
			slot = index
			break
		}
	}
	if slot < 0 {
		// A reply to a request whose task has gone or given up.
		return true
	}
	fault := &executableFaults[slot]
	reply, valid := vfs.DecodeReplyFrom(&local.executableReply)
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
		fault.physical, valid = copyExecutablePage(fault.count)
	}
	if !valid {
		fault.failed = true
	}
	amd64.WakeTaskID(uint64(fault.task))
	return true
}

// copyExecutablePage copies count bytes of the file cache's last page into a
// new zeroed page.
func copyExecutablePage(count uint64) (uint64, bool) {
	cacheFrame, found := bootFileCache.Frame(vm.PhysicalPage, fsproto.CachePages-1)
	cache, mapped := vm.PhysicalPage(cacheFrame)
	if !found || !mapped {
		return 0, false
	}
	physical, allocated := pageAllocator.Allocate()
	if !allocated {
		return 0, false
	}
	if !vm.InitializePhysical(physical, cache[:count]) {
		pageAllocator.Free(physical)
		return 0, false
	}
	return physical, true
}
