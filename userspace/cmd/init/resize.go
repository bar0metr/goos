//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/remoteapi"
	"goos/userspace/runtime/sys"
	"goos/userspace/tty"
)

// PID 1 keeps both ends to replace unread notices with the newest snapshot.
// The child gets a Read|Wait stream copy. Native stream references survive
// another holder closing the same receive end.
type resizeDelivery struct{ pair [2]uint64 }

var launchingResize *resizeDelivery
var launchingResizeSession *terminalSession
var resizeWire [tty.ResizeFrameSize]byte

func closeResize(delivery *resizeDelivery) {
	closePair(delivery.pair)
	delivery.pair = [2]uint64{}
}

func sendResize(delivery *resizeDelivery, snapshot *tty.Snapshot) {
	if delivery.pair[1] == 0 {
		return
	}
	geometry, valid := snapshot.Current()
	if !valid {
		return
	}
	// Both readers consume one atomic frame. A receive racing this drain
	// may take an older sequence; the child ignores it after a newer one.
	for budget := 0; budget < 8; budget++ {
		if _, status := sys.StreamRead(handle.ID(delivery.pair[0]), resizeWire[:]); status != kcall.StatusOK {
			break
		}
	}
	if !tty.EncodeResizeFrame(geometry, resizeWire[:]) {
		return
	}
	count, status := sys.StreamWrite(handle.ID(delivery.pair[1]), resizeWire[:])
	if status != kcall.StatusOK || count != len(resizeWire) {
		closeResize(delivery)
	}
}

func updateSessionGeometry(session *terminalSession, geometry tty.Geometry) {
	if !session.geometry.Apply(geometry) {
		return
	}
	for index := range execJobs {
		job := &execJobs[index]
		if !job.active || job.session != session || job.resizeReleased {
			continue
		}
		if job.linuxTerminal != 0 {
			sendLinuxGeometry(job.linuxTerminal, &session.geometry)
			continue
		}
		if session.resizeOwner.Allows(uint64(job.slot)+1, true) {
			sendResize(&job.launcher.resize, &session.geometry)
		} else if session.resizeOwner.Allows(uint64(job.slot)+1, false) {
			sendResize(&job.resize, &session.geometry)
		}
	}
}

func remoteGeometry(session *terminalSession, geometry remoteapi.Geometry) {
	updateSessionGeometry(session, tty.Geometry{Columns: int(geometry.Columns), Rows: int(geometry.Rows),
		Authoritative: true, Sequence: geometry.Sequence, Term: geometry.Term[:geometry.TermSize]})
}

func releaseSessionResize(session *terminalSession) {
	for index := range execJobs {
		job := &execJobs[index]
		if job.active && job.session == session {
			session.resizeOwner.Release(uint64(job.slot) + 1)
			job.resizeReleased = true
			closeResize(&job.resize)
			closeResize(&job.launcher.resize)
		}
	}
}

func restoreParentResize(job *execJob) {
	if job.resizeReleased || job.launcher.server == 0 || !job.resizeParent {
		return
	}
	if !job.session.resizeOwner.Restore(uint64(job.slot) + 1) {
		return
	}
	var pair [2]uint64
	if !createStream(&pair) {
		return
	}
	endpoint, status := sys.HandleDuplicate(handle.ID(pair[0]), capability.Read|capability.Wait|capability.Transfer)
	if status != kcall.StatusOK {
		closePair(pair)
		return
	}
	message, ok := tty.ResizeEndpointNotice(endpoint)
	if !ok || !sendMessage(uint64(job.launcher.server), message) {
		sys.HandleClose(endpoint)
		closePair(pair)
		return
	}
	job.resize.pair = pair
	sendResize(&job.resize, &job.session.geometry)
}

func resizeSessionAvailable(session *terminalSession) bool {
	if session.console != 0 {
		return true
	}
	geometry, valid := session.geometry.Current()
	return valid && geometry.Authoritative && geometry.ValidRemote()
}
