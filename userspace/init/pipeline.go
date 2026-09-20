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

// RunPipeline starts downstream stages first, before the console-input stage.
// When request.ErrorSource is set the final stage is a stderr sink: it reads a
// private stream fed by the source stage's stderr and is not part of the stdout
// chain. It is launched first so its reader exists before the source writes.
// On failure it closes all retained endpoints before reaping started children,
// allowing stream readers to observe EOF and blocked writers to observe closure.
// Applications still control their own termination; this is not job cancellation.
func RunPipeline(request execproto.Request, console handle.ID, runner PipelineRunner) (uint64, LaunchStatus, uint64) {
	if runner == nil || console == 0 || request.Count == 0 || request.Count > execproto.MaxStages || request.ErrorSource != 0 && (request.Count < 2 || request.ErrorSource >= request.Count) {
		return 0, LaunchInvalidImage, 0
	}
	chain, errorSource := int(request.Count), -1
	if request.ErrorSource != 0 {
		chain--
		errorSource = int(request.ErrorSource) - 1
	}
	var readers, writers [execproto.MaxStages - 1]handle.ID
	var errorReader, errorWriter handle.ID
	var processes [execproto.MaxStages]handle.ID
	var lastPID uint64
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
	if status == LaunchOK {
		for index := int(request.Count) - 1; index >= 0; index-- {
			device := Endpoint{ID: console, Type: handle.TypeDevice}
			streams := Streams{Input: device, Output: device, Error: device}
			if index >= chain {
				streams.Input = Endpoint{ID: errorReader, Type: handle.TypeStream}
			} else {
				if index > 0 {
					streams.Input = Endpoint{ID: readers[index-1], Type: handle.TypeStream}
				}
				if index < chain-1 {
					streams.Output = Endpoint{ID: writers[index], Type: handle.TypeStream}
				}
				if index == errorSource {
					streams.Error = Endpoint{ID: errorWriter, Type: handle.TypeStream}
				}
			}
			var pid uint64
			pid, processes[index], status = runner.Launch(request.Stages[index], request.CWD, request.Environment, streams)
			if status != LaunchOK {
				processes[index] = 0
				break
			}
			if index == chain-1 {
				lastPID = pid
			}
			if index >= chain {
				errorReader = 0
			} else {
				if index > 0 {
					readers[index-1] = 0
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
	if errorReader != 0 && !runner.Close(errorReader) || errorWriter != 0 && !runner.Close(errorWriter) {
		status = LaunchCapabilityInstallFailed
	}
	var exitCode uint64
	for _, process := range processes[:request.Count] {
		if process != 0 {
			code := runner.Wait(process)
			if exitCode == 0 {
				exitCode = code
			}
		}
	}
	if status != LaunchOK {
		return 0, status, 0
	}
	return lastPID, LaunchOK, exitCode
}
