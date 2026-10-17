//go:build goos_userspace

// Command vmprobe exercises the native virtual-memory calls from inside the
// running system (ADR 0104). Without an argument it checks demand-zero
// memory, protection, discard, unmap, the W^X and address rules, pinned
// regions, heap growth and collection, a large BSS, and a deep stack,
// printing one line per check. With an argument it commits one of the faults a process must not
// survive — and the rest of the system must — and never returns.
package main

import (
	"runtime"

	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/userspace/commands"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "vmprobe [read-only|guard|unmapped|quota]",
	Summary: "native memory/fault checks",
	MaxArgs: 1,
}

const page = uint64(4096)

// bss is a large zero-initialized global: it lives in the image's BSS, which
// the kernel supplies on demand rather than at launch.
var bss [4 << 20]byte

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	if data.ArgCount() == 1 {
		mode, _ := data.Arg(1)
		switch {
		case utility.Equal(mode, "read-only"):
			faultReadOnly()
		case utility.Equal(mode, "guard"):
			recurse(1 << 30)
		case utility.Equal(mode, "unmapped"):
			// One page past the end of a fresh reservation is in no region.
			base := reserve(page, kcall.VMRead|kcall.VMWrite)
			sys.VMUnmap(base, page)
			sys.Window(base, page)[0] = 1
		case utility.Equal(mode, "quota"):
			base := reserve(64<<20, kcall.VMRead|kcall.VMWrite)
			memory := sys.Window(base, 64<<20)
			for offset := uint64(0); offset < 64<<20; offset += page {
				memory[offset] = 1
			}
		default:
			utility.Usage()
		}
		utility.Fail(nil, "the fault did not happen")
	}
	checkDemandZero()
	checkProtectDiscardUnmap()
	checkRules()
	checkPinned()
	checkHeapBSSStack()
	utility.PrintText("vm: all checks passed")
	utility.EndLine()
	utility.Mark("VM_PROBE_OK\n")
}

func reserve(length uint64, protection uint64) uint64 {
	base, status := sys.VMReserve(length, protection)
	if status != kcall.StatusOK {
		failStatus("reserve", status)
	}
	return base
}

func failStatus(what string, status kcall.Status) {
	utility.PrintText("vm: ")
	utility.PrintText(what)
	utility.PrintText(" failed, status ")
	utility.PrintUint(uint64(status))
	utility.EndLine()
	utility.Exit(1)
}

func check(ok bool, what string) {
	if !ok {
		utility.PrintText("vm: ")
		utility.PrintText(what)
		utility.PrintText(" failed")
		utility.EndLine()
		utility.Exit(1)
	}
	utility.PrintText("vm: ")
	utility.PrintText(what)
	utility.EndLine()
}

// checkDemandZero reserves 64 MiB, touches pages scattered through it, and
// requires that untouched pages read as zero and cost nothing.
func checkDemandZero() {
	const size = uint64(64 << 20)
	// A program started from a file has its code copied in as it first runs
	// (ADR 0106); one round of the calls measured below brings theirs in
	// first, so the measurement counts only what the reservation costs.
	sys.VMUnmap(reserve(size, kcall.VMRead|kcall.VMWrite), size)
	sys.VMQuery(0)
	pagesUsed()
	before := pagesUsed()
	base := reserve(size, kcall.VMRead|kcall.VMWrite)
	reserved := pagesUsed()
	region, status := sys.VMQuery(base)
	check(status == kcall.StatusOK && region.Base == base && region.Length == size && region.Backing == 1 && !region.Pinned, "anonymous region reserved")
	check(reserved == before, "reservation costs no pages")
	memory := sys.Window(base, size)
	for offset := uint64(0); offset < size; offset += 1 << 20 {
		memory[offset] = byte(offset >> 20)
	}
	memory[size-1] = 0xff
	ok := memory[size-2] == 0 && memory[page] == 0 && memory[size-1] == 0xff
	for offset := uint64(0); offset < size; offset += 1 << 20 {
		ok = ok && memory[offset] == byte(offset>>20) && memory[offset+8] == 0
	}
	check(ok, "demand-zero pages")
	// Sixty-four pages, a page table for each two MiB they are spread over,
	// the directories above them, and any code of this loop run for the
	// first time.
	grown := pagesUsed() - before
	check(grown >= 64 && grown <= 64+32+4+8, "touched pages charged, untouched not")
	// Many consecutive pages.
	for offset := uint64(0); offset < 256*page; offset++ {
		memory[offset] = byte(offset * 7)
	}
	ok = true
	for offset := uint64(0); offset < 256*page; offset++ {
		ok = ok && memory[offset] == byte(offset*7)
	}
	check(ok, "multi-page mapping")
	// 64 scattered pages, the last page, and 255 more of the first MiB. The
	// page tables stay with the address space until the process ends.
	touched := pagesUsed()
	check(sys.VMUnmap(base, size) == kcall.StatusOK && touched-pagesUsed() == 320, "unmap returns the pages")
}

func checkProtectDiscardUnmap() {
	base := reserve(16*page, kcall.VMRead|kcall.VMWrite)
	memory := sys.Window(base, 16*page)
	for offset := uint64(0); offset < 16*page; offset += page {
		memory[offset] = 0x5a
	}
	check(sys.VMProtect(base+4*page, 4*page, kcall.VMRead) == kcall.StatusOK, "protect a range read-only")
	region, _ := sys.VMQuery(base + 5*page)
	check(region.Base == base+4*page && region.Length == 4*page && region.Protection == kcall.VMRead, "protection splits the region")
	check(memory[4*page] == 0x5a, "read-only pages stay readable")
	check(sys.VMProtect(base+4*page, 4*page, kcall.VMRead|kcall.VMWrite) == kcall.StatusOK, "protect back to read-write")
	memory[4*page] = 0x5b
	check(sys.VMDiscard(base, 8*page) == kcall.StatusOK && memory[0] == 0 && memory[4*page] == 0 && memory[8*page] == 0x5a, "discard reads as zero again")
	check(sys.VMUnmap(base+2*page, 2*page) == kcall.StatusOK, "unmap a hole")
	_, status := sys.VMQuery(base + 2*page)
	check(status == kcall.StatusNotFound, "the hole is in no region")
	low, _ := sys.VMQuery(base)
	high, _ := sys.VMQuery(base + 4*page)
	check(low.Length == 2*page && high.Base == base+4*page, "unmap splits the region")
	check(sys.VMUnmap(base, 16*page) == kcall.StatusOK, "unmap across the hole")
}

func checkRules() {
	_, status := sys.VMReserve(page, kcall.VMRead|kcall.VMWrite|kcall.VMExecute)
	check(status != kcall.StatusOK, "writable executable memory refused")
	_, status = sys.VMReserve(page, kcall.VMRead|kcall.VMExecute)
	check(status != kcall.StatusOK, "executable anonymous memory refused")
	base := reserve(4*page, kcall.VMRead|kcall.VMWrite)
	check(sys.VMProtect(base, page, kcall.VMRead|kcall.VMExecute) != kcall.StatusOK, "protect to executable refused")
	check(sys.VMReserveAt(base+page, page, kcall.VMRead) == kcall.StatusAlreadyExists, "overlapping fixed reservation refused")
	check(sys.VMReserveAt(base+1, page, kcall.VMRead) == kcall.StatusInvalidArgument, "unaligned address refused")
	check(sys.VMReserveAt(0xffff800000000000, page, kcall.VMRead) == kcall.StatusInvalidArgument, "kernel address refused")
	check(sys.VMReserveAt(vm.UserAddressLimit-page, 2*page, kcall.VMRead) == kcall.StatusInvalidArgument, "reservation past the user range refused")
	check(sys.VMReserveAt(base+64*page, ^uint64(0)&^(page-1), kcall.VMRead) == kcall.StatusInvalidArgument, "wrapping length refused")
	_, status = sys.VMReserve(0, kcall.VMRead)
	check(status == kcall.StatusInvalidArgument, "empty reservation refused")
	check(sys.VMUnmap(base+1, page) != kcall.StatusOK && sys.VMDiscard(base, 100) != kcall.StatusOK, "unaligned unmap and discard refused")
	sys.VMUnmap(base, 4*page)
}

func checkPinned() {
	code, status := sys.VMQuery(vm.UserCodeAddress)
	check(status == kcall.StatusOK && code.Pinned && code.Backing == 2 && code.Protection == kcall.VMRead|kcall.VMExecute, "image code is a pinned region")
	check(sys.VMUnmap(vm.UserCodeAddress, page) == kcall.StatusPermissionDenied, "image unmap refused")
	check(sys.VMProtect(vm.UserCodeAddress, page, kcall.VMRead|kcall.VMWrite) == kcall.StatusPermissionDenied, "image reprotect refused")
	stack, status := sys.VMQuery(vm.UserStackTop - page)
	check(status == kcall.StatusOK && stack.Pinned && stack.Backing == 1 && stack.Base+stack.Length == vm.UserStackTop, "stack is a pinned reservation")
	guard, status := sys.VMQuery(stack.Base - page)
	check(status == kcall.StatusOK && guard.Backing == 5 && guard.Protection == 0, "stack guard below the reservation")
	check(sys.VMDiscard(stack.Base, page) == kcall.StatusPermissionDenied, "stack discard refused")
}

func checkHeapBSSStack() {
	before := pagesUsed()
	grown := touchLargeObject()
	stats := arena.HeapStats()
	check(grown >= before+2048 && stats.Live >= 8<<20, "heap grows past its old 128 KiB")
	runtime.GC()
	after := pagesUsed()
	collected := arena.HeapStats()
	check(collected.Collections > stats.Collections && collected.Live+8<<20 <= stats.Live, "a dropped heap object is collected")
	check(after+2048-64 <= grown && collected.Released >= stats.Released+8<<20, "collected heap pages go back to the kernel")
	for index := 0; index < len(bss); index += int(page) {
		if bss[index] != 0 {
			check(false, "large BSS starts zero")
		}
		bss[index] = 1
	}
	check(bss[len(bss)-int(page)] == 1, "large BSS starts zero and is writable")
	check(recurse(32) == 32, "a 512 KiB stack fits the default reservation")
}

// touchLargeObject allocates an 8 MiB object, touches every page, and returns
// the pages charged while it is live. The object is garbage once it returns.
//
//go:noinline
func touchLargeObject() uint64 {
	large := make([]byte, 8<<20)
	for index := 0; index < len(large); index += int(page) {
		large[index] = byte(index)
	}
	if large[len(large)-int(page)] != byte(len(large)-int(page)) {
		check(false, "heap object holds its bytes")
	}
	return pagesUsed()
}

// recurse uses a 16 KiB frame per level, touching all of it, so a depth of 32
// needs half a MiB of stack and an unbounded depth overflows into the guard.
//
//go:noinline
func recurse(depth int) int {
	var frame [16 << 10]byte
	for index := 0; index < len(frame); index += int(page) {
		frame[index] = byte(depth)
	}
	if depth <= 1 {
		return int(frame[0])
	}
	return recurse(depth-1) + int(frame[len(frame)-int(page)]) - depth + 1
}

// faultReadOnly writes to a page it made read-only.
func faultReadOnly() {
	base := reserve(page, kcall.VMRead|kcall.VMWrite)
	memory := sys.Window(base, page)
	memory[0] = 1
	if sys.VMProtect(base, page, kcall.VMRead) != kcall.StatusOK {
		utility.Fail(nil, "protect failed")
	}
	memory[1] = 2
}

// pagesUsed is the page count this process is charged. The one process the
// kernel reports as running is the caller.
func pagesUsed() uint64 {
	var record [kcall.ProcessInfoSize]byte
	for index := uint64(0); utility.ProcessInfo(index, &record); index++ {
		if record[8] == kcall.ProcessRunning {
			used := record[kcall.ProcessInfoUsedOffset+4*kcall.ProcessPages:]
			return uint64(used[0]) | uint64(used[1])<<8 | uint64(used[2])<<16 | uint64(used[3])<<24
		}
	}
	return 0
}
