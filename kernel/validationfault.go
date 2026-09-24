//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/internal/validation"
	"goos/kernel/arch/amd64"
	"goos/kernel/handle"
	"goos/kernel/kcall"
)

// ValidationFault overflows the calling task's kernel-entry stack on purpose,
// so the boot test can see the guard below it stop the overflow and the double
// fault, on its own stack, report the guard page instead of the machine
// resetting (ADR 0091). Only the validation build carries it out, and only for
// the holder of the system-control capability, as for a reset: it ends the
// machine.
func (handler nativeCallHandler) ValidationFault(device uint64) kcall.Status {
	if !validation.Enabled {
		return kcall.StatusUnsupported
	}
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(device), handle.TypeDevice, capability.Write)
	if !allowed || authority.Object() != deviceSystemControl {
		return kcall.StatusPermissionDenied
	}
	low, high, ok := amd64.KernelStackGuard(amd64.StackPointer())
	if !ok {
		return kcall.StatusNotFound
	}
	console := amd64.NewCOM1()
	console.WriteString("KERNEL_STACK_GUARD ")
	console.WriteHex(low)
	console.WriteString(" ")
	console.WriteHex(high)
	console.WriteString("\n")
	overflowKernelStack(0)
	return kcall.StatusIOError
}

// overflowLimit is never reached; it only gives the recursion an exit the
// compiler cannot prove away.
var overflowLimit = ^uint64(0)

// overflowKernelStack recurses through 4 KiB frames, each written and read
// back after the call, so neither the frame nor the recursion can be
// optimised out.
//
//go:noinline
func overflowKernelStack(depth uint64) uint64 {
	if depth == overflowLimit {
		return 0
	}
	var frame [512]uint64
	frame[depth%512] = depth
	return overflowKernelStack(depth+1) + frame[depth*7%512]
}
