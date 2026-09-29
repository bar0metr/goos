// Package linux owns bounded per-process compatibility state that is not
// exposed in the Linux address space.
package linux

import (
	"goos/internal/limits"
	"goos/kernel/handle"
	"goos/kernel/process"
	"goos/userspace/startup"
)

// ProcessState retains the launch authority and working directory that later
// syscall phases will translate into Linux fds and VFS requests. Native handle
// IDs never enter the Linux initial stack.
type ProcessState struct {
	Root   uint64
	Input  handle.ID
	Output handle.ID
	Error  handle.ID
	VFS    handle.ID
	cwd    [limits.PathMax]byte
	cwdLen uint16
}

func (state *ProcessState) CWD() []byte { return state.cwd[:state.cwdLen] }

var processStates [process.MaxProcesses]ProcessState

// InstallProcess records one validated, bound Linux launch. The caller owns
// teardown if a later launch step fails.
func InstallProcess(root uint64, data startup.Data) bool {
	if root == 0 || data.HandleCount() != 4 || len(data.CWD()) == 0 || len(data.CWD()) > limits.PathMax {
		return false
	}
	input, inputOK := data.Handle("in")
	output, outputOK := data.Handle("stdout")
	errorOutput, errorOK := data.Handle("stderr")
	vfsHandle, vfsOK := data.Handle("vfs")
	if !inputOK || !outputOK || !errorOK || !vfsOK || input == 0 || output == 0 || errorOutput == 0 || vfsHandle == 0 {
		return false
	}
	free := -1
	for index := range processStates {
		if processStates[index].Root == root {
			return false
		}
		if free == -1 && processStates[index].Root == 0 {
			free = index
		}
	}
	if free == -1 {
		return false
	}
	state := ProcessState{Root: root, Input: input, Output: output, Error: errorOutput, VFS: vfsHandle, cwdLen: uint16(len(data.CWD()))}
	copy(state.cwd[:], data.CWD())
	processStates[free] = state
	return true
}

func FindProcess(root uint64) (ProcessState, bool) {
	if root != 0 {
		for index := range processStates {
			if processStates[index].Root == root {
				return processStates[index], true
			}
		}
	}
	return ProcessState{}, false
}

func RemoveProcess(root uint64) bool {
	if root != 0 {
		for index := range processStates {
			if processStates[index].Root == root {
				processStates[index] = ProcessState{}
				return true
			}
		}
	}
	return false
}
