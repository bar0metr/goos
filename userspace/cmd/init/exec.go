//go:build goos_userspace

package main

// The shell's exec protocol: job requests, their pipelines, completion, and the
// control requests that launch nothing (ADR 0064, 0067).

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	userwait "goos/userspace/wait"
)

func pollExec() {
	arena.Reset(arenaBase)
	status := ipc.Status(sys.IPCReceive(handle.ID(execServer), wire[:]))
	if status == ipc.StatusNotFound {
		return
	}
	if status == ipc.StatusPeerDied {
		// The shell is gone; its exit event ends the session, and its
		// replacement brings a new channel.
		sys.HandleClose(handle.ID(execServer))
		execServer = 0
		return
	}
	if status != ipc.StatusOK {
		fatal("INIT_EXEC_RECEIVE_FAILED\n")
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
	message, ok := ipc.Decode(wire[:size])
	if !ok {
		fatal("INIT_EXEC_CORRUPT\n")
	}
	if execproto.IsControl(message) {
		handleControl(message)
		return
	}
	slot, size, ok := execproto.DecodeSubmit(message)
	if !ok {
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchInvalidImage), 0, 0)
		return
	}
	job := freeJob(slot)
	if job == nil {
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchResourceExhausted), 0, 0)
		return
	}
	start := int(slot) * execproto.SlotSize
	copy(job.data[:size], execPage[start:start+size])
	request, ok := execproto.DecodeRequest(job.data[:size])
	if !ok {
		sendExecReply(message.Correlation, 0, uint64(supervisor.LaunchInvalidImage), 0, 0)
		return
	}
	job.active, job.orphaned, job.slot, job.correlation = true, false, slot, message.Correlation
	job.job = supervisor.StartPipeline(request, handle.ID(terminalOutput[1]), nativePipelineRunner{})
	// A foreground job's input is fed by the shell, which receives the writing
	// end before the job's reply. If it cannot be delivered, closing it gives
	// the job end of input instead of a stream no one will ever write.
	if input := job.job.TakeInput(); input != 0 {
		started, ok := execproto.EncodeStarted(slot, input, message.Correlation)
		if !ok || !sendMessage(execServer, started) {
			sys.HandleClose(input)
		}
	}
	finishJob(job)
}

// freeJob returns an unused job record, or nil when the slot is still owned
// by a running job or every record is busy.
func freeJob(slot uint8) *execJob {
	var free *execJob
	for index := range execJobs {
		if execJobs[index].active && execJobs[index].slot == slot {
			return nil
		}
		if !execJobs[index].active && free == nil {
			free = &execJobs[index]
		}
	}
	return free
}

// advanceJob reaps the process a job was waiting for; it has completed.
func advanceJob(index int) {
	job := &execJobs[index]
	if process, pending := job.job.Next(); job.active && pending {
		job.job.Complete(waitApplicationExit(process))
		finishJob(job)
	}
}

func finishJob(job *execJob) {
	if _, pending := job.job.Next(); pending {
		return
	}
	pid, launchStatus, exitCode := job.job.Result()
	job.active = false
	if job.orphaned {
		job.orphaned = false
		return
	}
	stage := uint8(0)
	if launchStatus != supervisor.LaunchOK {
		stage = job.job.FailedStage()
	}
	sendExecReply(job.correlation, pid, uint64(launchStatus), exitCode, stage)
}

// handleControl answers the shell's requests that launch nothing.
func handleControl(message ipc.Message) {
	operation, argument, ok := execproto.DecodeControl(message)
	switch {
	case !ok:
		sendControlStatus(execproto.ControlInvalid, message.Correlation)
	case operation == execproto.ControlKill:
		sendControlStatus(killJob(argument), message.Correlation)
	case operation == execproto.ControlFault && (argument == execproto.FaultConfig || argument == execproto.FaultLog):
		process, marker := &configProcessHandle, "CONFIG_FAULT_INJECTED\n"
		if argument == execproto.FaultLog {
			process, marker = &logProcessHandle, "LOG_FAULT_INJECTED\n"
		}
		status := execproto.ControlNotFound
		if injectServiceFault(process, marker) {
			status = execproto.ControlOK
		}
		sendControlStatus(status, message.Correlation)
	case operation == execproto.ControlFault && argument == execproto.FaultBlock:
		status := execproto.ControlNotFound
		if injectBlockFault() {
			status = execproto.ControlOK
		}
		sendControlStatus(status, message.Correlation)
	case operation == execproto.ControlFault && argument == execproto.FaultVFS:
		status := execproto.ControlNotFound
		if injectVFSFault() {
			status = execproto.ControlOK
		}
		sendControlStatus(status, message.Correlation)
	case operation == execproto.ControlFault && argument == execproto.FaultFilesystem:
		status := execproto.ControlNotFound
		if injectFilesystemFault() {
			status = execproto.ControlOK
		}
		sendControlStatus(status, message.Correlation)
	case operation == execproto.ControlFault:
		status := injectSessionFault(argument)
		// A shell that was stopped is not there to be answered.
		if argument != execproto.FaultShell || status != execproto.ControlOK {
			sendControlStatus(status, message.Correlation)
		}
	case operation == execproto.ControlDiagnostics:
		// Only a diagnostic build dumps; the others answer unsupported.
		status := execproto.ControlInvalid
		if sys.Diagnostics(serviceDeviceHandle) == 0 {
			status = execproto.ControlOK
		}
		sendControlStatus(status, message.Correlation)
	case operation == execproto.ControlServices:
		var entries [execproto.MaxServiceEntries]execproto.ServiceEntry
		count := 0
		for index := 0; count < len(entries); index++ {
			name, snapshot, found := manager.At(index)
			if !found {
				break
			}
			if len(name) > execproto.MaxServiceName {
				name = name[:execproto.MaxServiceName]
			}
			entries[count] = execproto.ServiceEntry{Name: name, State: uint8(snapshot.State), Restarts: snapshot.Restarts, PID: uint32(snapshot.PID)}
			count++
		}
		if reply, encoded := execproto.EncodeServices(entries[:count], message.Correlation); encoded {
			sendMessage(execServer, reply)
		}
	case operation == execproto.ControlReboot, operation == execproto.ControlPowerOff:
		shutDown(operation == execproto.ControlPowerOff, message.Correlation)
	}
}

// killJob stops every stage of the job in one slot. The kernel retires their
// tasks, so the reply to the job itself still arrives through the normal path.
func killJob(slot uint8) execproto.ControlStatus {
	for index := range execJobs {
		job := &execJobs[index]
		if !job.active || job.slot != slot {
			continue
		}
		stopped := false
		for _, process := range job.job.Processes() {
			if process != 0 && sys.ProcessKill(process) == 0 {
				stopped = true
			}
		}
		if !stopped {
			return execproto.ControlNotFound
		}
		syslog.Info("job stopped", syslog.Uint("slot", uint64(slot)))
		return execproto.ControlOK
	}
	return execproto.ControlNotFound
}

func sendControlStatus(status execproto.ControlStatus, correlation uint64) {
	if reply, encoded := execproto.EncodeControlStatus(status, correlation); encoded {
		sendMessage(execServer, reply)
	}
}

// nativePipelineRunner starts and waits for the stages of a job for the
// pipeline core in userspace/init.
type nativePipelineRunner struct{}

func (nativePipelineRunner) CreateStream() (handle.ID, handle.ID, bool) {
	var pair [2]uint64
	if !createStream(&pair) {
		return 0, 0, false
	}
	return handle.ID(pair[0]), handle.ID(pair[1]), true
}

func (nativePipelineRunner) Close(id handle.ID) bool { return sys.HandleClose(id) == 0 }

func (nativePipelineRunner) Launch(stage execproto.Stage, cwd, environment []byte, streams supervisor.Streams) (uint64, handle.ID, supervisor.LaunchStatus) {
	// A command of /system/bin is known by its name there, which selects its
	// grants. The rest of /system holds services, which are not commands; a
	// program anywhere else is named by its file.
	command, fromSearchPath := execproto.CommandFromSystemPath(stage.Path)
	if !fromSearchPath {
		if execproto.InSystem(stage.Path) {
			return 0, 0, supervisor.LaunchCapabilityDenied
		}
		command = execproto.ExecutableName(stage.Path)
	}
	return launchApplication(stage.Path, command, cwd, stage.Arguments, environment, streams)
}

func (nativePipelineRunner) Wait(process handle.ID) uint64 { return waitApplicationExit(process) }

func waitApplicationExit(processHandle handle.ID) uint64 {
	var exitCode uint64
	for {
		var interests [userwait.MaxObjects]userwait.Interest
		interests[0] = userwait.Interest{Handle: processHandle, Events: kcall.WaitCompleted}
		if _, status := userwait.Any(&interests, 1, kcall.WaitAnyInfiniteTimeout); status != kcall.StatusOK {
			fatal("INIT_EXEC_WAIT_READY_FAILED\n")
		}
		code, status := sys.ProcessWait(processHandle, 0)
		exitCode = code
		if status != kcall.StatusOK {
			if status == ipc.StatusNotFound {
				continue
			}
			fatal("INIT_EXEC_WAIT_FAILED\n")
		}
		if sys.HandleClose(processHandle) != 0 {
			fatal("INIT_EXEC_CLOSE_FAILED\n")
		}
		if !processWaitAnnounced {
			mark("PROCESS_HANDLE_WAIT_OK\n")
			processWaitAnnounced = true
		}
		return exitCode
	}
}

// sendExecReply answers the shell. A reply that cannot be encoded is PID 1's
// own fault; one that cannot be delivered means the shell has just gone, and
// its restart does not want the answer.
func sendExecReply(correlation, pid, status, exitCode uint64, stage uint8) {
	reply, ok := execproto.EncodeReply(execproto.Reply{Process: pid, LaunchStatus: status, ExitCode: exitCode, Stage: stage}, correlation)
	if !ok {
		fatal("INIT_EXEC_REPLY_FAILED\n")
	}
	if execServer == 0 || !sendMessage(execServer, reply) {
		syslog.Warn("exec reply undelivered")
	}
}
