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
	"goos/kernel/executable"
	"goos/kernel/handle"
	"goos/kernel/lock"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/shmem"
	"goos/kernel/stream"
)

var (
	// registryLock is read far more often than written, so readers share
	// it. Each process's handle table has a lock of its own (ADR 0187).
	registryLock lock.RW

	lifecycleLock = lock.New(lock.LevelLifecycle)
	// pagingLock guards the executable page requests out to file services
	// and the correlations they carry.
	pagingLock     = lock.New(lock.LevelPaging)
	streamLock     = lock.New(lock.LevelStream)
	executableLock = lock.New(lock.LevelExecutable)
	shmemLock      = lock.New(lock.LevelShmem)
)

func init() {
	registryLock.SetLevel(lock.LevelRegistry)
}

// familyLocks guard what the processes of one Linux family share beyond the
// personality's tables (ADR 0187): the fields of their states, their open
// descriptions and directories, their VFS session, and their signals. A
// family is the processes one launch forked; no call reaches into another
// family's state. A family takes the lock its ID hashes to, so two families
// may share one, which costs only concurrency.
var familyLocks [64]lock.Spin

func init() {
	for index := range familyLocks {
		familyLocks[index].SetLevel(lock.LevelLinux)
		familyLocks[index].SetRank(uint32(index + 1))
	}
}

// familyLock returns the lock of a Linux family.
func familyLock(family uint64) *lock.Spin {
	return &familyLocks[(family*0x9e3779b97f4a7c15)>>58]
}

// consoleInputLock serializes the readers of the console input rings. Each
// ring has one producer, its interrupt, and takes one consumer at a time.
var consoleInputLock = lock.New(lock.LevelLog)

// takeConsoleInput takes one byte from an input ring.
func takeConsoleInput(input uint64) uint64 {
	consoleInputLock.Lock()
	taken := amd64.InputTake(input)
	consoleInputLock.Unlock()
	return taken
}

// installLockGuards makes each package check, in validation builds, that its
// state is used under its lock. Boot runs it before a second processor joins;
// until then boot is the only code running.
func installLockGuards() {
	process.Guard(&registryLock)
	processRegistry.GuardSpaces()
	stream.Guard(streamLock)
	executable.Guard(executableLock)
	shmem.Guard(shmemLock)
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
	table := owner.Handles()
	table.Lock().RLock()
	authority, allowed := table.Lookup(id, typeID, required)
	table.Lock().RUnlock()
	return authority, allowed
}
