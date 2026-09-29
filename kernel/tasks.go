//go:build uefi

package main

// Kernel-side tasks and the dispatch entry points the scheduler calls.

import (
	"goos/internal/logfmt"
	"goos/internal/validation"
	"goos/kernel/arch/amd64"
	"goos/kernel/kcall"
	"goos/kernel/process"
)

var nativeCallABIRejections uint64

// authorizeNativeCall rejects the native gate before any operation-specific
// dispatcher observes registers belonging to another process ABI. The first
// rejection is logged and the counter remains bounded to one machine word.
//
//export goos_authorize_native_call
func authorizeNativeCall(root uint64) uint64 {
	if processRegistry.AllowsABI(root, process.ABINative) {
		return uint64(kcall.StatusOK)
	}
	nativeCallABIRejections++
	if nativeCallABIRejections == 1 {
		kernelLogUint(logfmt.Warn, "native call ABI rejected", "root", root)
	}
	return uint64(kcall.StatusPermissionDenied)
}

// dispatchKernelCall is the architecture-neutral native-call boundary. The
// packed return keeps the assembly bridge simple: action occupies the upper
// half and the typed native status occupies the lower half.
//
//export goos_dispatch_kernel_call
func dispatchKernelCall(number uint64, arg0 uint64, arg1 uint64, root uint64) uint64 {
	// The handler is reused through a pointer: a struct passed as an interface
	// would be boxed, and the kernel leaks every heap allocation.
	dispatchHandler.root = root
	result := kcall.Dispatch(kcall.Request{Operation: kcall.Operation(number), Arg0: arg0, Arg1: arg1, Arg2: amd64.KernelCallArg2()}, &dispatchHandler)
	return uint64(result.Action)<<32 | uint64(result.Status)
}

//export goos_demo_task_a
func runDemoTaskA() {
	for !validation.Enabled {
		amd64.KernelSleep()
	}
	for demoTaskA < 3 {
		demoTaskA++
		amd64.Halt()
	}
	demoABlocked = 1
	amd64.BlockCurrentTask()
	amd64.Halt()
	demoAResumed = 1
	demoTaskA++
	for {
		amd64.KernelSleep()
	}
}

//export goos_demo_task_b
func runDemoTaskB() {
	for !validation.Enabled {
		amd64.KernelSleep()
	}
	for {
		demoTaskB++
		if demoABlocked != 0 && demoAResumed == 0 && demoTaskB >= 6 {
			amd64.WakeTask(1)
		}
		if demoAResumed != 0 {
			break
		}
		amd64.Halt()
	}
	for {
		amd64.KernelSleep()
	}
}

//export goos_idle_task
func runIdleTask() {
	for {
		amd64.Halt()
	}
}
