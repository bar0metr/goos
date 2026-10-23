package vfsservice

import (
	"goos/internal/validation"
	"goos/kernel/vfs"
)

// ReadSession identifies a session lifetime, not only its reusable owner slot.
// A driver must drain and retire its requests before closing the session.
type ReadSession struct {
	Owner      vfs.Owner
	Generation uint64
}

// ReadLane describes which existing backend owns a read. Local file reads
// may overlap. Persistent reads serialize the entire Remote.Read, including
// cache lookup, exchange and copy; serial is the safe default for a backend
// whose read concurrency is not known.
type ReadLane uint8

const (
	ReadLaneSerial ReadLane = iota
	ReadLaneLocal
	ReadLanePersistent
)

// ReadPlan is an authorized file read with an execution lane. Its Read method
// may run on a worker. Planning and finishing stay on the service's owner.
type ReadPlan struct {
	plan   vfs.ReadPlan
	lane   ReadLane
	gate   validationReadGate
	remote *Remote
}

// BeginRead reserves the file cursor. The caller must drain all plans before
// dispatching a mutation, session close, mount or authority control.
func (service *Service) BeginRead(owner vfs.Owner, file vfs.Handle) (ReadPlan, vfs.Status) {
	plan, status := service.namespace.BeginRead(owner, file)
	if status != vfs.StatusOK {
		return ReadPlan{}, status
	}
	lane := ReadLaneSerial
	switch {
	case plan.BackendIs(&service.fs), plan.BackendIs(&service.image):
		lane = ReadLaneLocal
	case plan.BackendIs(&service.persistent):
		lane = ReadLanePersistent
	}
	result := ReadPlan{plan: plan, lane: lane, gate: service.validationReadGate(owner, file, lane)}
	if validation.Enabled && lane == ReadLanePersistent {
		result.remote = &service.persistent
	}
	return result, vfs.StatusOK
}

func (plan ReadPlan) Lane() ReadLane { return plan.lane }

// Read writes only target and the backend state assigned to this plan's lane.
// It never accesses the namespace or the Service's shared reply scratch.
func (plan ReadPlan) Read(target []byte) (int, vfs.Status) {
	plan.gate.hold()
	return plan.plan.Read(target)
}

// FinishRead commits the cursor on the service's owner, not on the worker.
func (service *Service) FinishRead(plan ReadPlan, count int, status vfs.Status) vfs.Status {
	return service.namespace.FinishRead(plan.plan, count, status)
}
