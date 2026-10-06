// Package linux owns bounded per-process compatibility state that is not
// exposed in the Linux address space.
package linux

import (
	"goos/internal/limits"
	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/userspace/startup"
)

// ProcessState retains launch authority, descriptor state, and the working
// directory used by Linux syscall translation. Native handle
// IDs never enter the Linux initial stack.
type ProcessState struct {
	Root       uint64
	VFSHandle  handle.ID
	FDs        FDTable
	FamilyID   uint64
	BrkBase    uint64
	BrkCurrent uint64
	BrkLimit   uint64
	// PID is the process identity a Linux program sees and the TID of its
	// first thread; later threads have TIDs of their own. It comes from a
	// small positive range and never carries a native process or task ID.
	PID int32
	// ClearChildTID is the first thread's clear-child-TID address. When that
	// thread exits while other tasks of the process remain, the kernel zeroes
	// it and wakes its futex.
	ClearChildTID uint64
	// umask is the file mode creation mask umask sets. It is inherited across
	// fork and exec as on Linux.
	umask uint16
	// Signals holds one thread's mask at a time: that of maskTID, or of the
	// first thread when maskTID is zero. The first thread's mask waits in
	// firstMask while another thread's is in force.
	maskTID   int32
	firstMask ThreadMask
	// IOBuffer is the native handle of the memory object file reads and
	// writes move their bytes through. The bridge creates it on first use.
	IOBuffer handle.ID
	// Signals holds the dispositions and mask the process registered with
	// rt_sigaction and rt_sigprocmask, and the signals pending for it.
	Signals SignalState
	// Forked marks a process created by fork rather than launched by PID 1,
	// which holds no handle to it and hears nothing of its exit.
	Forked bool
	// vforkParent is the task a vfork suspended until this process replaces
	// its image or exits.
	vforkParent sched.TaskID
	// childWaiter is this process's task while wait4 blocks for a child.
	childWaiter sched.TaskID
	cwd         [limits.PathMax]byte
	cwdLen      uint16
}

// HoldVforkParent records the task a vfork suspended for this child.
func (state *ProcessState) HoldVforkParent(task sched.TaskID) {
	state.vforkParent = task
}

// ReleaseVforkParent returns the suspended parent task, if any, and forgets
// it, so image replacement and exit release the parent once.
func (state *ProcessState) ReleaseVforkParent() sched.TaskID {
	task := state.vforkParent
	state.vforkParent = 0
	return task
}

type StandardStreams struct {
	Input  uint64
	Output uint64
	Error  uint64
}

// ProcessLifecycle is the part of Linux process lifetime retained after the
// GoOS Process has released its address space and handles.
type ProcessLifecycle uint8

const (
	ProcessRunning ProcessLifecycle = iota + 1
	ProcessZombie
	ProcessReaped
)

// ProcessRecord connects a Linux-visible identity to a generation-safe GoOS
// Process identity. Zombie records contain no address-space or handle state.
type ProcessRecord struct {
	PID           int32
	GoOSProcessID process.ID
	ParentPID     int32
	FamilyID      uint64
	State         ProcessLifecycle
	// ExitStatus is the status of a normal exit. SignalTermination is the
	// signal that ended the process instead, or zero.
	ExitStatus        uint8
	SignalTermination int32
	// CPUTicks is the run time the process accumulated, in timer ticks.
	CPUTicks uint64
}

// WaitStatus encodes how the process ended as wait4 reports it: the exit
// status in bits 8 to 15, or the terminating signal in the low seven bits.
func (record ProcessRecord) WaitStatus() uint32 {
	if record.SignalTermination != 0 {
		return uint32(record.SignalTermination) & 0x7f
	}
	return uint32(record.ExitStatus) << 8
}

// SetCWD replaces the working directory with a canonical absolute path.
func (state *ProcessState) SetCWD(path []byte) bool {
	if state == nil || len(path) == 0 || len(path) > limits.PathMax || path[0] != '/' {
		return false
	}
	state.cwdLen = uint16(copy(state.cwd[:], path))
	return true
}

func replaceRootHeld(oldRoot uint64, newRoot uint64, brkBase uint64, brkLimit uint64) bool {
	guard.Check()
	if oldRoot == 0 || newRoot == 0 || brkBase == 0 || brkBase >= brkLimit {
		return false
	}
	state, found := findProcessHeld(oldRoot)
	if !found {
		return false
	}
	state.Root = newRoot
	state.BrkBase, state.BrkCurrent, state.BrkLimit = brkBase, brkBase, brkLimit
	state.ClearChildTID = 0
	return true
}

// DefaultUmask is the mask a process launched by PID 1 starts with, the
// usual Linux login default.
const DefaultUmask = 0o022

// SetUmask replaces the file mode creation mask with the permission bits of
// mask and returns the previous one, as umask does.
func (state *ProcessState) SetUmask(mask uint64) uint64 {
	previous := uint64(state.umask)
	state.umask = uint16(mask & 0o777)
	return previous
}

func (state *ProcessState) closeOnExecHeld(released *Released) {
	guard.Check()
	if state == nil {
		return
	}
	for fd := 0; fd < LinuxMaxFD; fd++ {
		closeOnExec, open := state.FDs.closeOnExecHeld(fd)
		if !open || !closeOnExec {
			continue
		}
		description, last, closed := state.FDs.closeHeld(fd)
		if closed && last && description.Kind == FDVFSDirectory {
			releaseDirectoryHeld(state.FamilyID, description.Object)
		} else if closed && last {
			released.add(description)
		}
	}
}

func (state *ProcessState) closeOnExecVFSHeld(target []uint64) int {
	guard.Check()
	if state == nil {
		return 0
	}
	count := 0
	for fd := 0; fd < LinuxMaxFD; fd++ {
		entry := state.FDs.entries[fd]
		if !entry.open || !entry.CloseOnExec {
			continue
		}
		duplicate := false
		marked := 0
		for other := 0; other < LinuxMaxFD; other++ {
			candidate := state.FDs.entries[other]
			if candidate.open && candidate.description == entry.description && candidate.CloseOnExec {
				marked++
				if other < fd {
					duplicate = true
				}
			}
		}
		if duplicate {
			continue
		}
		slot, valid := state.FDs.description(entry.description)
		if valid && slot.description.Kind == FDVFSFile && int(slot.description.References) == marked && count < len(target) {
			target[count] = slot.description.Object
			count++
		}
	}
	return count
}

const (
	firstPID = int32(2)
	// MaxPID matches the default Linux pid_max, so a compatibility PID fits
	// every pid_t a program may narrow it to.
	MaxPID = int32(32767)
	// NativeProcessReserve keeps Process registry slots unavailable to Linux.
	// Linux launch roots and descendants together may hold 48 of the 64 slots.
	// A family holds at most 16.
	NativeProcessReserve    = 16
	LinuxGlobalMaxProcesses = process.MaxProcesses - NativeProcessReserve
	LinuxFamilyMaxProcesses = 16
	// NativeTaskReserve leaves scheduler capacity for PID 1 and native
	// services even when Linux processes create threads.
	NativeTaskReserve = 16
)

type ChildRegistrationStatus uint8

const (
	ChildRegistrationInvalid ChildRegistrationStatus = iota
	ChildRegistrationOK
	ChildRegistrationLimit
)

var (
	nextPID      = firstPID
	nextFamilyID = uint64(1)
)

func (state *ProcessState) CWD() []byte { return state.cwd[:state.cwdLen] }

var processStates [process.MaxProcesses]ProcessState
var processRecords [process.MaxProcesses]ProcessRecord

func installProcessHeld(root uint64, goosProcessID process.ID, data startup.Data, streams StandardStreams, brkBase uint64, brkLimit uint64) bool {
	guard.Check()
	if root == 0 || goosProcessID == 0 || brkBase == 0 || brkBase >= brkLimit || brkBase%LinuxPageSize != 0 || brkLimit%LinuxPageSize != 0 ||
		data.HandleCount() != 4 || len(data.CWD()) == 0 || data.CWD()[0] != '/' || len(data.CWD()) > limits.PathMax ||
		streams.Input == 0 || streams.Output == 0 || streams.Error == 0 {
		return false
	}
	input, inputOK := data.Handle("in")
	output, outputOK := data.Handle("stdout")
	errorOutput, errorOK := data.Handle("stderr")
	vfsHandle, vfsOK := data.Handle("vfs")
	if !inputOK || !outputOK || !errorOK || !vfsOK || input == 0 || output == 0 || errorOutput == 0 || vfsHandle == 0 {
		return false
	}
	free := -1
	for index := range processStates {
		if processStates[index].Root == root {
			return false
		}
		if free == -1 && processStates[index].Root == 0 {
			free = index
		}
	}
	if free == -1 {
		return false
	}
	state := &processStates[free]
	*state = ProcessState{}
	// The inherited working directory is stored in canonical form, so every
	// later relative path starts from a base that needs no second check.
	size, _, errno := ResolvePath(state.cwd[:], nil, data.CWD())
	if errno != 0 {
		return false
	}
	record, installed := installProcessRecord(goosProcessID, 1, 0)
	if !installed {
		*state = ProcessState{}
		return false
	}
	if !installVFSContext(record.FamilyID) {
		processRecords[recordIndex(record.PID)] = ProcessRecord{}
		*state = ProcessState{}
		return false
	}
	state.Root, state.VFSHandle, state.cwdLen = root, vfsHandle, uint16(size)
	state.PID, state.FamilyID = record.PID, record.FamilyID
	state.BrkBase, state.BrkCurrent, state.BrkLimit = brkBase, brkBase, brkLimit
	state.umask = DefaultUmask
	if !state.FDs.initializeHeld(record.FamilyID, streams.Input, streams.Output, streams.Error) {
		releaseVFSContextHeld(record.FamilyID)
		processRecords[recordIndex(record.PID)] = ProcessRecord{}
		*state = ProcessState{}
		return false
	}
	return true
}

func forkProcessHeld(parentRoot uint64, childRoot uint64, record ProcessRecord, vfsHandle handle.ID) bool {
	guard.Check()
	parent, found := findProcessHeld(parentRoot)
	if !found || childRoot == 0 || vfsHandle == 0 || record.State != ProcessRunning ||
		record.ParentPID != parent.PID || record.FamilyID != parent.FamilyID || recordIndex(record.PID) < 0 {
		return false
	}
	free := -1
	for index := range processStates {
		if processStates[index].Root == childRoot {
			return false
		}
		if free == -1 && processStates[index].Root == 0 {
			free = index
		}
	}
	if free == -1 || !retainVFSContextHeld(parent.FamilyID) {
		return false
	}
	child := &processStates[free]
	*child = ProcessState{}
	if !child.FDs.inheritHeld(&parent.FDs) {
		releaseVFSContextHeld(parent.FamilyID)
		*child = ProcessState{}
		return false
	}
	child.Root, child.VFSHandle, child.FamilyID = childRoot, vfsHandle, parent.FamilyID
	child.BrkBase, child.BrkCurrent, child.BrkLimit = parent.BrkBase, parent.BrkCurrent, parent.BrkLimit
	child.PID, child.Forked = record.PID, true
	child.umask = parent.umask
	child.Signals.CopyFrom(&parent.Signals)
	child.cwdLen = uint16(copy(child.cwd[:], parent.CWD()))
	return true
}

// LinuxProcessCreationAllowed reports whether a new Linux Process may join a
// native registry that holds total processes. Every Linux creation path asks
// it: a PID 1 launch and fork, vfork, and clone. The answer is no once the
// new process would take one of the NativeProcessReserve slots, which stay
// for native processes. Native creation never asks.
func LinuxProcessCreationAllowed(total int) bool {
	return total >= 0 && total < LinuxGlobalMaxProcesses
}

// LinuxTaskCreationAllowed reports whether one Linux task may be created
// while leaving the fixed native reserve free.
func LinuxTaskCreationAllowed(free int) bool {
	return free > NativeTaskReserve && free <= sched.MaxTasks
}

func registerChildHeld(goosProcessID process.ID, parentPID int32, liveProcessCount int) (ProcessRecord, ChildRegistrationStatus) {
	guard.Check()
	parent, found := findProcessRecordHeld(parentPID)
	if !found || parent.State != ProcessRunning || goosProcessID == 0 || liveProcessCount < 0 || liveProcessCount > process.MaxProcesses {
		return ProcessRecord{}, ChildRegistrationInvalid
	}
	global, family := processCounts(parent.FamilyID)
	if !LinuxProcessCreationAllowed(liveProcessCount) || global >= LinuxGlobalMaxProcesses || family >= LinuxFamilyMaxProcesses {
		return ProcessRecord{}, ChildRegistrationLimit
	}
	record, installed := installProcessRecord(goosProcessID, parentPID, parent.FamilyID)
	if !installed {
		return ProcessRecord{}, ChildRegistrationInvalid
	}
	return record, ChildRegistrationOK
}

func processCountsHeld(familyID uint64) (global int, family int) {
	guard.Check()
	return processCounts(familyID)
}

func processCounts(familyID uint64) (global int, family int) {
	for index := range processRecords {
		record := processRecords[index]
		if record.State == 0 {
			continue
		}
		global++
		if familyID != 0 && record.FamilyID == familyID {
			family++
		}
	}
	return global, family
}

func rollbackProcessRecordHeld(goosProcessID process.ID) bool {
	guard.Check()
	for index := range processRecords {
		record := &processRecords[index]
		if record.GoOSProcessID != goosProcessID || record.State != ProcessRunning {
			continue
		}
		for childIndex := range processRecords {
			if processRecords[childIndex].State != 0 && processRecords[childIndex].ParentPID == record.PID {
				return false
			}
		}
		*record = ProcessRecord{}
		return true
	}
	return false
}

func installProcessRecord(goosProcessID process.ID, parentPID int32, familyID uint64) (ProcessRecord, bool) {
	if goosProcessID == 0 || parentPID < 1 {
		return ProcessRecord{}, false
	}
	free := -1
	for index := range processRecords {
		if processRecords[index].GoOSProcessID == goosProcessID {
			return ProcessRecord{}, false
		}
		if free == -1 && processRecords[index].State == 0 {
			free = index
		}
	}
	if free == -1 {
		return ProcessRecord{}, false
	}
	pid := allocatePID()
	if familyID == 0 {
		familyID = allocateFamilyID()
	}
	processRecords[free] = ProcessRecord{
		PID:           pid,
		GoOSProcessID: goosProcessID,
		ParentPID:     parentPID,
		FamilyID:      familyID,
		State:         ProcessRunning,
	}
	return processRecords[free], true
}

func allocateFamilyID() uint64 {
	for {
		candidate := nextFamilyID
		nextFamilyID++
		if nextFamilyID == 0 {
			nextFamilyID = 1
		}
		used := false
		for index := range processRecords {
			if processRecords[index].State != 0 && processRecords[index].FamilyID == candidate {
				used = true
				break
			}
		}
		if !used {
			return candidate
		}
	}
}

// allocatePID returns the next identity no live Linux process holds. The
// table has fewer slots than the range has values, so the scan terminates.
func allocatePID() int32 {
	for {
		candidate := nextPID
		nextPID++
		if nextPID > MaxPID {
			nextPID = firstPID
		}
		used := tidInUse(candidate)
		for index := range processRecords {
			if processRecords[index].State != 0 && processRecords[index].PID == candidate {
				used = true
				break
			}
		}
		if !used {
			return candidate
		}
	}
}

func findProcessRecordHeld(pid int32) (ProcessRecord, bool) {
	guard.Check()
	if pid >= firstPID {
		for index := range processRecords {
			if processRecords[index].State != 0 && processRecords[index].PID == pid {
				return processRecords[index], true
			}
		}
	}
	return ProcessRecord{}, false
}

func parentPIDOfHeld(pid int32) int32 {
	guard.Check()
	record, found := findProcessRecordHeld(pid)
	if !found {
		return 1
	}
	return record.ParentPID
}

func reapChildHeld(parentPID int32, childPID int32) (ProcessRecord, bool) {
	guard.Check()
	for index := range processRecords {
		record := &processRecords[index]
		if record.PID != childPID || record.ParentPID != parentPID || record.State != ProcessZombie {
			continue
		}
		reaped := *record
		reaped.State = ProcessReaped
		*record = ProcessRecord{}
		return reaped, true
	}
	return ProcessRecord{}, false
}

// markProcessExited retains only wait-visible state, reparents live children,
// and removes zombies whose only reaper is conceptual PID 1 or whose parent
// asked for no zombies with SIG_IGN or SA_NOCLDWAIT on SIGCHLD.
func markProcessExited(pid int32, termination Termination) bool {
	index := recordIndex(pid)
	if index < 0 || processRecords[index].State != ProcessRunning {
		return false
	}
	processRecords[index].State = ProcessZombie
	processRecords[index].ExitStatus = termination.Status
	processRecords[index].SignalTermination = termination.Signal
	processRecords[index].CPUTicks = termination.CPUTicks
	for childIndex := range processRecords {
		child := &processRecords[childIndex]
		if child.State == 0 || child.ParentPID != pid {
			continue
		}
		if child.State == ProcessZombie {
			*child = ProcessRecord{}
		} else {
			child.ParentPID = 1
		}
	}
	if processRecords[index].ParentPID == 1 {
		processRecords[index] = ProcessRecord{}
	} else if parent, found := findProcessByPIDHeld(processRecords[index].ParentPID); found && parent.Signals.ReapsChildren() {
		processRecords[index] = ProcessRecord{}
	}
	return true
}

func recordIndex(pid int32) int {
	for index := range processRecords {
		if processRecords[index].State != 0 && processRecords[index].PID == pid {
			return index
		}
	}
	return -1
}

func lookupFDHeld(root uint64, fd int) (FileDescription, bool) {
	guard.Check()
	if root != 0 {
		for index := range processStates {
			if processStates[index].Root == root {
				return processStates[index].FDs.lookupHeld(fd)
			}
		}
	}
	return FileDescription{}, false
}

func allocateFDHeld(root uint64, description FileDescription, closeOnExec bool) (int, abi.Errno) {
	guard.Check()
	if root != 0 {
		for index := range processStates {
			if processStates[index].Root != root {
				continue
			}
			if fd, ok := processStates[index].FDs.allocateHeld(description, closeOnExec); ok {
				return fd, 0
			}
			return -1, abi.EMFILE
		}
	}
	return -1, abi.EBADF
}

func closeFDHeld(root uint64, fd int) (FileDescription, bool, abi.Errno) {
	guard.Check()
	if state, found := findProcessHeld(root); found {
		if released, last, ok := state.FDs.closeHeld(fd); ok {
			return released, last, 0
		}
	}
	return FileDescription{}, false, abi.EBADF
}

func descriptionOfHeld(root uint64, fd uint64) (*FileDescription, bool) {
	guard.Check()
	if state, found := findProcessHeld(root); found && fd < LinuxMaxFD {
		return state.FDs.descriptionHeld(int(fd))
	}
	return nil, false
}

func familyRootsHeld(familyID uint64, target []uint64) int {
	guard.Check()
	count := 0
	for index := range processStates {
		if familyID != 0 && processStates[index].Root != 0 && processStates[index].FamilyID == familyID && count < len(target) {
			target[count] = processStates[index].Root
			count++
		}
	}
	return count
}

func findProcessByPIDHeld(pid int32) (*ProcessState, bool) {
	guard.Check()
	if pid >= firstPID {
		for index := range processStates {
			if processStates[index].Root != 0 && processStates[index].PID == pid {
				return &processStates[index], true
			}
		}
	}
	return nil, false
}

func findProcessHeld(root uint64) (*ProcessState, bool) {
	guard.Check()
	if root != 0 {
		for index := range processStates {
			if processStates[index].Root == root {
				return &processStates[index], true
			}
		}
	}
	return nil, false
}

func removeProcessHeld(root uint64, released *Released) bool {
	guard.Check()
	if root != 0 {
		for index := range processStates {
			if processStates[index].Root == root {
				pid := processStates[index].PID
				familyID := processStates[index].FamilyID
				closeProcessFDs(&processStates[index], released)
				if final, _ := releaseVFSContextHeld(familyID); final {
					releaseDirectories(familyID)
				}
				processStates[index] = ProcessState{}
				forgetThreads(root)
				if record := recordIndex(pid); record >= 0 {
					processRecords[record] = ProcessRecord{}
				}
				return true
			}
		}
	}
	return false
}

func exitProcessHeld(root uint64, termination Termination, released *Released) bool {
	guard.Check()
	if root == 0 {
		return false
	}
	for index := range processStates {
		if processStates[index].Root != root {
			continue
		}
		pid := processStates[index].PID
		familyID := processStates[index].FamilyID
		closeProcessFDs(&processStates[index], released)
		if final, _ := releaseVFSContextHeld(familyID); final {
			releaseDirectories(familyID)
		}
		processStates[index] = ProcessState{}
		forgetThreads(root)
		return markProcessExited(pid, termination)
	}
	return false
}

func closeProcessFDs(state *ProcessState, released *Released) {
	if state == nil {
		return
	}
	for fd := 0; fd < LinuxMaxFD; fd++ {
		description, last, closed := state.FDs.closeHeld(fd)
		if closed && last && description.Kind == FDVFSDirectory {
			releaseDirectoryHeld(state.FamilyID, description.Object)
		} else if closed && last {
			released.add(description)
		}
	}
}

// ReplaceRoot commits a prepared address space while retaining process
// identity, descriptors, working directory, and family membership.
func ReplaceRoot(oldRoot uint64, newRoot uint64, brkBase uint64, brkLimit uint64) bool {
	tablesLock.Lock()
	result := replaceRootHeld(oldRoot, newRoot, brkBase, brkLimit)
	tablesLock.Unlock()
	return result
}

// CloseOnExec closes local fd entries marked for image replacement. Shared
// descriptions remain alive while another fd table references them.
func (state *ProcessState) CloseOnExec(released *Released) {
	tablesLock.Lock()
	state.closeOnExecHeld(released)
	tablesLock.Unlock()
}

// CloseOnExecVFS returns VFS handles whose final description reference will
// disappear at image replacement. The caller closes them through the shared
// VFS context before removing the local descriptor entries.
func (state *ProcessState) CloseOnExecVFS(target []uint64) int {
	tablesLock.Lock()
	result := state.closeOnExecVFSHeld(target)
	tablesLock.Unlock()
	return result
}

// InstallProcess records one validated, bound Linux launch. The caller owns
// teardown if a later launch step fails.
func InstallProcess(root uint64, goosProcessID process.ID, data startup.Data, streams StandardStreams, brkBase uint64, brkLimit uint64) bool {
	tablesLock.Lock()
	result := installProcessHeld(root, goosProcessID, data, streams, brkBase, brkLimit)
	tablesLock.Unlock()
	return result
}

// ForkProcess installs the compatibility state of a child registered by
// RegisterChild. The child shares its parent's open-file descriptions and
// family VFS context and copies everything else. It inherits no pending
// signal, syscall continuation, or clear-child-tid address. The caller owns
// rollback through RemoveProcess if a later step fails.
func ForkProcess(parentRoot uint64, childRoot uint64, record ProcessRecord, vfsHandle handle.ID) bool {
	tablesLock.Lock()
	result := forkProcessHeld(parentRoot, childRoot, record, vfsHandle)
	tablesLock.Unlock()
	return result
}

// RegisterChild reserves an identity for a compatibility-created GoOS
// Process. The caller installs the child's heavy compatibility state before
// making its task runnable.
func RegisterChild(goosProcessID process.ID, parentPID int32, liveProcessCount int) (ProcessRecord, ChildRegistrationStatus) {
	tablesLock.Lock()
	result0, result1 := registerChildHeld(goosProcessID, parentPID, liveProcessCount)
	tablesLock.Unlock()
	return result0, result1
}

// ProcessCounts reports all compatibility records and the records in one
// family. Running and zombie records consume both budgets.
func ProcessCounts(familyID uint64) (global int, family int) {
	tablesLock.Lock()
	result0, result1 := processCountsHeld(familyID)
	tablesLock.Unlock()
	return result0, result1
}

// RollbackProcessRecord removes a child reservation that never became
// runnable. It refuses records that have already exited or gained children.
func RollbackProcessRecord(goosProcessID process.ID) bool {
	tablesLock.Lock()
	result := rollbackProcessRecordHeld(goosProcessID)
	tablesLock.Unlock()
	return result
}

// FindProcessRecord returns a copy so callers cannot mutate family state
// without applying its lifecycle rules.
func FindProcessRecord(pid int32) (ProcessRecord, bool) {
	tablesLock.Lock()
	result0, result1 := findProcessRecordHeld(pid)
	tablesLock.Unlock()
	return result0, result1
}

// ParentPIDOf returns the compatibility parent. Unknown identities use PID 1,
// which matches Linux orphan semantics and the launch-root relationship.
func ParentPIDOf(pid int32) int32 {
	tablesLock.Lock()
	result := parentPIDOfHeld(pid)
	tablesLock.Unlock()
	return result
}

// ReapChild consumes one zombie owned by parentPID. The returned copy records
// the final reaped state while the registry releases the PID immediately.
func ReapChild(parentPID int32, childPID int32) (ProcessRecord, bool) {
	tablesLock.Lock()
	result0, result1 := reapChildHeld(parentPID, childPID)
	tablesLock.Unlock()
	return result0, result1
}

func LookupFD(root uint64, fd int) (FileDescription, bool) {
	tablesLock.Lock()
	result0, result1 := lookupFDHeld(root, fd)
	tablesLock.Unlock()
	return result0, result1
}

func AllocateFD(root uint64, description FileDescription, closeOnExec bool) (int, abi.Errno) {
	tablesLock.Lock()
	result0, result1 := allocateFDHeld(root, description, closeOnExec)
	tablesLock.Unlock()
	return result0, result1
}

// CloseFD invalidates one descriptor. It reports the description when the
// last descriptor naming it closed, so the caller can release what it held.
func CloseFD(root uint64, fd int) (FileDescription, bool, abi.Errno) {
	tablesLock.Lock()
	result0, result1, result2 := closeFDHeld(root, fd)
	tablesLock.Unlock()
	return result0, result1, result2
}

// DescriptionOf returns the open-file description behind a descriptor.
func DescriptionOf(root uint64, fd uint64) (*FileDescription, bool) {
	tablesLock.Lock()
	result0, result1 := descriptionOfHeld(root, fd)
	tablesLock.Unlock()
	return result0, result1
}

// FamilyRoots writes the address-space roots of the running members of a
// family to target and returns how many it wrote.
func FamilyRoots(familyID uint64, target []uint64) int {
	tablesLock.Lock()
	result := familyRootsHeld(familyID, target)
	tablesLock.Unlock()
	return result
}

// FindProcessByPID returns the running process with a compatibility PID.
func FindProcessByPID(pid int32) (*ProcessState, bool) {
	tablesLock.Lock()
	result0, result1 := findProcessByPIDHeld(pid)
	tablesLock.Unlock()
	return result0, result1
}

func FindProcess(root uint64) (*ProcessState, bool) {
	tablesLock.Lock()
	result0, result1 := findProcessHeld(root)
	tablesLock.Unlock()
	return result0, result1
}

// RemoveProcess rolls back compatibility state for a launch that never made
// its task runnable.
func RemoveProcess(root uint64, released *Released) bool {
	tablesLock.Lock()
	result := removeProcessHeld(root, released)
	tablesLock.Unlock()
	return result
}

// ExitProcess releases heavy compatibility state and records the Linux exit
// for its parent. Native resource teardown remains in the kernel lifecycle.
func ExitProcess(root uint64, termination Termination, released *Released) bool {
	tablesLock.Lock()
	result := exitProcessHeld(root, termination, released)
	tablesLock.Unlock()
	return result
}
