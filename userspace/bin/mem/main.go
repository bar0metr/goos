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
	Usage:   "mem",
	Summary: "show physical memory use",
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(startup.Data) {
	var info [kcall.SystemInfoSize]byte
	utility.SystemInfo(&info)
	// total is the memory the page allocator manages, not the machine's RAM:
	// firmware, the kernel image and unmanaged regions are outside it.
	total := binary.LittleEndian.Uint64(info[16:24]) * kcall.SystemInfoPageSize
	free := binary.LittleEndian.Uint64(info[24:32]) * kcall.SystemInfoPageSize
	line("total: ", total)
	line("used:  ", total-free)
	line("free:  ", free)
	utility.Mark("MEM_PROCESS_OK\n")
}

func line(label string, bytes uint64) {
	var text [24]byte
	utility.PrintText(label)
	utility.Print(shell.FormatSize(bytes, &text))
	utility.EndLine()
}
