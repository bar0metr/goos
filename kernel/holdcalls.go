//go:build uefi

package main

// The instruction-hold calls (ADR 0204). Only validation kernels carry them
// out: a test holds a thread of its own process at each instruction boundary
// of a short range of code, such as the goroutine switch, and runs a
// collection from another thread at each one. The debug-register and
// single-step work is in kernel/arch/amd64/hold_uefi.go.

import (
	"encoding/binary"

	"goos/internal/validation"
	"goos/kernel/arch/amd64"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/sched"
)

// holdTarget returns thread when it is another live thread of the caller's
// process.
func (handler nativeCallHandler) holdTarget(thread uint64) (sched.TaskID, kcall.Status) {
	_, group, caller, found := handler.threadCaller()
	if !found {
		return 0, kcall.StatusPermissionDenied
	}
	target := sched.TaskID(thread)
	if _, live := group.SlotOf(caller); !live {
		return 0, kcall.StatusPermissionDenied
	}
	if _, live := group.SlotOf(target); !live || target == caller {
		return 0, kcall.StatusNotFound
	}
	return target, kcall.StatusOK
}

// ThreadHoldRange arms a hold of thread at the first instruction of
// [lo, hi), which must lie in the process's code. A thread whose hold is
// still armed answers AlreadyExists; cancel it first.
func (handler nativeCallHandler) ThreadHoldRange(thread uint64, lo uint64, hi uint64) kcall.Status {
	if !validation.Enabled {
		return kcall.StatusUnsupported
	}
	owner, _, _, found := handler.threadCaller()
	if !found {
		return kcall.StatusPermissionDenied
	}
	target, status := handler.holdTarget(thread)
	if status != kcall.StatusOK {
		return status
	}
	if !executableEntry(owner, lo) || !executableEntry(owner, hi-1) {
		return kcall.StatusInvalidArgument
	}
	switch amd64.ArmTaskHold(target, lo, hi) {
	case amd64.ArmOK:
		return kcall.StatusOK
	case amd64.ArmBusy:
		return kcall.StatusAlreadyExists
	}
	return kcall.StatusNotFound
}

// ThreadHoldWait writes where thread stopped: its RIP and HoldStopped, or
// HoldDone once a step took it out of the range and the hold ended, and its
// stack pointer. It
// answers Timeout while the thread runs toward its next stop. A step that
// left the range through a call is stepped over here: the thread runs on to
// a breakpoint at the return address and the call answers Timeout.
func (handler nativeCallHandler) ThreadHoldWait(thread uint64, address uint64) kcall.Status {
	if !validation.Enabled {
		return kcall.StatusUnsupported
	}
	local := thisCPU()
	target, status := handler.holdTarget(thread)
	if status != kcall.StatusOK {
		return status
	}
	if !vm.ValidateUserRange(address, kcall.HoldRecordSize) || !vm.CanCopyOut(handler.root, address, kcall.HoldRecordSize) {
		return kcall.StatusInvalidArgument
	}
	hold, live := amd64.TaskHoldStatus(target)
	if !live || !hold.Armed {
		return kcall.StatusNotFound
	}
	if !hold.Off {
		return kcall.StatusTimeout
	}
	state := kcall.HoldStopped
	if hold.State == amd64.HoldLeft {
		if vm.ValidateUserRange(hold.RSP, 8) && vm.CopyIn(handler.root, hold.RSP, local.threadCodeWire[:]) &&
			amd64.ReturnInto(binary.LittleEndian.Uint64(local.threadCodeWire[:]), hold.Previous, hold.Lo, hold.Hi) {
			if !amd64.StepTaskHold(target, binary.LittleEndian.Uint64(local.threadCodeWire[:])) {
				return kcall.StatusNotFound
			}
			return kcall.StatusTimeout
		}
		if !amd64.EndTaskHold(target) {
			return kcall.StatusNotFound
		}
		state = kcall.HoldDone
	}
	binary.LittleEndian.PutUint64(local.holdWire[0:], hold.RIP)
	binary.LittleEndian.PutUint64(local.holdWire[8:], state)
	binary.LittleEndian.PutUint64(local.holdWire[16:], hold.RSP)
	if !vm.CopyOut(handler.root, address, local.holdWire[:]) {
		panicSerial("validated hold record write failed")
	}
	return kcall.StatusOK
}

// ThreadHoldStep runs a stopped thread to its next instruction boundary.
// The instruction it stands at decides how (amd64.StepOver). MOV SS, POP SS,
// IRET and code that cannot be read far enough are refused with
// InvalidArgument; the thread stays stopped.
func (handler nativeCallHandler) ThreadHoldStep(thread uint64) kcall.Status {
	if !validation.Enabled {
		return kcall.StatusUnsupported
	}
	target, status := handler.holdTarget(thread)
	if status != kcall.StatusOK {
		return status
	}
	hold, live := amd64.TaskHoldStatus(target)
	if !live || !hold.Armed {
		return kcall.StatusNotFound
	}
	if !hold.Off || hold.State != amd64.HoldStopped {
		return kcall.StatusTimeout
	}
	// The instruction may end on a page before an unmapped one: read what
	// is there, a byte at a time, and let StepOver refuse what it cannot
	// decode.
	var code [kcall.MaxInstruction]byte
	length := 0
	for length < len(code) && vm.ValidateUserRange(hold.RIP+uint64(length), 1) &&
		vm.CopyIn(handler.root, hold.RIP+uint64(length), code[length:length+1]) {
		length++
	}
	breakpoint, stepped := amd64.StepOver(hold.RIP, code[:length])
	if !stepped {
		return kcall.StatusInvalidArgument
	}
	if !amd64.StepTaskHold(target, breakpoint) {
		return kcall.StatusNotFound
	}
	return kcall.StatusOK
}

// ThreadHoldCancel disarms thread's hold and lets it run on.
func (handler nativeCallHandler) ThreadHoldCancel(thread uint64) kcall.Status {
	if !validation.Enabled {
		return kcall.StatusUnsupported
	}
	target, status := handler.holdTarget(thread)
	if status != kcall.StatusOK {
		return status
	}
	if !amd64.EndTaskHold(target) {
		return kcall.StatusNotFound
	}
	return kcall.StatusOK
}
