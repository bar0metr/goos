//go:build goos_userspace

// Command mem shows physical memory use.
package main

import (
	"encoding/binary"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/shell"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "mem [-p]",
	Summary: "physical memory use (-p: pages)",
	MaxArgs: 1,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	var info [kcall.SystemInfoSize]byte
	utility.SystemInfo(&info)
	// -p prints the exact count of pages in use, which a test compares
	// across repetitions of a workload to find what was not freed.
	if flag, given := data.Arg(1); given {
		if !utility.Equal(flag, "-p") {
			utility.Usage()
		}
		used := binary.LittleEndian.Uint64(info[16:24]) - binary.LittleEndian.Uint64(info[24:32])
		utility.PrintText("pages used: ")
		utility.PrintUint(used)
		utility.EndLine()
		// Processes' pages move with their collected heaps; what the
		// kernel holds for itself does not (ADR 0201).
		processes := processPages()
		utility.PrintText("pages in processes: ")
		utility.PrintUint(processes)
		utility.EndLine()
		utility.PrintText("pages in the kernel: ")
		utility.PrintUint(used - min(used, processes))
		utility.EndLine()
		return
	}
	// total is the memory the page allocator manages, not the machine's RAM:
	// firmware, the kernel image and unmanaged regions are outside it.
	total := binary.LittleEndian.Uint64(info[16:24]) * kcall.SystemInfoPageSize
	free := binary.LittleEndian.Uint64(info[24:32]) * kcall.SystemInfoPageSize
	line("total: ", total)
	line("used:  ", total-free)
	line("free:  ", free)
	utility.Mark("MEM_PROCESS_OK\n")
}

// processRecord is a global so that counting allocates nothing.
var processRecord [kcall.ProcessInfoSize]byte

// processPages is the sum of the pages the kernel charges every process.
func processPages() uint64 {
	total := uint64(0)
	for index := uint64(0); utility.ProcessInfo(index, &processRecord); index++ {
		total += uint64(binary.LittleEndian.Uint32(processRecord[kcall.ProcessInfoUsedOffset+kcall.ProcessPages*4:]))
	}
	return total
}

func line(label string, bytes uint64) {
	var text [24]byte
	utility.PrintText(label)
	utility.Print(shell.FormatSize(bytes, &text))
	utility.EndLine()
}
