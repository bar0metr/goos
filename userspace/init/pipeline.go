package supervisor

import (
	"goos/kernel/handle"
	execproto "goos/userspace/exec"
)

// PipelineRunner implements PID 1's resource operations. A successful Launch
// consumes its stream endpoints; a failed Launch leaves them with the caller.
// Wait reaps the process and closes its process handle.
type PipelineRunner interface {
	CreateStream() (handle.ID, handle.ID, bool)
	Close(handle.ID) bool
	Launch(execproto.Stage, []byte, []byte, handle.ID, handle.Type, handle.ID, handle.Type) (uint64, handle.ID, LaunchStatus)
	Wait(handle.ID) uint64
}

// RunPipeline starts downstream stages first, before the console-input stage.
// On failure it closes all retained endpoints before reaping started children,
// allowing stream readers to observe EOF and blocked writers to observe closure.
// Applications still control their own termination; this is not job cancellation.
func RunPipeline(request execproto.Request, console handle.ID, runner PipelineRunner) (uint64, LaunchStatus, uint64) {
	if runner == nil || console == 0 || request.Count == 0 || request.Count > execproto.MaxStages {
		return 0, LaunchInvalidImage, 0
	}
	var readers, writers [execproto.MaxStages - 1]handle.ID
	var processes [execproto.MaxStages]handle.ID
	var lastPID uint64
	status := LaunchOK
	for index := 0; index < int(request.Count)-1; index++ {
		var ok bool
		readers[index], writers[index], ok = runner.CreateStream()
		if !ok {
			status = LaunchResourceExhausted
			break
		}
	}
	if status == LaunchOK {
		for index := int(request.Count) - 1; index >= 0; index-- {
			input, output := console, console
			inputType, outputType := handle.TypeDevice, handle.TypeDevice
			if index > 0 {
				input, inputType = readers[index-1], handle.TypeStream
			}
			if index < int(request.Count)-1 {
				output, outputType = writers[index], handle.TypeStream
			}
			var pid uint64
			pid, processes[index], status = runner.Launch(request.Stages[index], request.CWD, request.Environment, input, inputType, output, outputType)
			if status != LaunchOK {
				processes[index] = 0
				break
			}
			if index == int(request.Count)-1 {
				lastPID = pid
			}
			if index > 0 {
				readers[index-1] = 0
			}
			if index < int(request.Count)-1 {
				writers[index] = 0
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
