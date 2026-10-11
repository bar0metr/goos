//go:build goos_userspace

// Command quota shows each process's share of kernel resources.
package main

import (
	"encoding/binary"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "quota",
	Summary: "process kernel quotas",
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

// run prints every process's resource account: what it holds of each bounded
// kernel resource and the share it may hold.
func run(startup.Data) {
	var record [kcall.ProcessInfoSize]byte
	names := [kcall.ProcessResources]string{"chan", "strm", "task", "page"}
	utility.PrintText("PID   COMMAND           RESOURCE  HELD/LIMIT")
	utility.EndLine()
	for index := uint64(0); utility.ProcessInfo(index, &record); index++ {
		pid := binary.LittleEndian.Uint64(record[0:8])
		name := processName(record[:])
		for resource := 0; resource < kcall.ProcessResources; resource++ {
			held := binary.LittleEndian.Uint32(record[kcall.ProcessInfoUsedOffset+resource*4:])
			limit := binary.LittleEndian.Uint32(record[kcall.ProcessInfoLimitsOffset+resource*4:])
			utility.Pad(utility.PrintUint(pid), 6)
			utility.Print(name)
			utility.Pad(len(name), 18)
			utility.PrintText(names[resource])
			utility.Pad(len(names[resource]), 10)
			utility.PrintUint(uint64(held))
			utility.PrintText("/")
			utility.PrintUint(uint64(limit))
			utility.EndLine()
		}
	}
	utility.Mark("QUOTA_PROCESS_OK\n")
}

func processName(record []byte) []byte {
	name := record[16 : 16+kcall.MaxProcessName]
	size := 0
	for size < len(name) && name[size] != 0 {
		size++
	}
	return name[:size]
}
