//go:build goos_userspace

package main

import (
	"encoding/binary"

	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
)

// requestsServed counts requests for the memory report.
var requestsServed uint64

// reportMemory writes the service's heap state and the pages it holds after
// every 1024 requests, so a long run shows whether its memory settles
// (ADR 0201), and the allocator profile after every 16384. Only validation
// builds call it.
func reportMemory() {
	requestsServed++
	if requestsServed%1024 != 0 {
		return
	}
	stats := arena.HeapStats()
	write("VFS_MEMORY requests=")
	writeDecimal(requestsServed)
	write(" live=")
	writeDecimal(stats.Live)
	write(" objects=")
	writeDecimal(stats.Objects)
	write(" collections=")
	writeDecimal(uint64(stats.Collections))
	write(" pages=")
	writeDecimal(ownPages())
	write("\n")
	// The profile is four console lines. The boot test reads the console
	// for the shell's prompts, and a service line printed as a prompt
	// appears can join it, so the profile comes rarely.
	if requestsServed%16384 == 0 {
		arena.ReadDiagnostics(&profile)
		profile.AppendProfile("vfs", profileLine[:], writeProfileLine)
	}
}

// profile and profileLine hold the allocator profile the report adds every
// fourth time, as globals so that reporting allocates nothing.
var (
	profile     arena.Diagnostics
	profileLine [160]byte
)

func writeProfileLine(line []byte) { sys.ConsoleWrite(consoleHandle, line) }

// ownPages is the page count the kernel charges this process: the one it
// reports as running.
func ownPages() uint64 {
	var record [kcall.ProcessInfoSize]byte
	for index := uint64(0); sys.ProcessInfo(index, &record) == kcall.StatusOK; index++ {
		if record[8] == kcall.ProcessRunning {
			return uint64(binary.LittleEndian.Uint32(record[kcall.ProcessInfoUsedOffset+kcall.ProcessPages*4:]))
		}
	}
	return 0
}

func writeDecimal(value uint64) {
	var digits [20]byte
	index := len(digits)
	for {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	sys.ConsoleWrite(consoleHandle, digits[index:])
}
