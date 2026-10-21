//go:build goos_userspace && goos_goroutines

package main

import (
	"goos/internal/validation"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/runtime/sys"
	vfsservice "goos/userspace/vfs"
)

const (
	readPoolProbeRAMFSPath      = "/tmp/.validation-readpool-local"
	readPoolProbeWriterPath     = "/tmp/.validation-readpool-writer"
	readPoolProbePersistentPath = "/persist/.validation-readpool-slow"
	readPoolProbeDeadPath       = "/persist/.validation-readpool-dead"
	readPoolProbeImagePath      = "/bootstrap/vfs"
	// The fixture's events reach PID 1 as control notifications whose one
	// payload byte names the event: a reply would need a pending request.
	readPoolEventPersistentEntered byte = 1
	readPoolEventLocalCompleted    byte = 2
)

type readPoolProbeState struct {
	imageArms        uint8
	writer           vfs.Owner
	persistentOwner  vfs.Owner
	persistentHandle vfs.Handle
	persistentWait   bool
	persistentReady  bool
	localReady       bool
	deadOwner        vfs.Owner
	deadHandle       vfs.Handle
	deadMappings     uint8
	localCompletion  bool
	writeAdmitted    bool
	controlAdmitted  bool
	released         bool
	localMarker      bool
	persistentMarker bool
	writeMarker      bool
	controlMarker    bool
	cleanupMarker    bool
	traces           uint8
	armed            bool
	deadMapped       bool
	wire             [ipc.WireMaxSize]byte
}

var readPoolProbe readPoolProbeState

func readPoolProbeEnabled() bool { return validation.VFSReadPoolProbe }

func readPoolProbeOpen(owner vfs.Owner, request vfs.Request, reply vfs.Reply) {
	if !readPoolProbeEnabled() || request.Operation != vfs.OperationOpen || reply.Status != vfs.StatusOK {
		return
	}
	if request.Rights&vfs.Write != 0 {
		// Preparing the files opens them for writing; only the fixture's
		// read handles take one of the bounded gates.
		if string(request.Path) == readPoolProbeWriterPath {
			readPoolProbe.writer = owner
		}
		return
	}
	switch string(request.Path) {
	case readPoolProbeRAMFSPath:
		readPoolProbe.armed = true
		service.ArmValidationRead(owner, reply.Handle, vfsservice.ValidationReadLocal)
	case readPoolProbeImagePath:
		if readPoolProbe.imageArms == 0 && service.ArmValidationRead(owner, reply.Handle, vfsservice.ValidationReadLocal) {
			readPoolProbe.imageArms++
		}
	case readPoolProbePersistentPath:
		if service.ArmValidationRead(owner, reply.Handle, vfsservice.ValidationReadPersistent) {
			readPoolProbe.persistentOwner, readPoolProbe.persistentHandle = owner, reply.Handle
		}
	case readPoolProbeDeadPath:
		if service.ArmValidationRead(owner, reply.Handle, vfsservice.ValidationReadPersistent) {
			readPoolProbe.deadOwner, readPoolProbe.deadHandle = owner, reply.Handle
		}
	case readPoolProbeWriterPath:
		readPoolProbe.writer = owner
	}
}

func readPoolProbeReadAdmitted(owner vfs.Owner, file vfs.Handle) {
	readPoolProbeTrace("VFS_READPOOL_TRACE parallel\n")
	if readPoolProbeEnabled() && owner == readPoolProbe.persistentOwner && file == readPoolProbe.persistentHandle {
		readPoolProbe.persistentWait = true
	}
}

func readPoolProbeBarrierAdmitted(kind driverRequestKind, owner vfs.Owner, message ipc.Message) {
	if !readPoolProbeEnabled() {
		return
	}
	if request, ok := vfs.DecodeRequest(message); kind == driverServe && ok && (request.Operation == vfs.OperationRead || request.Operation == vfs.OperationReadInto) {
		readPoolProbeTrace("VFS_READPOOL_TRACE barrier-read\n")
	}
	switch kind {
	case driverControl:
		if vfs.IsSync(message) {
			readPoolProbe.controlAdmitted = true
		}
	case driverServe:
		request, ok := vfs.DecodeRequest(message)
		if ok && owner == readPoolProbe.writer && request.Operation == vfs.OperationWrite {
			readPoolProbe.writeAdmitted = true
		}
	}
	readPoolProbeRelease()
}

func readPoolProbeReadCompleted() {
	if !readPoolProbeEnabled() {
		return
	}
	readPoolProbeTrace([...]string{"VFS_READPOOL_TRACE complete entered=0\n", "VFS_READPOOL_TRACE complete entered=1\n", "VFS_READPOOL_TRACE complete entered=2+\n"}[min(service.ValidationLocalEntered(), 2)])
	if service.ValidationPersistentActive() {
		readPoolProbe.localCompletion = true
		if !readPoolProbe.localReady {
			readPoolProbeNotify(readPoolEventLocalCompleted)
			readPoolProbe.localReady = true
		}
	}
	readPoolProbeRelease()
}

// readPoolProbeRelease is owner-only. The persistent worker observes only its
// private gate; it never sends IPC, logs, or reaches driver state.
func readPoolProbeRelease() {
	if readPoolProbe.released || !readPoolProbe.localCompletion || !readPoolProbe.writeAdmitted || !readPoolProbe.controlAdmitted || !service.ValidationPersistentActive() {
		return
	}
	readPoolProbe.released = true
	service.ReleaseValidationPersistentRead()
}

func readPoolProbePoll() {
	if !readPoolProbeEnabled() {
		return
	}
	if readPoolProbe.persistentWait && !readPoolProbe.persistentReady && service.ValidationPersistentActive() {
		readPoolProbeNotify(readPoolEventPersistentEntered)
		readPoolProbe.persistentReady = true
		mark("VFS_READPOOL_PERSISTENT_ENTERED\n")
	}
	readPoolProbeRelease()
}

func readPoolProbePending() bool {
	if !readPoolProbeEnabled() {
		return false
	}
	if readPoolProbe.persistentWait && !readPoolProbe.persistentReady {
		return true
	}
	return !readPoolProbe.released && (readPoolProbe.writeAdmitted || readPoolProbe.controlAdmitted || readPoolProbe.localCompletion)
}

func readPoolProbeLocalCompleted() {
	if !readPoolProbeEnabled() || readPoolProbe.localMarker || !service.ValidationReadLocalOverlap() {
		return
	}
	readPoolProbe.localMarker = true
	mark("VFS_READPOOL_LOCAL_OVERLAP_OK\n")
}

func readPoolProbePersistentCompleted() {
	if !readPoolProbeEnabled() || readPoolProbe.persistentMarker || !service.ValidationPersistentDone() || service.ValidationPersistentTimedOut() || !readPoolProbe.released || !readPoolProbe.localCompletion {
		return
	}
	readPoolProbe.persistentMarker = true
	mark("VFS_READPOOL_PERSISTENT_LOCAL_OK\n")
}

func readPoolProbeBarrierCompleted(kind driverRequestKind, owner vfs.Owner, message ipc.Message) {
	if !readPoolProbeEnabled() || service.ValidationPersistentTimedOut() || !readPoolProbe.released {
		return
	}
	switch kind {
	case driverControl:
		if !readPoolProbe.controlMarker && vfs.IsSync(message) {
			readPoolProbe.controlMarker = true
			mark("VFS_READPOOL_CONTROL_BARRIER_OK\n")
		}
	case driverServe:
		request, ok := vfs.DecodeRequest(message)
		if !readPoolProbe.writeMarker && ok && owner == readPoolProbe.writer && request.Operation == vfs.OperationWrite {
			readPoolProbe.writeMarker = true
			mark("VFS_READPOOL_WRITE_BARRIER_OK\n")
		}
	}
}

func readPoolProbeMapped(owner vfs.Owner, file vfs.Handle) {
	if readPoolProbeEnabled() && owner == readPoolProbe.deadOwner && file == readPoolProbe.deadHandle {
		readPoolProbe.deadMappings++
		readPoolProbe.deadMapped = true
	}
}

func readPoolProbeUnmapped(owner vfs.Owner, file vfs.Handle) {
	if readPoolProbeEnabled() && owner == readPoolProbe.deadOwner && file == readPoolProbe.deadHandle && readPoolProbe.deadMappings != 0 {
		readPoolProbe.deadMappings--
	}
}

func readPoolProbeSessionClosed(owner vfs.Owner) {
	if !readPoolProbeEnabled() || readPoolProbe.cleanupMarker || !readPoolProbe.deadMapped || owner != readPoolProbe.deadOwner || readPoolProbe.deadMappings != 0 || service.ValidationPersistentTimedOut() {
		return
	}
	readPoolProbe.cleanupMarker = true
	mark("VFS_READPOOL_PEER_CLEANUP_OK\n")
}

// readPoolProbeTrace marks how the fixture's first reads were admitted.
func readPoolProbeTrace(message string) {
	if readPoolProbeEnabled() && readPoolProbe.armed && readPoolProbe.traces < 12 {
		readPoolProbe.traces++
		mark(message)
	}
}

// readPoolProbeNotify tells PID 1 a fixture event happened. A failure names
// its status, which the shared send helper does not.
func readPoolProbeNotify(event byte) {
	message := ipc.Message{ProtocolID: vfs.ControlProtocolID, Version: vfs.ControlVersion, Kind: ipc.KindNotification, PayloadSize: 1}
	message.Payload[0] = event
	size, ok := ipc.Encode(message, readPoolProbe.wire[:])
	if !ok {
		fatal("VFS_READPOOL_SEND_ENCODE_FAILED\n")
	}
	if status := sys.IPCSend(controlHandle, readPoolProbe.wire[:size]); status != kcall.StatusOK {
		fatal([...]string{"VFS_READPOOL_SEND_FAILED 0\n", "VFS_READPOOL_SEND_FAILED 1\n", "VFS_READPOOL_SEND_FAILED 2\n", "VFS_READPOOL_SEND_FAILED 3\n", "VFS_READPOOL_SEND_FAILED 4\n", "VFS_READPOOL_SEND_FAILED 5\n", "VFS_READPOOL_SEND_FAILED 6\n", "VFS_READPOOL_SEND_FAILED 7+\n"}[min(status, 7)])
	}
}
