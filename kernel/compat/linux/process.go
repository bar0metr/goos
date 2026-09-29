// Package linux owns bounded per-process compatibility state that is not
// exposed in the Linux address space.
package linux

import (
	"goos/internal/limits"
	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/process"
	"goos/userspace/startup"
)

// ProcessState retains launch authority, descriptor state, and the working
// directory used by Linux syscall translation. Native handle
// IDs never enter the Linux initial stack.
type ProcessState struct {
	Root       uint64
	Input      handle.ID
	Output     handle.ID
	Error      handle.ID
	VFS        handle.ID
	FDs        FDTable
	BrkBase    uint64
	BrkCurrent uint64
	BrkLimit   uint64
	cwd        [limits.PathMax]byte
	cwdLen     uint16
}

func (state *ProcessState) CWD() []byte { return state.cwd[:state.cwdLen] }

var processStates [process.MaxProcesses]ProcessState

// InstallProcess records one validated, bound Linux launch. The caller owns
// teardown if a later launch step fails.
func InstallProcess(root uint64, data startup.Data, brkBase uint64, brkLimit uint64) bool {
	if root == 0 || brkBase == 0 || brkBase >= brkLimit || brkBase%LinuxPageSize != 0 || brkLimit%LinuxPageSize != 0 ||
		data.HandleCount() != 4 || len(data.CWD()) == 0 || len(data.CWD()) > limits.PathMax {
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
	state := &processStates[free]
	*state = ProcessState{}
	state.Root, state.Input, state.Output = root, input, output
	state.Error, state.VFS, state.cwdLen = errorOutput, vfsHandle, uint16(len(data.CWD()))
	state.BrkBase, state.BrkCurrent, state.BrkLimit = brkBase, brkBase, brkLimit
	if !state.FDs.Initialize(input, output, errorOutput) {
		*state = ProcessState{}
		return false
	}
	copy(state.cwd[:], data.CWD())
	return true
}

func LookupFD(root uint64, fd int) (FileDescription, bool) {
	if root != 0 {
		for index := range processStates {
			if processStates[index].Root == root {
				return processStates[index].FDs.Lookup(fd)
			}
		}
	}
	return FileDescription{}, false
}

func AllocateFD(root uint64, description FileDescription, closeOnExec bool) (int, abi.Errno) {
	if root != 0 {
		for index := range processStates {
			if processStates[index].Root != root {
				continue
			}
			if fd, ok := processStates[index].FDs.Allocate(description, closeOnExec); ok {
				return fd, 0
			}
			return -1, abi.EMFILE
		}
	}
	return -1, abi.EBADF
}

func FindProcess(root uint64) (*ProcessState, bool) {
	if root != 0 {
		for index := range processStates {
			if processStates[index].Root == root {
				return &processStates[index], true
			}
		}
	}
	return nil, false
}

func RemoveProcess(root uint64) bool {
	if root != 0 {
		for index := range processStates {
			if processStates[index].Root == root {
				processStates[index].FDs.CloseAll(nil)
				processStates[index] = ProcessState{}
				return true
			}
		}
	}
	return false
}
