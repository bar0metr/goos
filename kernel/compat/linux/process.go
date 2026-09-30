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
	VFS        VFSSession
	FDs        FDTable
	BrkBase    uint64
	BrkCurrent uint64
	BrkLimit   uint64
	// PID is the process and thread identity a Linux program sees for itself.
	// It comes from a small positive range and never carries a native process
	// or task ID.
	PID int32
	// ClearChildTID is the address set_tid_address recorded. Linux clears it
	// at thread exit only while another task shares the address space, which
	// a single-task process never has, so GoOS stores it and writes nothing.
	ClearChildTID uint64
	// IOBuffer is the native handle of the memory object file reads and
	// writes move their bytes through. The bridge creates it on first use.
	IOBuffer handle.ID
	cwd      [limits.PathMax]byte
	cwdLen   uint16
}

// SetCWD replaces the working directory with a canonical absolute path.
func (state *ProcessState) SetCWD(path []byte) bool {
	if state == nil || len(path) == 0 || len(path) > limits.PathMax || path[0] != '/' {
		return false
	}
	state.cwdLen = uint16(copy(state.cwd[:], path))
	return true
}

const (
	firstPID = int32(2)
	// MaxPID matches the default Linux pid_max, so a compatibility PID fits
	// every pid_t a program may narrow it to.
	MaxPID = int32(32767)
)

var nextPID = firstPID

func (state *ProcessState) CWD() []byte { return state.cwd[:state.cwdLen] }

var processStates [process.MaxProcesses]ProcessState

// InstallProcess records one validated, bound Linux launch. The caller owns
// teardown if a later launch step fails.
func InstallProcess(root uint64, data startup.Data, brkBase uint64, brkLimit uint64) bool {
	if root == 0 || brkBase == 0 || brkBase >= brkLimit || brkBase%LinuxPageSize != 0 || brkLimit%LinuxPageSize != 0 ||
		data.HandleCount() != 4 || len(data.CWD()) == 0 || data.CWD()[0] != '/' || len(data.CWD()) > limits.PathMax {
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
	// The inherited working directory is stored in canonical form, so every
	// later relative path starts from a base that needs no second check.
	size, _, errno := ResolvePath(state.cwd[:], nil, data.CWD())
	if errno != 0 {
		return false
	}
	state.Root, state.Input, state.Output = root, input, output
	state.Error, state.VFS.Handle, state.cwdLen = errorOutput, vfsHandle, uint16(size)
	state.PID = allocatePID()
	state.BrkBase, state.BrkCurrent, state.BrkLimit = brkBase, brkBase, brkLimit
	if !state.FDs.Initialize(input, output, errorOutput) {
		*state = ProcessState{}
		return false
	}
	return true
}

// allocatePID returns the next identity no live Linux process holds. The
// table has fewer slots than the range has values, so the scan terminates.
func allocatePID() int32 {
	for {
		candidate := nextPID
		nextPID++
		if nextPID > MaxPID {
			nextPID = firstPID
		}
		used := false
		for index := range processStates {
			if processStates[index].Root != 0 && processStates[index].PID == candidate {
				used = true
				break
			}
		}
		if !used {
			return candidate
		}
	}
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

// CloseFD invalidates one descriptor. It reports the description when the
// last descriptor naming it closed, so the caller can release what it held.
func CloseFD(root uint64, fd int) (FileDescription, bool, abi.Errno) {
	if state, found := FindProcess(root); found {
		if released, last, ok := state.FDs.Close(fd); ok {
			return released, last, 0
		}
	}
	return FileDescription{}, false, abi.EBADF
}

// DescriptionOf returns the open-file description behind a descriptor.
func DescriptionOf(root uint64, fd uint64) (*FileDescription, bool) {
	if state, found := FindProcess(root); found && fd < LinuxMaxFD {
		return state.FDs.Description(int(fd))
	}
	return nil, false
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
				releaseDirectories(root)
				processStates[index] = ProcessState{}
				return true
			}
		}
	}
	return false
}
