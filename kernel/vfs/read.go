package vfs

import "goos/kernel/ramfs"

// ReadPlan captures an authorized open file and its position. Only Read may
// run on a worker: BeginRead and FinishRead belong to the namespace's owner.
// The owner must finish every plan, including failed reads, and drain all
// plans before changing files, mounts, authorities or backend lifetimes.
// ReadPlan does not make an arbitrary backend safe for concurrent reads.
type ReadPlan struct {
	namespace *Namespace
	backend   Backend
	node      ramfs.NodeID
	offset    uint64
	owner     Owner
	handle    Handle
	token     uint64
}

// BeginRead reserves an open file's position without advancing it. A second
// read of that handle stays busy until the first plan has been finished.
func (namespace *Namespace) BeginRead(owner Owner, handle Handle) (ReadPlan, Status) {
	entry, status := namespace.file(owner, handle, Read)
	if status != StatusOK {
		return ReadPlan{}, status
	}
	if entry.readToken != 0 {
		return ReadPlan{}, StatusBusy
	}
	namespace.nextRead++
	if namespace.nextRead == 0 {
		namespace.nextRead++
	}
	entry.readToken = namespace.nextRead
	return ReadPlan{
		namespace: namespace,
		backend:   namespace.mounts[entry.mount].backend,
		node:      entry.node,
		offset:    entry.offset,
		owner:     owner,
		handle:    handle,
		token:     entry.readToken,
	}, StatusOK
}

// BackendIs lets an owner choose a backend's execution lane without exposing
// the open file or letting a worker access namespace state. Backend identity,
// like the mount table's backend identity, must be comparable.
func (plan ReadPlan) BackendIs(backend Backend) bool { return plan.backend == backend }

// Read copies into target using only the captured backend, node and offset.
// The worker owns target until the result is handed back to the owner.
func (plan ReadPlan) Read(target []byte) (int, Status) {
	if plan.backend == nil || plan.token == 0 {
		return 0, StatusInvalidArgument
	}
	count, status := plan.backend.Read(plan.node, plan.offset, target)
	if count < 0 || count > len(target) || uint64(count) > ^uint64(0)-plan.offset {
		return 0, StatusBackendError
	}
	return count, mapStatus(status)
}

// FinishRead releases the reservation and advances the cursor only for a
// successful read, including a short read. Partial errors retain their byte
// count in the caller's result but do not advance the cursor. A plan can be
// finished only once, even when its read moved no bytes.
func (namespace *Namespace) FinishRead(plan ReadPlan, count int, result Status) Status {
	if plan.namespace != namespace || plan.token == 0 {
		return StatusInvalidArgument
	}
	entry, status := namespace.file(plan.owner, plan.handle, 0)
	if status != StatusOK {
		return status
	}
	if entry.readToken != plan.token {
		return StatusInvalidArgument
	}
	entry.readToken = 0
	if entry.node != plan.node || entry.offset != plan.offset || namespace.mounts[entry.mount].backend != plan.backend {
		return StatusInvalidArgument
	}
	if count < 0 || uint64(count) > ^uint64(0)-plan.offset || result > lastStatus {
		return StatusBackendError
	}
	if result == StatusOK {
		entry.offset += uint64(count)
	}
	return result
}
