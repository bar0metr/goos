//go:build uefi

package main

import (
	"goos/internal/logfmt"
	"goos/kernel/arch/amd64"
	linuxcompat "goos/kernel/compat/linux"
	"goos/kernel/handle"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/userspace/startup"
)

var threadRollbackStartup [startup.MaxSize]byte

// checkThreadCreationRollback fails clone's architecture stage at several
// points and checks that each failure returns every resource the creation
// held: pages, scheduler slots, the Process task charge, the thread record,
// and the kernel-entry stack mappings in the shared root (ADR 0179).
func checkThreadCreationRollback() {
	owner, found := processRegistry.ByID(selfTestLinuxProcessID)
	data, encoded := threadRollbackStartupData()
	streams := linuxcompat.StandardStreams{Input: 1, Output: 2, Error: 3}
	if !found || !encoded || !linuxcompat.InstallProcess(selfTestRootLinux, selfTestLinuxProcessID, data, streams, 0x600000, 0x4600000) {
		panicSerial("thread rollback self-test setup failed")
	}
	registers := amd64.UserRegisters{RIP: selfTestLinuxEntry, RSP: selfTestLinuxStack}
	// The first attempt maps a full stack and leaves the page tables that
	// held it in the root, as every stack does. Later attempts take the same
	// lowest free run, so their page counts must match exactly.
	stages := [...]uint64{amd64.KernelStackPages(), 0, 2, amd64.KernelStackPages()}
	for index := 0; index < len(stages); index++ {
		freePages := pageAllocator.Statistics().FreePages
		freeSlots := amd64.FreeTaskSlots()
		availableTasks := owner.Available(process.ResourceTasks)
		creation, prepared := linuxcompat.PrepareThreadCreation(owner, selfTestRootLinux)
		if !prepared || !amd64.InjectTaskStackFault(stages[index]) {
			panicSerial("thread rollback self-test preparation failed")
		}
		if _, added := amd64.AddUserTaskWithRegisters(&pageAllocator, selfTestRootLinux, registers); added {
			panicSerial("injected task creation fault did not fire")
		}
		if !creation.Cancel() {
			panicSerial("thread rollback self-test cancel failed")
		}
		if index == 0 {
			continue
		}
		stack, size := amd64.InjectedTaskStack()
		pagesNow, slotsNow := pageAllocator.Statistics().FreePages, amd64.FreeTaskSlots()
		if stack == 0 || pagesNow != freePages || slotsNow != freeSlots ||
			owner.Available(process.ResourceTasks) != availableTasks || linuxcompat.ThreadRecords(selfTestRootLinux) != 0 ||
			!threadStackReleased(stack, size) {
			kernelLogUints(logfmt.Error, "thread rollback", "pages_before", freePages, "pages_after", pagesNow)
			kernelLogUints(logfmt.Error, "thread rollback", "slots_before", uint64(freeSlots), "slots_after", uint64(slotsNow))
			panicSerial("failed thread creation leaked resources")
		}
	}
	if !linuxcompat.RemoveProcess(selfTestRootLinux, nil) {
		panicSerial("thread rollback self-test teardown failed")
	}
	mark("THREAD_CREATION_ROLLBACK_OK")
}

// threadStackReleased reports whether an abandoned kernel-entry stack left no
// mapping in the shared root and its guard is back in the kernel identity map.
func threadStackReleased(stack, size uint64) bool {
	for offset := uint64(0); offset < size; offset += vm.PageSize {
		if _, mapped := vm.Resolve(selfTestRootLinux, stack+offset); mapped {
			return false
		}
	}
	physical, ok := vm.KernelPhysical(stack)
	if !ok || physical < amd64.KernelStackGuardSize {
		return false
	}
	for guard := physical - amd64.KernelStackGuardSize; guard < physical; guard += vm.PageSize {
		if !vm.IsMapped(kernelPageTable, guard) {
			return false
		}
	}
	return true
}

func threadRollbackStartupData() (startup.Data, bool) {
	writer := startup.NewWriter(threadRollbackStartup[:], 1, 0, 4)
	writer.Command([]byte("/selftest/thread"))
	writer.CWD([]byte("/"))
	writer.Argument([]byte("/selftest/thread"))
	writer.Handle([]byte("in"), 0)
	writer.Handle([]byte("stdout"), 0)
	writer.Handle([]byte("stderr"), 0)
	writer.Handle([]byte("vfs"), 0)
	template, ok := writer.Finish()
	ids := [4]handle.ID{1, 2, 3, 4}
	if !ok || !startup.BindHandles(template, ids[:]) {
		return startup.Data{}, false
	}
	return startup.Decode(template)
}
