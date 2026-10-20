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
		// Processes' pages move with their collected heaps; what the
		// kernel holds for itself does not (ADR 0201).
		used, processes, stable := stablePages(&info)
		utility.PrintText("pages used: ")
		utility.PrintUint(used)
		utility.EndLine()
		utility.PrintText("pages in processes: ")
		utility.PrintUint(processes)
		utility.EndLine()
		utility.PrintText("pages in the kernel: ")
		utility.PrintUint(used - min(used, processes))
		utility.EndLine()
		// A test compares the counts only when they settled; an unsettled
		// sample says nothing about a leak.
		if stable {
			utility.PrintText("pages stable: yes")
		} else {
			utility.PrintText("pages stable: no")
		}
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

// stablePages samples the pages in use and the pages processes are charged
// until two samples in a row agree, at most eight times, and reports whether
// they did. The two counts come from separate calls, one per process for the
// second, and a service that collects or touches a page between them would
// move one without the other; nothing is printed while sampling, since the
// output itself wakes services. Agreeing samples are not one atomic reading,
// but a count that moved between them is not taken for a settled one.
func stablePages(info *[kcall.SystemInfoSize]byte) (used, processes uint64, stable bool) {
	used, processes = usedPages(info), processPages()
	for attempt := 0; attempt < 8; attempt++ {
		nextUsed, nextProcesses := usedPages(info), processPages()
		if nextUsed == used && nextProcesses == processes && usedPages(info) == used {
			return used, processes, true
		}
		used, processes = nextUsed, nextProcesses
	}
	return used, processes, false
}

func usedPages(info *[kcall.SystemInfoSize]byte) uint64 {
	utility.SystemInfo(info)
	return binary.LittleEndian.Uint64(info[16:24]) - binary.LittleEndian.Uint64(info[24:32])
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
