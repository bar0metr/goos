//go:build goos_userspace

// Command ps lists processes with their memory and CPU use.
package main

import (
	"encoding/binary"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "ps",
	Summary: "list processes with their memory and CPU use",
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(startup.Data) {
	var record [kcall.ProcessInfoSize]byte
	utility.PrintText("PID   TYPE     STATE      MEM  CPU   COMMAND")
	utility.EndLine()
	for index := uint64(0); utility.ProcessInfo(index, &record); index++ {
		pid := binary.LittleEndian.Uint64(record[0:8])
		state := "waiting"
		switch record[8] {
		case kcall.ProcessRunning:
			state = "running"
		case kcall.ProcessReady:
			state = "ready"
		}
		pages := binary.LittleEndian.Uint32(record[kcall.ProcessInfoUsedOffset+kcall.ProcessPages*4:])
		cpu := binary.LittleEndian.Uint64(record[kcall.ProcessInfoCPUOffset:])
		utility.Pad(utility.PrintUint(pid), 6)
		utility.PrintText("native   ")
		utility.PrintText(state)
		utility.Pad(len(state), 11)
		// MEM is the pages the kernel allocated for the process; CPU is the
		// timer ticks it was found running on.
		utility.Pad(utility.PrintUint(uint64(pages)), 5)
		utility.Pad(utility.PrintUint(cpu), 6)
		utility.Print(processName(record[:]))
		utility.EndLine()
	}
	utility.Mark("PS_PROCESS_OK\n")
}

func processName(record []byte) []byte {
	name := record[16 : 16+kcall.MaxProcessName]
	size := 0
	for size < len(name) && name[size] != 0 {
		size++
	}
	return name[:size]
}
