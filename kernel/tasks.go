//go:build uefi

package main

// Kernel-side tasks and the dispatch entry points the scheduler calls.

import (
	"sync/atomic"

	"goos/internal/logfmt"
	"goos/internal/validation"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/sched"
)

// Entry counters; every processor adds to them.
var nativeCallABIRejections atomic.Uint64
var linuxSyscallEntries atomic.Uint64
var linuxSyscallABIRejections atomic.Uint64
var linuxSyscallFrameRejections atomic.Uint64

// authorizeNativeCall rejects the native gate before any operation-specific
// dispatcher observes registers belonging to another process ABI. The first
// rejection is logged and the counter remains bounded to one machine word.
//
//export goos_authorize_native_call
func authorizeNativeCall(root uint64) uint64 {
	if owner, found := processRegistry.Owner(amd64.CurrentTask()); found && owner.ABI() == process.ABINative && owner.AddressSpace().Root == root {
		return uint64(kcall.StatusOK)
	}
	if nativeCallABIRejections.Add(1) == 1 {
		kernelLogUint(logfmt.Warn, "native call ABI rejected", "root", root)
	}
	return uint64(kcall.StatusPermissionDenied)
}

// dispatchLinuxSyscall validates the architectural return state and Process
// personality before the compatibility subsystem observes the request. The
// assembly caller owns frame on the current task's guarded kernel stack.
//
// Most calls run holding their family's lock and the domains a file or
// process call can reach (linuxCallLocks). The calls of the fast class hold
// only their address space for reading. They find the caller's state and
// thread ID in a per-task cache, and take the family's lock after the call
// only when a signal is pending, to deliver it.
//
//export goos_dispatch_linux_syscall
func dispatchLinuxSyscall(frame *linuxabi.Frame) {
	local := thisCPU()
	if frame == nil {
		return
	}
	frame.Return = linuxabi.Negative(linuxabi.EINVAL)
	frame.Action = linuxabi.ActionReturn
	if !vm.ValidateUserRange(frame.UserIP, 1) || !vm.ValidateUserRange(frame.UserStack, 0) {
		if linuxSyscallFrameRejections.Add(1) == 1 {
			kernelLogUint(logfmt.Warn, "Linux syscall frame rejected", "ip", frame.UserIP)
		}
		amd64.SetCurrentTaskExitCode(uint64(1)<<63 | 13)
		frame.Action = linuxabi.ActionExit
		return
	}
	task := amd64.CurrentTask()
	owner, space := callerSpace()
	slot, _, _ := task.Parts()
	if owner == nil || owner.ABI() != process.ABILinuxX8664 || owner.AddressSpace().Root != frame.Root {
		if linuxSyscallABIRejections.Add(1) == 1 {
			kernelLogUint(logfmt.Warn, "Linux syscall ABI rejected", "root", frame.Root)
		}
		frame.Return = linuxabi.Negative(linuxabi.EPERM)
		return
	}
	linuxSyscallEntries.Add(1)
	local.callKind, local.callNumber = 'l', frame.Number
	handler := &local.linuxHandler
	handler.root, handler.frame, handler.task = frame.Root, frame, task
	set := linuxCallLocks(linuxabi.Syscall(frame.Number))
	stop := amd64.StopWord()
	caller := linuxCaller(task, frame.Root)
	handler.state, handler.tid = caller.state, caller.tid
	// A file call moves a handle to the VFS service, whose table it needs as
	// well as its own. The task's last such peer is taken at once; a call
	// that needs another stops before it changes anything and runs again
	// holding that one.
	held := &local.linuxCall
	peer := process.ID(linuxPeerHints[slot].Load())
	var result linuxcompat.Result
	for attempt := 0; ; attempt++ {
		*held = heldLocks{set: set, owner: owner, peerID: peer, family: caller.family}
		if !held.take(stop) {
			frame.Action = linuxabi.ActionRetry
			return
		}
		if set&linuxState != 0 && caller.state != nil {
			// The call works with, and returns under, its own thread's
			// signal mask.
			linuxcompat.SelectThreadSignals(caller.state, task)
		}
		local.peerWanted = 0
		result = linuxcompat.Dispatch(frame, handler)
		if local.peerWanted == 0 {
			break
		}
		peer = local.peerWanted
		linuxPeerHints[slot].Store(uint64(peer))
		held.drop()
		if attempt == 1 {
			frame.Action = linuxabi.ActionRetry
			return
		}
	}
	wanted, wants := uint64(0), false
	if result.Errno == linuxabi.EFAULT {
		if wanted, wants = takeExecutableWant(task); wants {
			// A copy touched a page of the program's own image that an
			// executable object holds. The call changed nothing before the
			// copy; it runs again once the page is in.
			linuxcompat.SyscallContinuations.Cancel(task)
			ipcWaitSet.Cancel(task)
			result = linuxcompat.Result{Action: linuxabi.ActionBlock}
		} else {
			linuxcompat.NoteBadPointer(linuxabi.Syscall(frame.Number))
		}
	}
	if result.Action == linuxabi.ActionExec {
		if result.Entry == 0 || result.Stack == 0 || result.Root == 0 {
			result = linuxcompat.Result{Errno: linuxabi.EIO}
		} else {
			frame.UserIP, frame.UserStack, frame.Root = result.Entry, result.Stack, result.Root
		}
	}
	if result.Errno == linuxabi.EPIPE && handler.state != nil && (frame.Number == uint64(linuxabi.SysWrite) || frame.Number == uint64(linuxabi.SysWritev)) {
		// A write to a pipe nobody reads raises SIGPIPE as well as failing.
		// Linux reports it as sent by the writer itself. Writes run under the
		// family's lock.
		handler.state.Signals.Raise(linuxabi.SignalPipe, linuxcompat.SignalInfo{Code: linuxabi.SignalCodeUser, PID: handler.state.PID, UID: linuxcompat.StatUID})
	}
	frame.Return, frame.Action = result.Register(), result.Action
	if result.Restart && frame.Action == linuxabi.ActionReturn && frame.UserIP >= 2 {
		// The return enters a handler, or comes straight back, at the
		// SYSCALL instruction with the call number, as Linux restarts a call.
		frame.UserIP -= 2
		frame.Return = frame.Number
	}
	if set&linuxState == 0 {
		held.drop()
		if frame.Action == linuxabi.ActionReturn && caller.state != nil && caller.state.Signals.AnyPending() {
			// A stopped task skips delivery; its next call delivers.
			family := familyLock(caller.family)
			if family.LockUnless(stop) {
				if space.RLockUnless(stop) {
					handler.finish(frame)
					space.RUnlock()
				}
				family.Unlock()
			}
		}
	} else {
		if frame.Action == linuxabi.ActionReturn {
			handler.finish(frame)
		}
		held.drop()
	}
	if wants {
		frame.Action = executableCallStep(owner, wanted)
		if frame.Action == linuxabi.ActionReturn {
			linuxcompat.NoteBadPointer(linuxabi.Syscall(frame.Number))
			frame.Return = linuxabi.Negative(linuxabi.EFAULT)
		}
	}
	if frame.Action == linuxabi.ActionBlock && (frame.UserIP < 2 || !vm.ValidateUserRange(frame.UserIP-2, 2)) {
		linuxSyscallFrameRejections.Add(1)
		amd64.SetCurrentTaskExitCode(uint64(1)<<63 | 13)
		frame.Action = linuxabi.ActionExit
	}
}

// linuxPeerHints holds, per task slot, the process whose handle table the
// task's last file call needed.
var linuxPeerHints [sched.MaxTasks]atomic.Uint64

// executableCallStep brings in the image page a call's copy wanted, with the
// call's locks released, and returns how the Linux call goes on: again at
// once when the page is mapped, after the reply when a request is out, and
// with EFAULT when no page can come.
func executableCallStep(owner *process.Process, page uint64) uint64 {
	switch executableStep(owner, page, false, false) {
	case pageMapped, pageRetry:
		return linuxabi.ActionRetry
	case pageParked:
		return linuxabi.ActionBlock
	}
	return linuxabi.ActionReturn
}

// linuxCallerEntry is what a Linux task's calls need to know of it before
// they take any lock: its process state, its thread ID, and its family.
type linuxCallerEntry struct {
	task   sched.TaskID
	root   uint64
	state  *linuxcompat.ProcessState
	tid    int32
	family uint64
}

// linuxCallers caches, per task slot, the entry of the task's last call. A
// task writes only its own slot and the exit of the task clears it, so it is
// read without a lock. A state outlives every call of its tasks, and its
// thread ID and family never change; exec changes the root, which the entry
// checks.
var linuxCallers [sched.MaxTasks]linuxCallerEntry

// linuxCaller returns the calling task's entry, finding it in the
// personality's tables the first time.
func linuxCaller(task sched.TaskID, root uint64) linuxCallerEntry {
	slot, _, valid := task.Parts()
	if !valid || int(slot) >= len(linuxCallers) {
		return linuxCallerEntry{}
	}
	if entry := linuxCallers[slot]; entry.task == task && entry.root == root && entry.state != nil {
		return entry
	}
	state, found := linuxcompat.FindProcess(root)
	if !found {
		return linuxCallerEntry{}
	}
	entry := linuxCallerEntry{task: task, root: root, state: state, tid: linuxcompat.TaskTID(task, state), family: state.FamilyID}
	linuxCallers[slot] = entry
	return entry
}

// forgetLinuxCaller clears the entry of a task that exited.
func forgetLinuxCaller(task sched.TaskID) {
	if slot, _, valid := task.Parts(); valid && int(slot) < len(linuxCallers) {
		linuxCallers[slot] = linuxCallerEntry{}
	}
}

// finish delivers the caller's pending signals and ends a temporary mask no
// handler took over. The family's lock and the address space are held.
func (handler *linuxCallHandler) finish(frame *linuxabi.Frame) {
	state, found := linuxcompat.FindProcess(frame.Root)
	if !found {
		return
	}
	// Another thread's call may have selected its own mask since prepare.
	linuxcompat.SelectThreadSignals(state, handler.task)
	deliverLinuxSignals(frame, handler.task)
	state.Signals.EndMaskSwap()
}

// resumeLinuxSyscall accepts the exact continuation correlation before waking
// its task. A stale reply therefore cannot make a reused task slot runnable.
func resumeLinuxSyscall(task sched.TaskID, correlation uint64, reason linuxcompat.WakeReason) bool {
	if !linuxcompat.SyscallContinuations.Resume(task, correlation, reason) {
		return false
	}
	if amd64.WakeTaskID(uint64(task)) {
		return true
	}
	linuxcompat.SyscallContinuations.Cancel(task)
	return false
}

// dispatchKernelCall is the architecture-neutral native-call boundary. The
// packed return keeps the assembly bridge simple: action occupies the upper
// half and the typed native status occupies the lower half.
//
//export goos_dispatch_kernel_call
func dispatchKernelCall(number uint64, arg0 uint64, arg1 uint64, root uint64) uint64 {
	local := thisCPU()
	arg2 := amd64.KernelCallArg2()
	peer, extra := process.ID(0), lockSet(0)
	for attempt := 0; ; attempt++ {
		call, ok := beginNativeCall(kcall.Operation(number), peer, extra)
		if !ok {
			return uint64(kcall.ActionRetry) << 32
		}
		// The handler is reused through a pointer: a struct passed as an
		// interface would be boxed, and the kernel leaks every heap
		// allocation.
		local.dispatchHandler.root = root
		local.peerWanted, local.streamsWanted = 0, false
		result := kcall.Dispatch(kcall.Request{Operation: kcall.Operation(number), Arg0: arg0, Arg1: arg1, Arg2: arg2}, &local.dispatchHandler)
		if local.peerWanted == 0 {
			return call.end(uint64(result.Action)<<32 | uint64(result.Status))
		}
		// The call stopped before it changed anything, because it needs the
		// handle table of the process its handles move to. It runs again
		// holding both tables; a peer that keeps changing has it run again
		// after a scheduling point.
		peer = local.peerWanted
		if local.streamsWanted {
			extra = streams
		}
		call.end(0)
		if attempt == 1 {
			return uint64(kcall.ActionRetry) << 32
		}
	}
}

// nativeCall is the lock set a native call holds while it runs.
type nativeCall struct {
	held heldLocks
}

// beginNativeCall takes the set of operation, plus extra, for the calling
// task, with the handle table of peer when peer is not zero. It returns
// false, holding nothing, when the task is being stopped.
func beginNativeCall(operation kcall.Operation, peer process.ID, extra lockSet) (*nativeCall, bool) {
	local := thisCPU()
	local.callKind, local.callNumber = 'n', uint64(operation)
	call := &local.nativeCall
	owner, _ := callerSpace()
	call.held = heldLocks{set: nativeCallLocks(operation) | extra, owner: owner, peerID: peer}
	return call, call.held.take(amd64.StopWord())
}

// end releases the call's set and returns its packed result. A copy that
// needed a page an executable object holds left the call without effect; the
// page is brought in with the set released and the call runs again.
func (call *nativeCall) end(packed uint64) uint64 {
	owner := call.held.owner
	call.held.drop()
	page, wants := takeExecutableWant(amd64.CurrentTask())
	if !wants || owner == nil {
		return packed
	}
	switch executableStep(owner, page, false, false) {
	case pageMapped, pageRetry:
		return uint64(kcall.ActionRetry) << 32
	case pageParked:
		return uint64(kcall.ActionBlock) << 32
	}
	return packed
}

//export goos_demo_task_a
func runDemoTaskA() {
	for !validation.Enabled {
		amd64.KernelSleep()
	}
	for demoTaskA < 3 {
		demoTaskA++
		amd64.Halt()
	}
	demoABlocked = 1
	amd64.BlockCurrentTask()
	amd64.Halt()
	demoAResumed = 1
	demoTaskA++
	for {
		amd64.KernelSleep()
	}
}

//export goos_demo_task_b
func runDemoTaskB() {
	for !validation.Enabled {
		amd64.KernelSleep()
	}
	for {
		demoTaskB++
		if demoABlocked != 0 && demoAResumed == 0 && demoTaskB >= 6 {
			amd64.WakeTask(1)
		}
		if demoAResumed != 0 {
			break
		}
		amd64.Halt()
	}
	for {
		amd64.KernelSleep()
	}
}

//export goos_idle_task
func runIdleTask() {
	for {
		amd64.Halt()
	}
}
