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
	channels
	streams
	sharedMemory
	spaceRead
	spaceWrite
	executableTable
)

// takeLocks takes set in level order; space is the address-space lock the
// set names, if any. When stop turns nonzero it releases what it took and
// returns false.
func takeLocks(set lockSet, space *lock.RW, stop *uint64) bool {
	taken := lockSet(0)
	ok := true
	switch {
	case set&registryWrite != 0:
		ok = registryLock.LockUnless(stop)
		taken |= registryWrite
	case set&registryRead != 0:
		ok = registryLock.RLockUnless(stop)
		taken |= registryRead
	}
	if ok && set&linuxState != 0 {
		ok = linuxLock.LockUnless(stop)
		taken |= linuxState
	}
	if ok && set&lifecycleQueue != 0 {
		ok = lifecycleLock.LockUnless(stop)
		taken |= lifecycleQueue
	}
	if ok && set&handleWrite != 0 {
		ok = handleLock.LockUnless(stop)
		taken |= handleWrite
	} else if ok && set&handleRead != 0 {
		ok = handleLock.RLockUnless(stop)
		taken |= handleRead
	}
	if ok && set&channels != 0 {
		ok = channelLock.LockUnless(stop)
		taken |= channels
	}
	if ok && set&streams != 0 {
		ok = streamLock.LockUnless(stop)
		taken |= streams
	}
	if ok && set&sharedMemory != 0 {
		ok = shmemLock.LockUnless(stop)
		taken |= sharedMemory
	}
	if ok && space != nil && set&spaceWrite != 0 {
		ok = space.LockUnless(stop)
		taken |= spaceWrite
	} else if ok && space != nil && set&spaceRead != 0 {
		ok = space.RLockUnless(stop)
		taken |= spaceRead
	}
	if ok && set&executableTable != 0 {
		ok = executableLock.LockUnless(stop)
		taken |= executableTable
	}
	if !ok {
		// The last lock in taken is the one that was not acquired.
		dropLocks(taken&^highest(taken), space)
		return false
	}
	return true
}

// highest returns the last lock takeLocks attempts of those in set.
func highest(set lockSet) lockSet {
	for bit := executableTable; bit != 0; bit >>= 1 {
		if set&bit != 0 {
			return bit
		}
	}
	return 0
}

// dropLocks releases set in the reverse of the order takeLocks took it.
func dropLocks(set lockSet, space *lock.RW) {
	if set&executableTable != 0 {
		executableLock.Unlock()
	}
	if space != nil {
		if set&spaceWrite != 0 {
			space.Unlock()
		} else if set&spaceRead != 0 {
			space.RUnlock()
		}
	}
	if set&sharedMemory != 0 {
		shmemLock.Unlock()
	}
	if set&streams != 0 {
		streamLock.Unlock()
	}
	if set&channels != 0 {
		channelLock.Unlock()
	}
	if set&handleWrite != 0 {
		handleLock.Unlock()
	} else if set&handleRead != 0 {
		handleLock.RUnlock()
	}
	if set&lifecycleQueue != 0 {
		lifecycleLock.Unlock()
	}
	if set&linuxState != 0 {
		linuxLock.Unlock()
	}
	if set&registryWrite != 0 {
		registryLock.Unlock()
	} else if set&registryRead != 0 {
		registryLock.RUnlock()
	}
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
	callCopy     = spaceRead
	callDevice   = handleRead | spaceRead
	callMapping  = registryRead | handleRead | executableTable | sharedMemory | spaceWrite
	callIPC      = registryRead | handleWrite | channels | streams | executableTable | sharedMemory | spaceRead
	callStream   = registryRead | handleWrite | streams | spaceRead
	callMemory   = registryRead | handleWrite | executableTable | sharedMemory | spaceRead
	callExecFile = registryRead | handleWrite | channels | executableTable | sharedMemory | spaceRead
	callInspect  = registryRead | handleRead | spaceRead
	callEvery    = registryRead | linuxState | lifecycleQueue | handleRead | channels | streams | executableTable | sharedMemory | spaceRead
)

// nativeCallLocks is the set native operation takes.
func nativeCallLocks(operation kcall.Operation) lockSet {
	switch operation {
	case kcall.OperationExit:
		return 0
	case kcall.OperationClockRead, kcall.OperationClockReadNanos, kcall.OperationClockNow,
		kcall.OperationCPUInfo, kcall.OperationCPUEntry, kcall.OperationCPUCacheInfo, kcall.OperationStartupRead:
		return callCopy
	case kcall.OperationConsoleWrite, kcall.OperationConsoleRead, kcall.OperationConsoleInterrupt, kcall.OperationConsoleInfo,
		kcall.OperationPortRead, kcall.OperationPortWrite, kcall.OperationLogWrite, kcall.OperationLogRead, kcall.OperationLogBounds,
		kcall.OperationSystemReset, kcall.OperationSystemPowerOff, kcall.OperationValidationFault, kcall.OperationClockSet,
		kcall.OperationIRQWait:
		return callDevice
	case kcall.OperationDMAAllocate, kcall.OperationImageMap:
		return handleRead | spaceWrite
	case kcall.OperationVMQuery:
		return spaceRead
	case kcall.OperationVMMap, kcall.OperationVMUnmap, kcall.OperationVMProtect, kcall.OperationVMDiscard, kcall.OperationMemoryMap:
		return callMapping
	case kcall.OperationIPCSend, kcall.OperationIPCReceive, kcall.OperationIPCWaitReceive,
		kcall.OperationChannelCreate, kcall.OperationHandleClose:
		return callIPC
	case kcall.OperationStreamCreate, kcall.OperationStreamRead, kcall.OperationStreamWrite, kcall.OperationStreamWait:
		return callStream
	case kcall.OperationMemoryCreate, kcall.OperationHandleDuplicate:
		return callMemory
	case kcall.OperationExecutableCreate, kcall.OperationExecutableWrite, kcall.OperationExecutableSource:
		return callExecFile
	case kcall.OperationSystemInfo, kcall.OperationHandleInfo, kcall.OperationProcessInfo, kcall.OperationProcessWait:
		return callInspect
	case kcall.OperationWaitAny:
		return registryRead | handleRead | channels | streams | spaceRead
	case kcall.OperationProcessKill:
		return registryRead | linuxState | handleRead | channels | streams | spaceRead
	}
	// Diagnostics, and any operation not named above, see everything.
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
	linuxProcess = registryWrite | linuxState | lifecycleQueue | handleWrite | channels | streams | executableTable | sharedMemory | spaceWrite
	// linuxFiles is every other call: descriptors, files through the VFS,
	// pipes, polls, signal state, and identity.
	linuxFiles = registryRead | linuxState | handleWrite | channels | streams | executableTable | sharedMemory | spaceRead
)

// linuxCallLocks is the set a Linux call takes.
func linuxCallLocks(number linuxabi.Syscall) lockSet {
	switch number {
	case linuxabi.SysFutex, linuxabi.SysSchedYield, linuxabi.SysGetpid, linuxabi.SysGettid,
		linuxabi.SysGetuid, linuxabi.SysGeteuid, linuxabi.SysGetgid, linuxabi.SysGetegid,
		linuxabi.SysSetuid, linuxabi.SysSetgid, linuxabi.SysGetgroups, linuxabi.SysUname,
		linuxabi.SysClockGettime, linuxabi.SysGettimeofday, linuxabi.SysTime, linuxabi.SysGetrandom,
		linuxabi.SysSchedGetaffinity, linuxabi.SysGetcpu:
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
