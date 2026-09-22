package supervisor

import (
	"goos/kernel/handle"
	execproto "goos/userspace/exec"
)

// Endpoint is one handle granted to a child together with its object type.
type Endpoint struct {
	ID   handle.ID
	Type handle.Type
}

// Streams names the standard-stream endpoints granted to one stage.
type Streams struct {
	Input, Output, Error Endpoint
}

// PipelineRunner implements PID 1's resource operations. A successful Launch
// consumes its stream endpoints; a failed Launch leaves them with the caller.
// Wait reaps the process and closes its process handle.
type PipelineRunner interface {
	CreateStream() (handle.ID, handle.ID, bool)
	Close(handle.ID) bool
	Launch(stage execproto.Stage, cwd, environment []byte, streams Streams) (uint64, handle.ID, LaunchStatus)
	Wait(handle.ID) uint64
}

// Job is a started pipeline. Its processes are reaped in stage order; a stage
// that exits early stays completed in the kernel until its turn.
type Job struct {
	processes [execproto.MaxStages]handle.ID
	codes     [execproto.MaxStages]uint64
	count     uint8
	next      uint8
	lastPID   uint64
	status    LaunchStatus
}

// RunPipeline starts a pipeline and waits for all of its processes.
func RunPipeline(request execproto.Request, console handle.ID, runner PipelineRunner) (uint64, LaunchStatus, uint64) {
	job := StartPipeline(request, console, runner)
	for {
		process, pending := job.Next()
		if !pending {
			return job.Result()
		}
		job.Complete(runner.Wait(process))
	}
}

// StartPipeline starts downstream stages first, before the console-input stage.
// When request.ErrorSource is set the final stage is a stderr sink: it reads a
// private stream fed by the source stage's stderr and is not part of the stdout
// chain. It is launched first so its reader exists before the source writes.
// A background request gives the first stage an input stream that is already
// at EOF instead of the console.
// On failure it closes all retained endpoints, so the stages already started
// observe EOF or closure; the job still reaps them before it reports.
func StartPipeline(request execproto.Request, console handle.ID, runner PipelineRunner) Job {
	var job Job
	if runner == nil || console == 0 || request.Count == 0 || request.Count > execproto.MaxStages || request.ErrorSource != 0 && (request.Count < 2 || request.ErrorSource >= request.Count) {
		job.status = LaunchInvalidImage
		return job
	}
	job.count = request.Count
	chain, errorSource := int(request.Count), -1
	if request.ErrorSource != 0 {
		chain--
		errorSource = int(request.ErrorSource) - 1
	}
	var readers, writers [execproto.MaxStages - 1]handle.ID
	var errorReader, errorWriter, emptyInput handle.ID
	status := LaunchOK
	for index := 0; index < chain-1; index++ {
		var ok bool
		readers[index], writers[index], ok = runner.CreateStream()
		if !ok {
			status = LaunchResourceExhausted
			break
		}
	}
	if status == LaunchOK && errorSource >= 0 {
		var ok bool
		errorReader, errorWriter, ok = runner.CreateStream()
		if !ok {
			status = LaunchResourceExhausted
		}
	}
	if status == LaunchOK && request.Background {
		var writer handle.ID
		var ok bool
		emptyInput, writer, ok = runner.CreateStream()
		if !ok || !runner.Close(writer) {
			status = LaunchResourceExhausted
		}
	}
	if status == LaunchOK {
		for index := int(request.Count) - 1; index >= 0; index-- {
			device := Endpoint{ID: console, Type: handle.TypeDevice}
			streams := Streams{Input: device, Output: device, Error: device}
			if index >= chain {
				streams.Input = Endpoint{ID: errorReader, Type: handle.TypeStream}
			} else {
				if index > 0 {
					streams.Input = Endpoint{ID: readers[index-1], Type: handle.TypeStream}
				} else if emptyInput != 0 {
					streams.Input = Endpoint{ID: emptyInput, Type: handle.TypeStream}
				}
				if index < chain-1 {
					streams.Output = Endpoint{ID: writers[index], Type: handle.TypeStream}
				}
				if index == errorSource {
					streams.Error = Endpoint{ID: errorWriter, Type: handle.TypeStream}
				}
			}
			var pid uint64
			pid, job.processes[index], status = runner.Launch(request.Stages[index], request.CWD, request.Environment, streams)
			if status != LaunchOK {
				job.processes[index] = 0
				break
			}
			if index == chain-1 {
				job.lastPID = pid
			}
			if index >= chain {
				errorReader = 0
			} else {
				if index > 0 {
					readers[index-1] = 0
				} else {
					emptyInput = 0
				}
				if index < chain-1 {
					writers[index] = 0
				}
				if index == errorSource {
					errorWriter = 0
				}
			}
		}
	}
	for index := range readers {
		if readers[index] != 0 && !runner.Close(readers[index]) {
			status = LaunchCapabilityInstallFailed
		}
		if writers[index] != 0 && !runner.Close(writers[index]) {
			status = LaunchCapabilityInstallFailed
		}
	}
	if errorReader != 0 && !runner.Close(errorReader) || errorWriter != 0 && !runner.Close(errorWriter) || emptyInput != 0 && !runner.Close(emptyInput) {
		status = LaunchCapabilityInstallFailed
	}
	job.status = status
	job.skipIdle()
	return job
}

// Next returns the process the job waits for now.
func (job *Job) Next() (handle.ID, bool) {
	if job.next >= job.count {
		return 0, false
	}
	return job.processes[job.next], true
}

// Complete records the exit code of the process returned by Next.
func (job *Job) Complete(code uint64) {
	if job.next < job.count {
		job.codes[job.next] = code
		job.next++
		job.skipIdle()
	}
}

func (job *Job) skipIdle() {
	for job.next < job.count && job.processes[job.next] == 0 {
		job.next++
	}
}

// Processes returns the handles of the stages that are still running.
func (job *Job) Processes() []handle.ID {
	if job.next >= job.count {
		return nil
	}
	return job.processes[job.next:job.count]
}

// Result reports the last stdout stage's PID and the first non-zero exit code
// in stage order, or the launch failure.
func (job *Job) Result() (uint64, LaunchStatus, uint64) {
	if job.status != LaunchOK {
		return 0, job.status, 0
	}
	for index := uint8(0); index < job.count; index++ {
		if job.codes[index] != 0 {
			return job.lastPID, LaunchOK, job.codes[index]
		}
	}
	return job.lastPID, LaunchOK, 0
}
