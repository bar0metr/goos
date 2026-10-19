//go:build uefi

package main

// Lock sets (ADR 0184). A call takes the locks of the state it touches when
// it begins, in level order, and holds them until it returns or blocks: no
// object it finds through those locks can change or go away under it, and
// two calls whose sets are disjoint run at once. A task that another
// processor stops while it waits for its set gives the set up and the call
// is retried, so the stopping processor never waits on it.

import (
	"goos/kernel/arch/amd64"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/kcall"
	"goos/kernel/lock"
	"goos/kernel/process"
)

// lockSet names the domain locks a call holds.
type lockSet uint16

const (
	registryRead lockSet = 1 << iota
	registryWrite
	linuxState
	lifecycleQueue
	handleRead
	handleWrite
	streams
	sharedMemory
	spaceRead
	spaceWrite
	executableTable
	// peerTable marks, in what a call took, a second process's handle table.
	peerTable
)

// heldLocks is what a call holds: its set, the process whose handle table
// and address space the set names, and a second process whose handle table
// it also holds for writing, the peer a message's handles move to. A peer is
// named by ID and found once the registry is held.
type heldLocks struct {
	set    lockSet
	owner  *process.Process
	peerID process.ID
	peer   *process.Process
	// family is the Linux family whose lock linuxState names, or zero.
	family uint64
	taken  lockSet
}

// take takes the set in level order, the two handle tables lower rank first.
// When stop turns nonzero it releases what it took and returns false.
func (held *heldLocks) take(stop *uint64) bool {
	set := held.set
	held.taken, held.peer = 0, nil
	ok := true
	switch {
	case set&registryWrite != 0:
		ok = held.mark(registryWrite, registryLock.LockUnless(stop))
	case set&registryRead != 0:
		ok = held.mark(registryRead, registryLock.RLockUnless(stop))
	}
	if ok && held.peerID != 0 && held.taken&(registryRead|registryWrite) != 0 {
		if peer, found := processRegistry.ByID(held.peerID); found && peer != held.owner {
			held.peer = peer
		}
	}
	if ok && set&linuxState != 0 && held.family != 0 {
		ok = held.mark(linuxState, familyLock(held.family).LockUnless(stop))
	}
	if ok && set&lifecycleQueue != 0 {
		ok = held.mark(lifecycleQueue, lifecycleLock.LockUnless(stop))
	}
	if ok && set&(handleRead|handleWrite) != 0 && held.owner != nil {
		ok = held.takeTables(stop)
	}
	if ok && set&streams != 0 {
		ok = held.mark(streams, streamLock.LockUnless(stop))
	}
	if ok && set&sharedMemory != 0 {
		ok = held.mark(sharedMemory, shmemLock.LockUnless(stop))
	}
	if ok && held.owner != nil && set&spaceWrite != 0 {
		ok = held.mark(spaceWrite, held.owner.Space().LockUnless(stop))
	} else if ok && held.owner != nil && set&spaceRead != 0 {
		ok = held.mark(spaceRead, held.owner.Space().RLockUnless(stop))
	}
	if ok && set&executableTable != 0 {
		ok = held.mark(executableTable, executableLock.LockUnless(stop))
	}
	if !ok {
		held.drop()
		return false
	}
	return true
}

// mark records lock as taken when acquired is true and returns acquired.
func (held *heldLocks) mark(lock lockSet, acquired bool) bool {
	if acquired {
		held.taken |= lock
	}
	return acquired
}

// takeTables takes the owner's handle table, for writing when the set says
// so, and the peer's for writing, the lower-ranked first.
func (held *heldLocks) takeTables(stop *uint64) bool {
	own := held.owner.Handles().Lock()
	write := held.set&handleWrite != 0
	if held.peer == nil {
		if write {
			return held.mark(handleWrite, own.LockUnless(stop))
		}
		return held.mark(handleRead, own.RLockUnless(stop))
	}
	peer := held.peer.Handles().Lock()
	if peer.Rank() < own.Rank() {
		if !peer.LockUnless(stop) {
			return false
		}
		held.taken |= peerTable
		if write {
			return held.mark(handleWrite, own.LockAfter(peer, stop))
		}
		return held.mark(handleRead, own.RLockAfter(peer, stop))
	}
	if write {
		if !held.mark(handleWrite, own.LockUnless(stop)) {
			return false
		}
	} else if !held.mark(handleRead, own.RLockUnless(stop)) {
		return false
	}
	return held.mark(peerTable, peer.LockAfter(own, stop))
}

// holdsPeer reports whether the call holds peer's handle table: the
// caller's own table, or the peer's it took.
func (held *heldLocks) holdsPeer(peer *process.Process) bool {
	return peer == held.owner || peer != nil && peer == held.peer && held.taken&peerTable != 0
}

// drop releases what take took, in the reverse order.
func (held *heldLocks) drop() {
	taken := held.taken
	held.taken = 0
	if taken&executableTable != 0 {
		executableLock.Unlock()
	}
	if taken&spaceWrite != 0 {
		held.owner.Space().Unlock()
	} else if taken&spaceRead != 0 {
		held.owner.Space().RUnlock()
	}
	if taken&sharedMemory != 0 {
		shmemLock.Unlock()
	}
	if taken&streams != 0 {
		streamLock.Unlock()
	}
	// The second table taken goes first.
	own, peerFirst := (*lock.RW)(nil), false
	if held.owner != nil {
		own = held.owner.Handles().Lock()
		peerFirst = taken&peerTable != 0 && held.peer.Handles().Lock().Rank() < own.Rank()
	}
	if taken&peerTable != 0 && !peerFirst {
		held.peer.Handles().Lock().Unlock()
	}
	if taken&handleWrite != 0 {
		own.Unlock()
	} else if taken&handleRead != 0 {
		own.RUnlock()
	}
	if taken&peerTable != 0 && peerFirst {
		held.peer.Handles().Lock().Unlock()
	}
	if taken&lifecycleQueue != 0 {
		lifecycleLock.Unlock()
	}
	if taken&linuxState != 0 {
		familyLock(held.family).Unlock()
	}
	if taken&registryWrite != 0 {
		registryLock.Unlock()
	} else if taken&registryRead != 0 {
		registryLock.RUnlock()
	}
}

// takeLocks takes set for owner, which may be nil when the set names no
// handle table or address space, and never gives up.
func takeLocks(set lockSet, owner *process.Process) heldLocks {
	held := heldLocks{set: set, owner: owner}
	held.take(nil)
	return held
}

// callerSpace is the address-space lock of the calling task's process.
func callerSpace() (*process.Process, *lock.RW) {
	owner, found := processRegistry.Owner(amd64.CurrentTask())
	if !found {
		return nil, nil
	}
	return owner, owner.Space()
}

// The sets of the native calls. A call that copies to or from its caller
// reads its address space; one that maps or unmaps writes it. A call that
// can drop the last reference to an object returns the object's charge to
// whichever process paid for it, which takes the registry for reading.
const (
	callCopy    = spaceRead
	callDevice  = handleRead | spaceRead
	callMapping = registryRead | handleRead | executableTable | sharedMemory | spaceWrite
	callIPC     = registryRead | handleWrite | streams | executableTable | sharedMemory | spaceRead
	// A send takes the stream lock only for a message that moves a stream
	// handle; it asks for it the way it asks for the peer's table.
	callIPCSend    = registryRead | handleWrite | executableTable | sharedMemory | spaceRead
	callIPCReceive = registryRead | handleRead | spaceRead
	callStream     = registryRead | handleWrite | streams | spaceRead
	callMemory     = registryRead | handleWrite | executableTable | sharedMemory | spaceRead
	callExecFile   = registryRead | handleWrite | executableTable | sharedMemory | spaceRead
	callInspect    = registryRead | handleRead | spaceRead
	callEvery      = registryRead | lifecycleQueue | handleRead | streams | executableTable | sharedMemory | spaceRead
)

// nativeCallLocks is the set native operation takes.
func nativeCallLocks(operation kcall.Operation) lockSet {
	switch operation {
	case kcall.OperationExit:
		return 0
	case kcall.OperationClockRead, kcall.OperationClockReadNanos, kcall.OperationClockNow,
		kcall.OperationCPUInfo, kcall.OperationCPUEntry, kcall.OperationCPUCacheInfo, kcall.OperationCPURuntime,
		kcall.OperationStartupRead, kcall.OperationRandomRead:
		return callCopy
	case kcall.OperationConsoleWrite, kcall.OperationConsoleRead, kcall.OperationConsoleInterrupt, kcall.OperationConsoleInfo,
		kcall.OperationPortRead, kcall.OperationPortWrite, kcall.OperationLogWrite, kcall.OperationLogRead, kcall.OperationLogBounds, kcall.OperationLogReadSequence,
		kcall.OperationSystemReset, kcall.OperationSystemPowerOff, kcall.OperationPowerOffReady, kcall.OperationValidationFault, kcall.OperationClockSet,
		kcall.OperationIRQWait, kcall.OperationIRQTake:
		return callDevice
	case kcall.OperationDMAAllocate, kcall.OperationImageMap:
		return handleRead | spaceWrite
	case kcall.OperationVMQuery:
		return spaceRead
	case kcall.OperationVMMap, kcall.OperationVMUnmap, kcall.OperationVMProtect, kcall.OperationVMDiscard, kcall.OperationMemoryMap:
		return callMapping
	case kcall.OperationIPCSend:
		return callIPCSend
	case kcall.OperationIPCReceive, kcall.OperationIPCWaitReceive:
		return callIPCReceive
	case kcall.OperationChannelCreate, kcall.OperationHandleClose:
		return callIPC
	case kcall.OperationStreamCreate, kcall.OperationStreamRead, kcall.OperationStreamWrite, kcall.OperationStreamWait:
		return callStream
	case kcall.OperationMemoryCreate:
		return callMemory
	case kcall.OperationHandleDuplicate:
		return callMemory | streams
	case kcall.OperationExecutableCreate, kcall.OperationExecutableWrite, kcall.OperationExecutableSource:
		return callExecFile
	case kcall.OperationSystemInfo, kcall.OperationHandleInfo, kcall.OperationProcessInfo, kcall.OperationProcessWait,
		kcall.OperationTaskAffinity:
		return callInspect
	case kcall.OperationWaitAny:
		return registryRead | handleRead | streams | spaceRead
	case kcall.OperationProcessKill:
		return registryRead | handleRead | streams | spaceRead
	case kcall.OperationDiagnostics:
		// The dump takes the locks of each part as it prints it.
		return 0
	case kcall.OperationThreadYield:
		return 0
	case kcall.OperationThreadExit, kcall.OperationThreadResume:
		// They change the thread group, which the registry's write lock
		// guards (ADR 0203).
		return registryWrite
	case kcall.OperationThreadJoin, kcall.OperationThreadSelf, kcall.OperationThreadSuspend:
		// A suspension takes the registry for writing so that no thread is
		// created while it holds the others.
		return registryWrite | spaceRead
	case kcall.OperationThreadCreate:
		return registryWrite | spaceWrite
	}
	// An operation not named above sees everything.
	return callEvery
}

// The sets of the Linux calls (linuxCallLocks).
const (
	// linuxFast is the fast class: calls that read nothing of the Linux
	// tables but the caller's PID and TID, which the dispatcher finds first.
	linuxFast = spaceRead
	// linuxMemory changes the caller's mappings and its break.
	linuxMemory = registryRead | linuxState | sharedMemory | spaceWrite
	// linuxProcess creates, replaces, ends, waits for, or signals processes
	// and threads.
	linuxProcess = registryWrite | linuxState | lifecycleQueue | handleWrite | streams | executableTable | sharedMemory | spaceWrite
	// linuxFiles is every other call: descriptors, files through the VFS,
	// pipes, polls, signal state, and identity.
	linuxFiles = registryRead | linuxState | handleWrite | streams | executableTable | sharedMemory | spaceRead
)

// linuxCallLocks is the set a Linux call takes.
func linuxCallLocks(number linuxabi.Syscall) lockSet {
	switch number {
	case linuxabi.SysFutex, linuxabi.SysSchedYield, linuxabi.SysGetpid, linuxabi.SysGettid,
		linuxabi.SysGetuid, linuxabi.SysGeteuid, linuxabi.SysGetgid, linuxabi.SysGetegid,
		linuxabi.SysSetuid, linuxabi.SysSetgid, linuxabi.SysGetgroups, linuxabi.SysUname,
		linuxabi.SysClockGettime, linuxabi.SysGettimeofday, linuxabi.SysTime, linuxabi.SysGetrandom,
		linuxabi.SysGetcpu:
		return linuxFast
	case linuxabi.SysMmap, linuxabi.SysMunmap, linuxabi.SysMprotect, linuxabi.SysMadvise, linuxabi.SysBrk:
		return linuxMemory
	case linuxabi.SysExecve, linuxabi.SysFork, linuxabi.SysVfork, linuxabi.SysClone,
		linuxabi.SysExit, linuxabi.SysExitGroup, linuxabi.SysWait4,
		linuxabi.SysKill, linuxabi.SysTkill, linuxabi.SysTgkill:
		return linuxProcess
	}
	return linuxFiles
}
