//go:build uefi

package main

// Process exit and teardown: CPU accounting, exit delivery to PID 1, and the
// release of everything an exited process held (ADR 0050).

import (
	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/internal/validation"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	"goos/kernel/device"
	"goos/kernel/diag"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
	supervisor "goos/userspace/init"
)

// releasingProcess names the process releaseProcessHandles is tearing down. A
// closure capturing it would be built on the kernel's leaking heap.
var releasingProcess process.ID

func (handler nativeCallHandler) Exit(code uint64) kcall.Status {
	amd64.SetCurrentTaskExitCode(code)
	if bootProcessA != nil && handler.root == bootProcessA.AddressSpace().Root {
		userExited = 1
	}
	// Once is enough to show the call works. Printed on every exit, the marker
	// lands at arbitrary places on a console the terminal service owns, which
	// cannot know the cursor moved (ADR 0089).
	if userExitAnnounced == 0 {
		userExitAnnounced = 1
		mark("USER_EXIT_OK")
	}
	return kcall.StatusOK
}

func deliverProcessExits() {
	for task := amd64.TakeExitedTask(); task != 0; task = amd64.TakeExitedTask() {
		taskID := sched.TaskID(task)
		cancelLinuxExec(taskID)
		wakeVFSContextWaiter(linuxcompat.CancelVFSTask(taskID))
		linuxcompat.SyscallContinuations.Cancel(taskID)
		if waitAnyTable.Cancel(taskID) && diag.Enabled {
			trace(diag.WaitCancelExit, pidOfTask(taskID), task, 0, 0)
		}
		// The exiting task's run time is folded into its process before the
		// slot is released, so the process keeps the time and the next task to
		// take the slot starts from zero.
		accountTaskCPU(taskID)
		forgetFutexWaiter(taskID)
		code := amd64.TaskExitCode(task)
		if validation.Enabled {
			noteStackGuardFault(taskID, code)
		}
		owner, _ := processRegistry.ByTask(taskID)
		// A task exec ended ran in an address space exec has destroyed.
		execEnded := isExecEnded(taskID)
		event, report := processRegistry.ReportTaskExit(taskID, code)
		liveRoot := uint64(0)
		if report == process.ExitTaskRemoved && owner != nil && !execEnded {
			liveRoot = owner.AddressSpace().Root
		}
		if report == process.ExitTaskRemoved && owner != nil && owner.ABI() == process.ABILinuxX8664 {
			endLinuxThread(taskID, owner, code)
		} else {
			linuxcompat.RemoveThread(taskID)
			takeExecEnded(taskID)
		}
		// Reclaiming the guarded kernel stack is independent bookkeeping. It must
		// not gate the steps below: a process that has exited has to release its
		// waiters and its handles even if its stack cannot be returned, or its
		// peers wait forever for an EOF that never arrives.
		if !amd64.ReclaimExitedTask(&pageAllocator, kernelPageTable, task, liveRoot) {
			taskStackReclaimFailures++
			if taskStackReclaimFailures == 1 {
				kernelLog(logfmt.Warn, "TASK_STACK_RECLAIM_FAILED", nil)
			}
		}
		if report != process.ExitProcessEnded {
			continue
		}
		releaseExitedProcess(event)
	}
}

// accountTaskCPU moves a task's tick count onto its process and clears the
// slot counter.
func accountTaskCPU(taskID sched.TaskID) {
	slot, _, valid := taskID.Parts()
	if !valid {
		return
	}
	if owner, found := processRegistry.ByTask(taskID); found {
		owner.AccountCPU(amd64.TaskTicks(uint64(slot)))
	}
	amd64.ClearTaskTicks(uint64(slot))
}

// processCPUTicks reports a process's total run time: what its exited tasks
// contributed plus what its live tasks have accumulated since.
func processCPUTicks(owner *process.Process) uint64 {
	total := owner.CPUTicks()
	for _, taskID := range owner.Tasks() {
		if slot, _, valid := taskID.Parts(); valid {
			total += amd64.TaskTicks(uint64(slot))
		}
	}
	return total
}

// releaseExitedProcess performs the observable half of process teardown first,
// so that waiters and peers are released before any cleanup that may fail.
func releaseExitedProcess(event process.ExitEvent) {
	processExitDelivered = 1
	trace(diag.ProcessExited, event.Process, uint64(event.Task), 0, event.Code)
	if waited, status := processRegistry.Wait(event.Process); status == process.WaitExited && waited == event {
		processWaitConfirmed = 1
	}
	wakeProcessWaiters(event.Process)
	if event.Process == 1 {
		initExitObserved = 1
		initExitCode = event.Code
		return
	}
	exited, found := processRegistry.ByID(event.Process)
	// PID 1 holds no handle to a forked Linux process and is not told it ended.
	forked := false
	if found {
		state, linux := linuxcompat.FindProcess(exited.AddressSpace().Root)
		forked = linux && state.Forked
	}
	if lifecycleChannel != nil && !forked {
		message, encoded := supervisor.ProcessExited(uint64(event.Process), event.Code)
		if encoded && queueLifecycle(message) && event.Process == 3 {
			supervisorExitDelivered = 1
		}
	}
	if found {
		exitLinuxProcess(exited, event.Code)
		releaseProcessHandles(exited)
		releasingProcess = exited.ID()
		releaseSharedMappings(exited)
		releasingProcess = 0
		before := pageAllocator.Statistics().FreePages
		if !vm.DestroyUserAddressSpace(&pageAllocator, exited.AddressSpace().Root) {
			addressSpaceReclaimFailures++
			if addressSpaceReclaimFailures == 1 {
				kernelLog(logfmt.Warn, "ADDRESS_SPACE_RECLAIM_FAILED", nil)
			}
		} else if pageAllocator.Statistics().FreePages > before {
			processReclaimConfirmed = 1
		}
		// The address space is gone, so its pages are no longer held. Channels
		// and streams the process created are released by their own registries
		// when their endpoints close, which may be later than this.
		exited.Release(process.ResourcePages, exitedPages(exited))
		if id := exited.Executable(); id != 0 {
			releasingProcess = exited.ID()
			dropExecutable(id)
			releasingProcess = 0
		}
	}
	processRegistry.Remove(event.Process)
	for index := range spawnedProcesses {
		if spawnedProcesses[index] != nil && spawnedProcesses[index].ID() == event.Process {
			spawnedProcesses[index].Reset()
			break
		}
	}
}

// releaseProcessHandles closes everything the exited process held so that the
// peer of each channel and stream observes closure instead of blocking.
func releaseProcessHandles(exited *process.Process) {
	releasingProcess = exited.ID()
	exited.Handles().CloseAll(releaseProcessHandle)
	releasingProcess = 0
}

func releaseProcessHandle(typeID handle.Type, cap capability.Capability) {
	if typeID == handle.TypeDevice {
		if _, dma := device.DecodeDMAObject(cap.Object()); dma && virtioBlockPort != 0 {
			amd64.PortWrite(virtioBlockPort+18, 1, 0)
			amd64.QuiesceIRQ11()
		}
	}
	if typeID == handle.TypeMemory {
		releaseMemory(cap)
		return
	}
	if typeID == handle.TypeStream {
		// One reference goes with each handle. The end closes only when the
		// process was its last holder (ADR 0089).
		dropStreamCapability(cap)
		return
	}
	if typeID != handle.TypeChannel {
		return
	}
	channelID, side, valid := ipc.DecodeEndpointObject(cap.Object())
	channel, exists := channelRegistry.Lookup(channelID)
	if valid && exists && channel.Close(side) {
		trace(diag.ChannelClosed, releasingProcess, 0, channelID|uint64(side)<<63, diag.CloseByTeardown)
		wakeIPCWaiters(channelID, side^1, ipc.WaitPeerDied)
		releaseChannelSlot(channelID)
	}
}

// exitedPages reports the page reservation a process still holds, so teardown
// returns exactly that and an accounting bug shows up as a refused release
// rather than as a counter that wraps.
func exitedPages(exited *process.Process) uint32 {
	used, _ := exited.Quota()
	return used[process.ResourcePages]
}

func reportProcessExit(handler nativeCallHandler, resultAddress uint64, event process.ExitEvent) kcall.Status {
	putUint64(exitWire[:], event.Code)
	if !vm.CopyOut(handler.root, resultAddress, exitWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}
