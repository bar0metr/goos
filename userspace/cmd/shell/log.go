//go:build goos_userspace

package main

import (
	"goos/internal/logfmt"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	shellcmd "goos/userspace/shell"
	terminal "goos/userspace/tty"
)

// Large snapshot/interest storage is static: TinyGo's bump heap cannot reclaim
// per-command allocations, and neither kernel nor userspace has a large stack.
var (
	logSnapshot  [kcall.LogReadSize]byte
	logSelection [kcall.LogReadMax]uint8
	logInterests [kcall.WaitAnyMaxObjects]sys.CursorInterest
	logText      [logfmt.MaxText]byte
)

func printLog(invocation shellcmd.Invocation) uint64 {
	options, valid := shellcmd.ParseLog(invocation)
	if !valid {
		writeError("usage: " + shellcmd.LogUsage + "\n")
		return 2
	}
	if options.Follow && ttyHandle == 0 {
		writeError("log: -f requires an interactive terminal\n")
		return 2
	}
	flushOutput()
	if options.Follow && reading == 0 {
		requestLine(nil)
	}
	status := shellcmd.RunLog(options, shellcmd.LogHooks{Read: readLogSnapshot, Write: writeLogRecord, Gap: writeLogGap, Check: checkLogInput, Wait: waitLogInput}, &logSelection)
	if status == 1 {
		writeError("log: read, output or wait failed\n")
	}
	return status
}

func readLogSnapshot(cursor uint64, snapshot bool) (sys.LogBatch, bool) {
	if snapshot {
		oldest, _, status := sys.LogBounds(logHandle)
		if status != kcall.StatusOK {
			return sys.LogBatch{}, false
		}
		cursor = oldest
	}
	batch, status := sys.LogReadSequence(logHandle, cursor, &logSnapshot)
	return batch, status == kcall.StatusOK
}

func writeLogRecord(record logfmt.Record) bool {
	size := logfmt.Format(record, logText[:])
	return size != 0 && writeStream(outputHandle, logText[:size], outputDeadline)
}

func writeLogGap(lost uint64) bool {
	size := copy(logText[:], "log: overwritten ")
	text := logfmt.AppendUint(logText[:size], lost)
	size = len(text)
	size += copy(logText[size:], " records\n")
	writeErrorBytes(logText[:size])
	return true
}

func checkLogInput() (uint64, bool) {
	pollReplies()
	if ttyHandle == 0 {
		return 1, true
	}
	if reading == 0 {
		requestLine(nil)
	}
	result, _, received := takeLine()
	if ttyHandle == 0 {
		return 1, true
	}
	if !received {
		return 0, false
	}
	switch result {
	case terminal.ResultInterrupt:
		return 130, true
	case terminal.ResultEndOfInput:
		session, _ := startupData.Env("GOOS_SESSION")
		if shellcmd.SessionEndsOnEOF(session, result) {
			sys.Exit(0)
		}
		return 0, true
	}
	if reading == 0 {
		requestLine(nil)
	}
	return 0, false
}

func waitLogInput(cursor uint64) bool {
	if ttyHandle == 0 {
		return false
	}
	logInterests[0] = sys.CursorInterest{Handle: logHandle, Events: kcall.WaitReadable, Cursor: cursor}
	logInterests[1] = sys.CursorInterest{Handle: ttyHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
	count := 2
	if execHandle != 0 {
		logInterests[2] = sys.CursorInterest{Handle: execHandle, Events: kcall.WaitReadable}
		count++
	}
	_, status := sys.WaitAnyCursor(&logInterests, count, kcall.WaitAnyInfiniteTimeout)
	return status == kcall.StatusOK
}
