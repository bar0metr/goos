//go:build goos_vfs_probe && goos_goroutines

package vfsservice

import (
	"sync"
	"sync/atomic"
	"time"

	"goos/kernel/vfs"
)

// ValidationReadKind selects one of the bounded acceptance-fixture gates.
// It exists only in images built with goos_vfs_probe.
type ValidationReadKind uint8

const (
	ValidationReadLocal ValidationReadKind = iota + 1
	ValidationReadPersistent
)

type validationReadArm struct {
	owner vfs.Owner
	file  vfs.Handle
	kind  ValidationReadKind
}

type validationReadGate struct {
	state *validationReadState
	kind  ValidationReadKind
}

type validationReadState struct {
	arms     [4]validationReadArm
	initOnce sync.Once

	localEntered atomic.Uint32
	localPeak    atomic.Uint32
	localRelease chan struct{}
	localOnce    sync.Once

	persistentActive atomic.Bool
	persistentTimed  atomic.Bool
	persistentDone   atomic.Bool
	persistentGate   chan struct{}
	persistentOnce   sync.Once
}

var validationReads validationReadState

// ArmValidationRead marks one already-open file for the native acceptance
// fixture. It is not a VFS protocol operation or a process capability.
func (service *Service) ArmValidationRead(owner vfs.Owner, file vfs.Handle, kind ValidationReadKind) bool {
	validationReads.initOnce.Do(func() {
		validationReads.localRelease = make(chan struct{})
		validationReads.persistentGate = make(chan struct{})
	})
	if service == nil || owner == 0 || file == 0 || kind == 0 {
		return false
	}
	for index := range validationReads.arms {
		if validationReads.arms[index].file == 0 {
			validationReads.arms[index] = validationReadArm{owner: owner, file: file, kind: kind}
			return true
		}
	}
	return false
}

func (service *Service) validationReadGate(owner vfs.Owner, file vfs.Handle, lane ReadLane) validationReadGate {
	for index := range validationReads.arms {
		arm := &validationReads.arms[index]
		if arm.owner != owner || arm.file != file {
			continue
		}
		kind := arm.kind
		*arm = validationReadArm{}
		if kind == ValidationReadLocal && lane == ReadLaneLocal || kind == ValidationReadPersistent && lane == ReadLanePersistent {
			return validationReadGate{state: &validationReads, kind: kind}
		}
		return validationReadGate{}
	}
	return validationReadGate{}
}

func (gate validationReadGate) hold() {
	if gate.state == nil {
		return
	}
	switch gate.kind {
	case ValidationReadLocal:
		entered := gate.state.localEntered.Add(1)
		for {
			peak := gate.state.localPeak.Load()
			if peak >= entered || gate.state.localPeak.CompareAndSwap(peak, entered) {
				break
			}
		}
		if entered >= 2 {
			gate.state.localOnce.Do(func() { close(gate.state.localRelease) })
		}
		select {
		case <-gate.state.localRelease:
		case <-time.After(2 * time.Second):
			gate.state.localOnce.Do(func() { close(gate.state.localRelease) })
		}
	case ValidationReadPersistent:
		gate.state.persistentActive.Store(true)
		select {
		case <-gate.state.persistentGate:
		case <-time.After(2 * time.Second):
			gate.state.persistentTimed.Store(true)
			gate.state.persistentOnce.Do(func() { close(gate.state.persistentGate) })
		}
		gate.state.persistentActive.Store(false)
		gate.state.persistentDone.Store(true)
	}
}

// ReleaseValidationPersistentRead releases the selected persistent plan only
// after the owner has observed the local completion and both barrier admissions.
func (service *Service) ReleaseValidationPersistentRead() {
	validationReads.persistentOnce.Do(func() { close(validationReads.persistentGate) })
}

// ValidationReadLocalOverlap reports whether both selected local plans entered
// workers at once.
func (service *Service) ValidationReadLocalOverlap() bool {
	return validationReads.localPeak.Load() >= 2
}

// ValidationPersistentActive reports whether the held persistent worker is in
// its test gate. The VFS owner uses it only for observation.
func (service *Service) ValidationPersistentActive() bool {
	return validationReads.persistentActive.Load()
}

// ValidationPersistentTimedOut reports that the bounded fail-safe, rather than
// the owner observation, released the selected persistent worker.
func (service *Service) ValidationPersistentTimedOut() bool {
	return validationReads.persistentTimed.Load()
}

// ValidationPersistentDone reports that the selected persistent plan left its
// gate.
func (service *Service) ValidationPersistentDone() bool { return validationReads.persistentDone.Load() }
