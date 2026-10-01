//go:build goos_userspace

package main

// Reading a service's or an application child's program without blocking
// PID 1 (ADR 0108, ADR 0175). Once the system is up, a launch that needs a
// program from a file starts the read, the same fill a job's preparation
// uses, and returns. The main loop waits on the fill with everything else;
// when it is answered the launch runs again and takes the source. Only the
// boot, which runs before the main loop and in order, reads a service's
// program in place.

import (
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

// serviceSource is one service's program being read, or read and waiting for
// its launch.
type serviceSource struct {
	// name is copied in: the launch's own name lives in memory that is
	// reused once the launch returns.
	name       [supervisor.MaxName]byte
	nameSize   int
	fill       pendingFill
	executable supervisor.Executable
	status     supervisor.LaunchStatus
	ready      bool
}

// serviceSources has room for every service that runs from the volume.
var serviceSources [4]serviceSource

// readyExecutable is a source read ahead for the launch about to happen. It
// is taken by openExecutable for the path it was read from, and closed by
// discardReady if the launch never got that far.
var readyExecutable struct {
	path       [execproto.MaxPath]byte
	size       int
	executable supervisor.Executable
	status     supervisor.LaunchStatus
	set        bool
}

func setReady(path []byte, executable supervisor.Executable, status supervisor.LaunchStatus) {
	discardReady()
	readyExecutable.size = copy(readyExecutable.path[:], path)
	readyExecutable.executable, readyExecutable.status, readyExecutable.set = executable, status, true
}

// takeReady hands over the source read ahead for path.
func takeReady(path []byte) (supervisor.Executable, supervisor.LaunchStatus, bool) {
	if !readyExecutable.set || !equalPath(readyExecutable.path[:readyExecutable.size], path) {
		return supervisor.Executable{}, 0, false
	}
	readyExecutable.set = false
	return readyExecutable.executable, readyExecutable.status, true
}

// discardReady closes a source read ahead and not taken.
func discardReady() {
	if readyExecutable.set {
		closeExecutable(readyExecutable.executable)
		readyExecutable.set = false
	}
}

// serviceReadable decides whether a service launch can go ahead now. A
// program from the archive, or any program while the system boots, can. Once
// the system is up, a program from a file can when its read has completed;
// otherwise the read is started, the launch deferred, and false returned.
func serviceReadable(name string, path []byte) bool {
	if execproto.OriginOf(path) == execproto.OriginArchive || !supervisionComplete {
		return true
	}
	slot := serviceSlot(name)
	if slot == nil {
		return true
	}
	if slot.ready {
		setReady(path, slot.executable, slot.status)
		*slot = serviceSource{}
		return true
	}
	if slot.fill.session != 0 {
		manager.Defer(name)
		return false
	}
	pending, status := beginSource(path)
	if status != supervisor.LaunchOK {
		// The launch fails the ordinary way, and is counted.
		setReady(path, supervisor.Executable{}, status)
		return true
	}
	slot.nameSize = copy(slot.name[:], name)
	slot.fill = pending
	manager.Defer(name)
	mark("SERVICE_SOURCE_PENDING " + name + "\n")
	return false
}

// serviceSlot returns the slot of name, or a free one for it.
func serviceSlot(name string) *serviceSource {
	var free *serviceSource
	for index := range serviceSources {
		slot := &serviceSources[index]
		if slot.nameSize != 0 && equalBytes(slot.name[:slot.nameSize], name) {
			return slot
		}
		if slot.nameSize == 0 && free == nil {
			free = slot
		}
	}
	return free
}

// completeServiceFill records the VFS's answer to a service's read and lets
// the supervisor launch it again.
func completeServiceFill(slot *serviceSource) {
	pending := slot.fill
	slot.fill = pendingFill{}
	message, status := receiveNow(pending.session)
	slot.executable, slot.status = finishSource(pending, message, status)
	slot.ready = true
	manager.Proceed(slot.serviceName())
}

// serviceName is the slot's service, as the supervisor names it.
func (slot *serviceSource) serviceName() string { return bytesString(slot.name[:slot.nameSize]) }

// cancelServiceSources drops every read and every source not yet launched,
// when the storage behind them goes or the system leaves normal mode.
func cancelServiceSources() {
	for index := range serviceSources {
		slot := &serviceSources[index]
		if slot.nameSize == 0 {
			continue
		}
		if slot.fill.session != 0 {
			sys.HandleClose(slot.fill.object)
			sys.HandleClose(slot.fill.session)
		}
		if slot.ready {
			closeExecutable(slot.executable)
		}
		manager.Proceed(slot.serviceName())
		*slot = serviceSource{}
	}
}

// earliestFill returns the earliest deadline of every fill in flight, and
// whether there is one. It runs on every pass of the main loop, so it walks
// the tables itself rather than take a closure, which TinyGo would allocate.
func earliestFill() (uint64, bool) {
	earliest, pending := uint64(0), false
	for index := range execJobs {
		job := &execJobs[index]
		if !job.active {
			continue
		}
		for _, fill := range [2]*pendingFill{&job.fill, &job.launcher.fill} {
			if fill.session != 0 && (!pending || fill.deadline < earliest) {
				earliest, pending = fill.deadline, true
			}
		}
	}
	for index := range serviceSources {
		fill := &serviceSources[index].fill
		if fill.session != 0 && (!pending || fill.deadline < earliest) {
			earliest, pending = fill.deadline, true
		}
	}
	return earliest, pending
}

// expireServiceFills gives up on service and child reads the VFS has not
// answered in time.
func expireServiceFills(now uint64) bool {
	expired := false
	for index := range serviceSources {
		slot := &serviceSources[index]
		if slot.fill.session == 0 || now < slot.fill.deadline {
			continue
		}
		syslog.Warn("service read timed out", syslog.Str("service", slot.serviceName()))
		dropFill(&slot.fill)
		slot.status, slot.ready = supervisor.LaunchProcessFailed, true
		manager.Proceed(slot.serviceName())
		expired = true
	}
	for index := range execJobs {
		job := &execJobs[index]
		if job.active && job.launcher.fill.session != 0 && now >= job.launcher.fill.deadline {
			syslog.Warn("application read timed out")
			dropFill(&job.launcher.fill)
			endChildLaunch(&job.launcher, supervisor.LaunchProcessFailed)
		}
	}
	return expired
}

// dropFill abandons a fill: closing the session tells the VFS, and the object
// goes with it.
func dropFill(fill *pendingFill) {
	sys.HandleClose(fill.object)
	sys.HandleClose(fill.session)
	*fill = pendingFill{}
}
