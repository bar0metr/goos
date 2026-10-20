//go:build goos_userspace

package main

// Programs read from a file are prepared without blocking PID 1 (ADR 0108).
// For a stage outside the boot archive, PID 1 opens the file and asks VFS to
// pin it and return its header page. The resulting executable source owns
// that session. A job's preparation stays asynchronous, so PID 1 continues
// serving lifecycle events, other jobs, restarts, and shutdown. A service and
// an application's child are prepared the same way, waiting for the answer
// under the same bounded deadline (ADR 0175).

import (
	"goos/internal/capability"
	"goos/kernel/executable"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

// prepared is one stage's program as its preparation left it.
type prepared struct {
	executable supervisor.Executable
	status     supervisor.LaunchStatus
	ready      bool
	consumed   bool
}

// pendingFill is the one fill a job has in flight.
type pendingFill struct {
	session     handle.ID
	file        vfs.Handle
	object      handle.ID
	size        uint64
	correlation uint64
	deadline    uint64
	format      supervisor.ExecutableFormat
}

// needsPreparation reports whether a stage names a program to be read from a
// file: anything outside the boot archive except what the launch refuses
// anyway, the services of the installed system.
func needsPreparation(stage execproto.Stage) bool {
	if execproto.OriginOf(stage.Path) == execproto.OriginArchive {
		return false
	}
	_, command := execproto.CommandFromSystemPath(stage.Path)
	return !execproto.InSystem(stage.Path) || command || execproto.LinuxSystemPath(stage.Path)
}

// beginJob starts a decoded job: its programs' preparation if any stage
// needs one, its pipeline otherwise.
func beginJob(job *execJob) {
	job.preparing, job.cancelled, job.nextStage = true, false, 0
	job.prepared = [execproto.MaxStages]prepared{}
	advancePreparation(job)
}

// advancePreparation starts the next stage's fill, or the pipeline once none
// is left. A stage whose file cannot be opened records why and is skipped, so
// the pipeline reports it the ordinary way.
func advancePreparation(job *execJob) {
	for job.nextStage < job.request.Count {
		index := job.nextStage
		if job.cancelled || !needsPreparation(job.request.Stages[index]) {
			job.nextStage++
			continue
		}
		if startFill(job, index) {
			return
		}
		job.nextStage++
	}
	job.preparing = false
	if job.cancelled {
		releasePrepared(job)
		// Nothing was started, so the job ends here, as a stopped one does.
		job.active = false
		if !job.orphaned {
			sendExecReply(job.session, job.correlation, 0, uint64(supervisor.LaunchStopped), 0, 0)
		}
		job.orphaned = false
		return
	}
	startPipeline(job)
}

// startFill opens stage index's file and asks the VFS to fill a new object
// with it. It reports false when it recorded a result instead of starting.
func startFill(job *execJob, index uint8) bool {
	entry := &job.prepared[index]
	pending, status := beginSource(job.request.Stages[index].Path)
	if status != supervisor.LaunchOK {
		entry.ready, entry.status = true, status
		return false
	}
	entry.ready = false
	job.fill = pending
	return true
}

// beginSource opens the file at path, reads its header, and asks the VFS to
// fill a new header object with the first page and pin the file. It returns
// the fill to wait for, or why none was started; nothing is left open then.
func beginSource(path []byte) (pendingFill, supervisor.LaunchStatus) {
	if execproto.InSystem(path) && !systemReady {
		// The installed system is not published, or is being attached
		// again after a restart: nothing is read from it until it is.
		return pendingFill{}, supervisor.LaunchSystemUnavailable
	}
	session, reason := openVFSSession()
	if reason != "" {
		syslog.Warn("executable read failed", syslog.Str("reason", reason))
		return pendingFill{}, supervisor.LaunchProcessFailed
	}
	stat, ok := callSession(session, vfs.Request{Operation: vfs.OperationStat, Path: path})
	switch {
	case !ok:
		sys.HandleClose(session)
		return pendingFill{}, supervisor.LaunchProcessFailed
	case stat.Status == vfs.StatusNotFound:
		sys.HandleClose(session)
		return pendingFill{}, supervisor.LaunchNotFound
	case stat.Status != vfs.StatusOK || stat.Kind != ramfs.KindFile || stat.Size == 0 || stat.Size > executable.MaxSize:
		sys.HandleClose(session)
		return pendingFill{}, supervisor.LaunchInvalidImage
	}
	opened, ok := callSession(session, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if !ok || opened.Status != vfs.StatusOK {
		sys.HandleClose(session)
		return pendingFill{}, supervisor.LaunchProcessFailed
	}
	headerReply, ok := callSession(session, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 8})
	format := supervisor.ClassifyExecutable(headerReply.Data)
	if !ok || headerReply.Status != vfs.StatusOK || len(headerReply.Data) != 8 || format == supervisor.ExecutableUnknown {
		closeFile(session, opened.Handle)
		return pendingFill{}, supervisor.LaunchInvalidImage
	}
	correlation++
	header := min(stat.Size, uint64(vm.PageSize))
	object, status := sys.ExecutableCreate(loaderHandle, header)
	if status != kcall.StatusOK {
		closeFile(session, opened.Handle)
		return pendingFill{}, supervisor.LaunchResourceExhausted
	}
	fill, status := sys.HandleDuplicate(object, capability.Write|capability.Transfer)
	if status != kcall.StatusOK {
		sys.HandleClose(object)
		closeFile(session, opened.Handle)
		return pendingFill{}, supervisor.LaunchResourceExhausted
	}
	message, encoded := vfs.EncodeRequest(vfs.Request{Operation: vfs.OperationPrepareExecutable, Handle: opened.Handle, Size: uint32(header)}, correlation)
	if encoded {
		message.HandleCount, message.Handles[0] = 1, fill
	}
	if encoded && sendMessage(uint64(session), message) {
		now, _ := sys.ClockRead()
		return pendingFill{session: session, file: opened.Handle, object: object, size: stat.Size, correlation: correlation, deadline: now + fillDeadline(header), format: format}, supervisor.LaunchOK
	}
	sys.HandleClose(fill)
	sys.HandleClose(object)
	filesystemRequests.Reject(session, nil)
	closeFile(session, opened.Handle)
	return pendingFill{}, supervisor.LaunchResourceExhausted
}

// finishSource takes the VFS's answer to a fill and makes the executable
// source, which then owns the session and its open file. On failure
// everything the fill held is closed.
func finishSource(pending pendingFill, message ipc.Message, status ipc.Status) (supervisor.Executable, supervisor.LaunchStatus) {
	request := vfs.Request{Operation: vfs.OperationPrepareExecutable, Size: uint32(min(pending.size, uint64(vm.PageSize)))}
	reply, valid := filesystemRequests.Complete(pending.session, request, pending.correlation, &message, status)
	result := supervisor.LaunchProcessFailed
	switch {
	case valid && reply.Operation == vfs.OperationPrepareExecutable && reply.Status == vfs.StatusOK && reply.Size == min(pending.size, uint64(vm.PageSize)):
		object, createStatus := sys.ExecutableSource(loaderHandle, pending.session, pending.file, pending.object, pending.size)
		if createStatus == kcall.StatusOK {
			return supervisor.Executable{Image: object, Size: uint32(pending.size), Format: pending.format}, supervisor.LaunchOK
		}
		result = supervisor.LaunchResourceExhausted
		fallthrough
	case valid && reply.Status != vfs.StatusBackendError:
		result = supervisor.LaunchInvalidImage
		fallthrough
	default:
		syslog.Warn("executable read failed", syslog.Uint("read", reply.Size), syslog.Uint("size", pending.size))
		sys.HandleClose(pending.object)
	}
	if !valid {
		// No Close request may follow an unresolved exchange on this channel.
		sys.HandleClose(pending.session)
	} else {
		closeFile(pending.session, pending.file)
	}
	return supervisor.Executable{}, result
}

// prepareSource reads the program at path into an executable source and
// waits for it, at most the fill deadline. A VFS that does not answer costs
// the launch, not PID 1.
func prepareSource(path []byte) (supervisor.Executable, supervisor.LaunchStatus) {
	pending, status := beginSource(path)
	if status != supervisor.LaunchOK {
		return supervisor.Executable{}, status
	}
	now, clockStatus := sys.ClockRead()
	if clockStatus != kcall.StatusOK || now >= pending.deadline {
		dropFill(&pending)
		return supervisor.Executable{}, supervisor.LaunchProcessFailed
	}
	message, received := receive(uint64(pending.session), pending.deadline-now)
	if received == ipc.StatusTimeout {
		syslog.Warn("executable read timed out", syslog.Uint("size", pending.size))
		filesystemRequests.Reject(pending.session, nil)
		sys.HandleClose(pending.object)
		sys.HandleClose(pending.session)
		return supervisor.Executable{}, supervisor.LaunchProcessFailed
	}
	return finishSource(pending, message, received)
}

// completeFill takes the VFS's answer to a job's fill, records the object or
// the failure, and moves on to the next stage.
func completeFill(job *execJob) {
	pending := job.fill
	job.fill = pendingFill{}
	entry := &job.prepared[job.nextStage]
	message, status := receiveNow(pending.session)
	entry.executable, entry.status = finishSource(pending, message, status)
	entry.ready = true
	job.nextStage++
	advancePreparation(job)
}

// fillTimeout returns how long the loop may wait before the earliest pending
// fill is overdue, and whether any fill is pending.
func fillTimeout() (uint64, bool) {
	now, _ := sys.ClockRead()
	earliest, pending := earliestFill()
	if !pending {
		return 0, false
	}
	if earliest <= now {
		return 1, true
	}
	return earliest - now, true
}

// expireFills gives up on unanswered fills. Local handles are retired;
// closing a channel does not revoke mappings the VFS may still retain.
// It reports whether a service's read ended, so the caller starts it again.
func expireFills() bool {
	now, _ := sys.ClockRead()
	for index := range execJobs {
		job := &execJobs[index]
		if !job.active || job.fill.session == 0 || now < job.fill.deadline {
			continue
		}
		pending := job.fill
		job.fill = pendingFill{}
		syslog.Warn("executable read timed out", syslog.Uint("size", pending.size))
		filesystemRequests.Reject(pending.session, nil)
		sys.HandleClose(pending.object)
		sys.HandleClose(pending.session)
		entry := &job.prepared[job.nextStage]
		entry.ready, entry.status = true, supervisor.LaunchProcessFailed
		job.nextStage++
		advancePreparation(job)
	}
	expireRemoteFills(now)
	return expireServiceFills(now)
}

// receiveNow takes a message that has arrived, without waiting.
func receiveNow(channel handle.ID) (ipc.Message, ipc.Status) { return receive(uint64(channel), 0) }

func closeFile(session handle.ID, file vfs.Handle) {
	callSession(session, vfs.Request{Operation: vfs.OperationClose, Handle: file})
	sys.HandleClose(session)
}

// takePrepared hands the pipeline the object prepared for the first stage of
// the launching job that names path and has not been used yet.
func takePrepared(path []byte) (supervisor.Executable, supervisor.LaunchStatus, bool) {
	job := launchingJob
	if job == nil {
		return supervisor.Executable{}, 0, false
	}
	for index := uint8(0); index < job.request.Count; index++ {
		entry := &job.prepared[index]
		if entry.ready && !entry.consumed && equalPath(job.request.Stages[index].Path, path) {
			entry.consumed = true
			return entry.executable, entry.status, true
		}
	}
	return supervisor.Executable{}, 0, false
}

// releasePrepared closes the objects of stages that were never launched.
func releasePrepared(job *execJob) {
	for index := range job.prepared {
		entry := &job.prepared[index]
		if entry.ready && !entry.consumed && entry.executable.Image != 0 {
			sys.HandleClose(entry.executable.Image)
		}
		*entry = prepared{}
	}
}

// cancelPreparation stops a job still being prepared: its pending fill is
// answered and dropped, and nothing else starts.
func cancelPreparation(job *execJob) bool {
	if !job.preparing {
		return false
	}
	job.cancelled = true
	return true
}

func equalPath(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
