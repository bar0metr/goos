//go:build uefi

package main

import (
	"encoding/binary"

	"goos/internal/capability"
	"goos/internal/limits"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	linuxelf "goos/kernel/compat/linux/elf"
	"goos/kernel/executable"
	"goos/kernel/handle"
	"goos/kernel/loader"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/ramfs"
	"goos/kernel/sched"
	"goos/kernel/vfs"
)

const maxLinuxExecOperations = 4

type linuxExecState struct {
	task         sched.TaskID
	snapshot     linuxcompat.ExecSnapshot
	file         vfs.Handle
	fileSize     uint64
	objectHandle handle.ID
	objectID     uint64
	newRoot      uint64
	newPages     uint32
	regions      region.Map
	entry        uint64
	stack        uint64
	brkBase      uint64
	brkLimit     uint64
	failure      linuxabi.Errno
	closeHandles [linuxcompat.LinuxMaxFD]uint64
	closeCount   uint8
}

var (
	linuxExecStates [maxLinuxExecOperations]linuxExecState
	linuxExecString [linuxcompat.MaxExecStringBytes + 1]byte
)

func (state *linuxExecState) reset() {
	state.snapshot.Reset()
	state.task = 0
	state.file = 0
	state.fileSize = 0
	state.objectHandle = 0
	state.objectID = 0
	state.newRoot = 0
	state.newPages = 0
	state.entry = 0
	state.stack = 0
	state.brkBase = 0
	state.brkLimit = 0
	state.failure = 0
	state.closeCount = 0
}

func findLinuxExec(task sched.TaskID) (*linuxExecState, int, bool) {
	for index := range linuxExecStates {
		if linuxExecStates[index].task == task {
			return &linuxExecStates[index], index, true
		}
	}
	return nil, 0, false
}

func reserveLinuxExec(task sched.TaskID) (*linuxExecState, int, bool) {
	if task == 0 {
		return nil, 0, false
	}
	for index := range linuxExecStates {
		if linuxExecStates[index].task != 0 {
			continue
		}
		linuxExecStates[index].reset()
		linuxExecStates[index].task = task
		return &linuxExecStates[index], index, true
	}
	return nil, 0, false
}

// Execve replaces the calling Linux image after the candidate ELF, argument
// snapshot, address space, and initial stack have all been prepared.
func (handler *linuxCallHandler) Execve(request linuxcompat.Request) linuxcompat.Result {
	local := thisCPU()
	pending, active, errno := handler.vfsStage(request)
	state, slot, retained := findLinuxExec(handler.task)
	if errno != 0 {
		if retained {
			cancelLinuxExec(handler.task)
		}
		return linuxErrno(errno)
	}
	compat, found := linuxcompat.FindProcess(handler.root)
	if !found {
		return linuxErrno(linuxabi.EIO)
	}
	if !active {
		state, slot, retained = reserveLinuxExec(handler.task)
		if !retained {
			return linuxErrno(linuxabi.ENOMEM)
		}
		if errno = handler.copyExecSnapshot(request, &state.snapshot); errno != 0 {
			state.reset()
			return linuxErrno(errno)
		}
		path, _, resolveErrno := linuxcompat.ResolvePath(local.linuxResolved[:], compat.CWD(), state.snapshot.Command())
		if resolveErrno != 0 {
			state.reset()
			return linuxErrno(resolveErrno)
		}
		if errno = handler.vfsBegin(request, local.linuxResolved[:path], nil, uint64(slot+1)); errno != 0 {
			state.reset()
			return linuxErrno(errno)
		}
		pending, _ = linuxcompat.SyscallContinuations.Pending(handler.task)
	} else if !retained || pending.Note != uint64(slot+1) {
		handler.vfsAbandon()
		if retained {
			cancelLinuxExec(handler.task)
		}
		return linuxErrno(linuxabi.EIO)
	}
	path, _ := linuxcompat.SyscallContinuations.Paths(handler.task)
	for {
		pending, _ = linuxcompat.SyscallContinuations.Pending(handler.task)
		switch pending.Stage {
		case linuxcompat.ExecStageStat:
			reply, exchangeErrno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationStat, Path: path}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if exchangeErrno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(exchangeErrno)
			}
			if reply.Status != vfs.StatusOK || reply.Kind != ramfs.KindFile || reply.Size == 0 || reply.Size > executable.MaxSize || reply.Size > uint64(^uint32(0)) {
				handler.vfsFinish()
				errno = linuxcompat.VFSErrno(reply.Status, linuxcompat.VFSPermissionAccess, linuxabi.ENOMEM)
				if reply.Status == vfs.StatusOK {
					errno = linuxabi.EACCES
					if reply.Kind == ramfs.KindFile {
						errno = linuxabi.ENOEXEC
					}
				}
				cancelLinuxExec(handler.task)
				return linuxErrno(errno)
			}
			state.fileSize = reply.Size
			if errno = handler.vfsNext(pending.Stage, linuxcompat.ExecStageOpen, 0); errno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(errno)
			}
		case linuxcompat.ExecStageOpen:
			reply, exchangeErrno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if exchangeErrno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(exchangeErrno)
			}
			if reply.Status != vfs.StatusOK {
				handler.vfsFinish()
				cancelLinuxExec(handler.task)
				return linuxErrno(linuxcompat.VFSErrno(reply.Status, linuxcompat.VFSPermissionAccess, linuxabi.ENOMEM))
			}
			state.file = reply.Handle
			if !createLinuxExecObject(state, compat) {
				state.failure = linuxabi.ENOMEM
				if errno = handler.vfsNext(pending.Stage, linuxcompat.ExecStageClose, 0); errno != 0 {
					cancelLinuxExec(handler.task)
					return linuxErrno(errno)
				}
				continue
			}
			if errno = handler.vfsNext(pending.Stage, linuxcompat.ExecStagePrepare, 0); errno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(errno)
			}
		case linuxcompat.ExecStagePrepare:
			header := min(state.fileSize, uint64(vm.PageSize))
			reply, exchangeErrno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationPrepareExecutable, Handle: state.file, Size: uint32(header)}, attachExecutable)
			if blocked {
				return linuxBlocked
			}
			if exchangeErrno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(exchangeErrno)
			}
			next := linuxcompat.ExecStageSeek
			if reply.Status != vfs.StatusOK || reply.Size != header {
				state.failure, next = execVFSErrno(reply.Status), linuxcompat.ExecStageClose
				if reply.Status == vfs.StatusOK {
					state.failure = linuxabi.ENOEXEC
				}
			}
			if errno = handler.vfsNext(pending.Stage, next, 0); errno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(errno)
			}
		case linuxcompat.ExecStageSeek:
			header := min(state.fileSize, uint64(vm.PageSize))
			reply, exchangeErrno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationSeek, Handle: state.file, Offset: header}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if exchangeErrno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(exchangeErrno)
			}
			next := linuxcompat.ExecStageFill
			if reply.Status != vfs.StatusOK {
				state.failure, next = linuxabi.ENOEXEC, linuxcompat.ExecStageClose
			} else if state.fileSize == header {
				next = linuxcompat.ExecStageClose
			}
			if errno = handler.vfsNext(pending.Stage, next, 0); errno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(errno)
			}
		case linuxcompat.ExecStageFill:
			header := min(state.fileSize, uint64(vm.PageSize))
			reply, exchangeErrno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationFillExecutable, Handle: state.file, Size: uint32(state.fileSize - header)}, attachExecutable)
			if blocked {
				return linuxBlocked
			}
			if exchangeErrno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(exchangeErrno)
			}
			if reply.Status != vfs.StatusOK || reply.Size != state.fileSize-header {
				state.failure = execVFSErrno(reply.Status)
				if reply.Status == vfs.StatusOK {
					state.failure = linuxabi.ENOEXEC
				}
			}
			if errno = handler.vfsNext(pending.Stage, linuxcompat.ExecStageClose, 0); errno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(errno)
			}
		case linuxcompat.ExecStageClose:
			reply, exchangeErrno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationClose, Handle: state.file}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if exchangeErrno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(exchangeErrno)
			}
			if state.failure != 0 || reply.Status != vfs.StatusOK {
				handler.vfsFinish()
				if state.failure == 0 {
					state.failure = linuxabi.EIO
				}
				errno = state.failure
				cancelLinuxExec(handler.task)
				return linuxErrno(errno)
			}
			if errno = handler.prepareLinuxExec(state); errno != 0 {
				handler.vfsFinish()
				cancelLinuxExec(handler.task)
				return linuxErrno(errno)
			}
			state.closeCount = uint8(compat.CloseOnExecVFS(state.closeHandles[:]))
			if state.closeCount == 0 {
				handler.vfsFinish()
				return commitLinuxExec(handler, state, compat, state.snapshot.Command())
			}
			if errno = handler.vfsNext(pending.Stage, linuxcompat.ExecStageCloseDescriptors, 0); errno != 0 {
				cancelLinuxExec(handler.task)
				return linuxErrno(errno)
			}
		default:
			if pending.Stage < linuxcompat.ExecStageCloseDescriptors {
				handler.vfsAbandon()
				cancelLinuxExec(handler.task)
				return linuxErrno(linuxabi.EIO)
			}
			index := int(pending.Stage - linuxcompat.ExecStageCloseDescriptors)
			if index >= int(state.closeCount) {
				handler.vfsAbandon()
				cancelLinuxExec(handler.task)
				return linuxErrno(linuxabi.EIO)
			}
			_, exchangeErrno, blocked := handler.vfsExchange(vfs.Request{Operation: vfs.OperationClose, Handle: vfs.Handle(state.closeHandles[index])}, attachPaths)
			if blocked {
				return linuxBlocked
			}
			if exchangeErrno != 0 {
				return commitLinuxExec(handler, state, compat, state.snapshot.Command())
			}
			if index+1 == int(state.closeCount) {
				handler.vfsFinish()
				return commitLinuxExec(handler, state, compat, state.snapshot.Command())
			}
			if errno = handler.vfsNext(pending.Stage, pending.Stage+1, 0); errno != 0 {
				return commitLinuxExec(handler, state, compat, state.snapshot.Command())
			}
		}
	}
}

func (handler *linuxCallHandler) copyExecSnapshot(request linuxcompat.Request, snapshot *linuxcompat.ExecSnapshot) linuxabi.Errno {
	command, errno := handler.copyExecString(request.Arguments[0], limits.PathMax)
	if errno != 0 {
		return errno
	}
	if errno = snapshot.SetCommand(command); errno != 0 {
		return errno
	}
	if errno = handler.copyExecVector(request.Arguments[1], linuxcompat.MaxExecArgs, snapshot.AddArgument); errno != 0 {
		return errno
	}
	return handler.copyExecVector(request.Arguments[2], linuxcompat.MaxExecEnv, snapshot.AddEnvironment)
}

func (handler *linuxCallHandler) copyExecVector(address uint64, maximum int, appendValue func([]byte) linuxabi.Errno) linuxabi.Errno {
	local := thisCPU()
	if address == 0 {
		return 0
	}
	for index := 0; index <= maximum; index++ {
		pointerAddress := address + uint64(index)*8
		if pointerAddress < address || !vm.CopyIn(handler.root, pointerAddress, local.linuxExecPointer[:]) {
			return linuxabi.EFAULT
		}
		pointer := binary.LittleEndian.Uint64(local.linuxExecPointer[:])
		if pointer == 0 {
			return 0
		}
		if index == maximum {
			return linuxabi.E2BIG
		}
		value, errno := handler.copyExecString(pointer, len(linuxExecString))
		if errno != 0 {
			return errno
		}
		if errno = appendValue(value); errno != 0 {
			return errno
		}
	}
	return linuxabi.E2BIG
}

func (handler *linuxCallHandler) copyExecString(address uint64, maximum int) ([]byte, linuxabi.Errno) {
	if address == 0 {
		return nil, linuxabi.EFAULT
	}
	length := 0
	for length < maximum {
		chunk := int(vm.PageSize - (address+uint64(length))%vm.PageSize)
		if chunk > maximum-length {
			chunk = maximum - length
		}
		if !vm.CopyIn(handler.root, address+uint64(length), linuxExecString[length:length+chunk]) {
			return nil, linuxabi.EFAULT
		}
		for index := length; index < length+chunk; index++ {
			if linuxExecString[index] == 0 {
				return linuxExecString[:index], 0
			}
		}
		length += chunk
	}
	return nil, linuxabi.E2BIG
}

func createLinuxExecObject(state *linuxExecState, compat *linuxcompat.ProcessState) bool {
	owner, found := processRegistry.ByRoot(compat.Root)
	if !found || owner.Handles().Available() == 0 {
		return false
	}
	object, reserved := executables.Reserve(state.fileSize, uint64(owner.ID()))
	if !reserved {
		return false
	}
	state.objectID = object.ID()
	if !owner.Charge(process.ResourcePages, object.Charge()) {
		executables.Remove(object.ID())
		state.objectID = 0
		return false
	}
	for !object.Complete() {
		physical, ok := pageAllocator.Allocate()
		if ok && !vm.InitializePhysical(physical, nil) {
			pageAllocator.Free(physical)
			ok = false
		}
		if !ok || !object.AddFrame(&pageAllocator, vm.PhysicalPage, physical) {
			if ok {
				pageAllocator.Free(physical)
			}
			destroyExecutable(object.ID())
			state.objectID = 0
			return false
		}
	}
	created, inserted := owner.Handles().Insert(handle.TypeMemory, capability.New(object.ID(), capability.Write|capability.Execute|capability.Transfer))
	if !inserted {
		destroyExecutable(object.ID())
		state.objectID = 0
		return false
	}
	state.objectHandle = created
	return true
}

func lendLinuxExecObject(task sched.TaskID, owner *process.Process, rights capability.Rights) (handle.ID, bool) {
	state, _, found := findLinuxExec(task)
	if !found || owner == nil || state.objectHandle == 0 {
		return 0, false
	}
	authority, allowed := owner.Handles().Lookup(state.objectHandle, handle.TypeMemory, rights|capability.Transfer)
	derived, derivable := authority.Derive(rights | capability.Transfer)
	if !allowed || !derivable {
		return 0, false
	}
	duplicate, inserted := owner.Handles().Insert(handle.TypeMemory, derived)
	if !inserted {
		return 0, false
	}
	retainMemory(derived)
	return duplicate, true
}

func (handler *linuxCallHandler) prepareLinuxExec(state *linuxExecState) linuxabi.Errno {
	local := thisCPU()
	owner, found := processRegistry.ByTask(handler.task)
	object, objectFound := executables.Find(state.objectID)
	if !found || !objectFound || !object.Sealed() {
		return linuxabi.ENOEXEC
	}
	launchObject = object
	source, sourceOK := loader.Paged(executablePage, vm.PageSize, object.Size())
	parseStatus := linuxelf.SourceReadFailed
	if sourceOK {
		parseStatus = linuxelf.ParseInto(&linuxLaunchImage, source, linuxelf.GoOSConfig())
	}
	launchObject = nil
	if parseStatus != linuxelf.OK {
		return linuxabi.ENOEXEC
	}
	pagesBefore := pageAllocator.Statistics().FreePages
	root, created := vm.CreateUserAddressSpace(&pageAllocator, kernelPageTable)
	if !created {
		return linuxabi.ENOMEM
	}
	state.newRoot = root
	if !amd64.PrepareTaskAddressSpace(&pageAllocator, handler.task, root) {
		return linuxabi.ENOMEM
	}
	state.regions.Init(vm.UserAddressStart, vm.UserAddressLimit)
	launchObject = object
	layout, mapped := linuxelf.MapImage(&linuxLaunchImage, source, loader.Origin{Object: object.ID()}, &state.regions)
	launchObject = nil
	if !mapped {
		return linuxabi.ENOEXEC
	}
	var random *[linuxabi.RandomSize]byte
	if randomBytes(local.linuxStackRandom[:]) {
		random = &local.linuxStackRandom
	}
	stack, stackStatus := linuxabi.BuildInitialStack(linuxInitialStack[:], layout.StackTop, &linuxLaunchImage, &state.snapshot, random)
	clear(local.linuxStackRandom[:])
	brkBase, brkLimit, brkOK := linuxcompat.BrkBounds(linuxLaunchImage.LoadEnd(), vm.PageSize, vm.UserMapBase)
	if stackStatus != linuxabi.StackOK {
		return linuxabi.E2BIG
	}
	if !brkOK {
		return linuxabi.ENOEXEC
	}
	if !linuxabi.InstallInitialStack(&pageAllocator, root, stack) {
		return linuxabi.ENOMEM
	}
	used := pagesBefore - pageAllocator.Statistics().FreePages
	if used > uint64(^uint32(0)) || used != 0 && !owner.Charge(process.ResourcePages, uint32(used)) {
		return linuxabi.ENOMEM
	}
	state.newPages = uint32(used)
	state.entry, state.stack = layout.Entry, stack.Pointer
	state.brkBase, state.brkLimit = brkBase, brkLimit
	return 0
}

func commitLinuxExec(handler *linuxCallHandler, state *linuxExecState, compat *linuxcompat.ProcessState, path []byte) linuxcompat.Result {
	owner, found := processRegistry.ByTask(handler.task)
	if !found {
		panicSerial("Linux exec process disappeared at commit")
	}
	oldRoot := handler.root
	oldExecutable := owner.Executable()
	// Linux ends every other thread at this point of no return. A thread
	// running on another processor stops there before the old address space
	// goes away; none runs the old image once exec returns.
	siblings := collectSiblings(owner, handler.task)
	endOtherLinuxThreads(handler)
	if !amd64.QuiesceTasks(siblings) {
		panicSerial("Linux exec: a sibling thread did not stop")
	}
	// The ended threads belong to the old image. Their exits, delivered
	// later, neither end the process nor give it their exit code.
	for index := range siblings {
		owner.DetachTask(siblings[index])
	}
	if !amd64.ReplaceTaskAddressSpace(handler.task, state.newRoot) {
		panicSerial("prepared Linux exec task root refused at commit")
	}
	if !linuxcompat.ReplaceRoot(oldRoot, state.newRoot, state.brkBase, state.brkLimit) {
		panicSerial("prepared Linux exec state refused at commit")
	}
	releaseSharedMappings(owner)
	if !owner.ReplaceAddressSpace(process.AddressSpace{Root: state.newRoot}, &state.regions) {
		panicSerial("prepared Linux exec address space refused at commit")
	}
	amd64.SetTaskFSBase(handler.task, 0)
	if fpu, found := amd64.LinuxUserFPU(handler.task); found {
		amd64.InitialFPUArea(fpu)
	}
	compat.Signals.ResetForExec()
	compat.CloseOnExec(releaseLinuxDescriptionReference)
	releaseVforkParent(compat)
	owner.SetName(execBaseName(path))
	if object, found := executables.Find(state.objectID); found {
		object.MarkLaunched()
	}
	owner.Handles().Close(state.objectHandle)
	owner.SetExecutable(state.objectID)
	newRoot, entry, stack := state.newRoot, state.entry, state.stack
	state.newRoot, state.objectHandle, state.objectID, state.newPages = 0, 0, 0, 0
	state.reset()
	before := pageAllocator.Statistics().FreePages
	if !vm.DestroyUserAddressSpace(&pageAllocator, oldRoot) {
		addressSpaceReclaimFailures++
	} else if freed := pageAllocator.Statistics().FreePages - before; freed != 0 {
		owner.Release(process.ResourcePages, uint32(freed))
	}
	if oldExecutable != 0 {
		dropExecutable(oldExecutable)
	}
	return linuxcompat.Result{Action: linuxabi.ActionExec, Entry: entry, Stack: stack, Root: newRoot}
}

func execBaseName(path []byte) []byte {
	start := 0
	for index, character := range path {
		if character == '/' {
			start = index + 1
		}
	}
	return path[start:]
}

func execVFSErrno(status vfs.Status) linuxabi.Errno {
	if status == vfs.StatusInvalidArgument {
		return linuxabi.ENOEXEC
	}
	return linuxcompat.VFSErrno(status, linuxcompat.VFSPermissionAccess, linuxabi.ENOMEM)
}

func cancelLinuxExec(task sched.TaskID) {
	state, _, found := findLinuxExec(task)
	if !found {
		return
	}
	owner, ownerFound := processRegistry.ByTask(task)
	if state.newRoot != 0 {
		if vm.DestroyUserAddressSpace(&pageAllocator, state.newRoot) && ownerFound && state.newPages != 0 {
			owner.Release(process.ResourcePages, state.newPages)
		}
	}
	if state.objectID != 0 {
		if ownerFound && state.objectHandle != 0 {
			owner.Handles().Close(state.objectHandle)
		}
		dropExecutable(state.objectID)
	}
	state.reset()
}
