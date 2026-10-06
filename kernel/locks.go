//go:build uefi

package main

// The kernel's domain locks (ADR 0184; docs/kernel/smp.md lists what each
// guards and the order they are taken in). A call takes the locks of the
// state it touches, in level order, with local interrupts disabled, and
// releases them before it returns or blocks. The wait tables, the logs, the
// page allocator, the random generator, and the scheduler lock themselves.

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	"goos/kernel/executable"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/lock"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/shmem"
	"goos/kernel/stream"
)

var (
	// registryLock and handleLock are read far more often than written, so
	// readers share them.
	registryLock lock.RW
	handleLock   lock.RW

	linuxLock      = lock.New(lock.LevelLinux)
	lifecycleLock  = lock.New(lock.LevelLifecycle)
	channelLock    = lock.New(lock.LevelChannel)
	streamLock     = lock.New(lock.LevelStream)
	executableLock = lock.New(lock.LevelExecutable)
	shmemLock      = lock.New(lock.LevelShmem)
)

func init() {
	registryLock.SetLevel(lock.LevelRegistry)
	handleLock.SetLevel(lock.LevelHandle)
}

// installLockGuards makes each package check, in validation builds, that its
// state is used under its lock. Boot runs it before a second processor joins;
// until then boot is the only code running.
func installLockGuards() {
	process.Guard(&registryLock)
	processRegistry.GuardSpaces()
	linuxcompat.Guard(&linuxLock)
	handle.Guard(&handleLock)
	ipc.Guard(&channelLock)
	stream.Guard(&streamLock)
	executable.Guard(&executableLock)
	shmem.Guard(&shmemLock)
}

// self returns the calling task's process. A task's process outlives every
// call the task makes, so no lock is needed to find it.
func (handler nativeCallHandler) self() (*process.Process, bool) {
	return ownerOf(amd64.CurrentTask(), handler.root)
}

// ownerOf returns the process task runs in when its root is root. Boot's
// self-tests call handlers from a kernel task, which belongs to no process;
// for them the process is found by its root, before a second processor
// runs.
func ownerOf(task sched.TaskID, root uint64) (*process.Process, bool) {
	owner, found := processRegistry.Owner(task)
	if !found {
		return processRegistry.ByRoot(root)
	}
	if owner.AddressSpace().Root != root {
		return nil, false
	}
	return owner, true
}

// self returns the calling task's process.
func (handler *linuxCallHandler) self() (*process.Process, bool) {
	return ownerOf(handler.task, handler.root)
}

// lookupHandle resolves one of the caller's handles under the handle lock.
func lookupHandle(owner *process.Process, id handle.ID, typeID handle.Type, required capability.Rights) (capability.Capability, bool) {
	handleLock.RLock()
	authority, allowed := owner.Handles().Lookup(id, typeID, required)
	handleLock.RUnlock()
	return authority, allowed
}
