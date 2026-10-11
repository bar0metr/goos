//go:build goos_userspace

package main

import (
	"goos/kernel/handle"
	"goos/userspace/runtime/sys"
	shellcmd "goos/userspace/shell"
)

var oneShot bool
var inputHandle, errorHandle handle.ID
var commandStatus uint64

// writeError preserves interactive presentation but separates exec diagnostics.
func writeError(message string) { writeErrorBytes(sys.Bytes(message)) }
func writeErrorBytes(value []byte) {
	if commandStatus == 0 {
		commandStatus = shellcmd.StatusFailure
	}
	if !oneShot {
		writeBytes(value)
		return
	}
	flushOutput()
	if !writeStream(errorHandle, value, outputDeadline) {
		commandStatus = shellcmd.StatusFailure
	}
}

func runOneShot(command []byte) {
	size := copy(lineStorage[:], command)
	status := execute(lineStorage[:size])
	flushOutput()
	if commandStatus != 0 && status == 0 {
		status = commandStatus
	}
	sys.Exit(status)
}
