//go:build goos_userspace

package main

// The terminal of an interactive Linux job (ADR 0206). A Linux program that
// is the only stage of a foreground job gets its terminal from the kernel,
// built from the job's input, the session's output, and one more stream PID 1
// keeps the writer of: the control stream, which carries the session's
// geometry as fixed terminalwire records. The program never holds a native
// handle; the kernel consumes all of them at launch.

import (
	"goos/internal/terminalwire"
	"goos/kernel/handle"
	execproto "goos/userspace/exec"
	"goos/userspace/runtime/sys"
	"goos/userspace/tty"
)

// launchingLinuxTerminal is the job whose only stage is being launched, set
// while the stage may become an interactive Linux program.
var launchingLinuxTerminal *execJob

// linuxTerminalDefaultTerm names the GoOS console to ncurses when the session
// reports no terminal type of its own.
const linuxTerminalDefaultTerm = "goos"

// linuxTerminalInfo is where the Linux application tree keeps its terminfo
// entries.
const linuxTerminalInfo = "TERMINFO=/system/linux/terminfo"

var linuxGeometryWire [terminalwire.RecordSize]byte

// sendLinuxGeometry writes the session's current geometry to a job's control
// stream. A full stream means the kernel has not taken the older records;
// it reads the newest one, so a record that does not fit is not needed yet.
func sendLinuxGeometry(writer uint64, snapshot *tty.Snapshot) {
	if writer == 0 {
		return
	}
	geometry, valid := snapshot.Current()
	if !valid || geometry.Rows <= 0 || geometry.Columns <= 0 || geometry.Rows > 0xffff || geometry.Columns > 0xffff {
		return
	}
	record := terminalwire.Geometry{Rows: uint16(geometry.Rows), Columns: uint16(geometry.Columns), Sequence: geometry.Sequence}
	if !terminalwire.Encode(record, &linuxGeometryWire) {
		return
	}
	sys.StreamWrite(handle.ID(writer), linuxGeometryWire[:])
}

// linuxTerminalEntry builds the TERM of an interactive Linux launch from the
// session. COLUMNS and LINES stay out: ncurses prefers them to TIOCGWINSZ, so
// they would pin a program to its starting size through every SIGWINCH.
func linuxTerminalEntry(snapshot *tty.Snapshot, storage *[32]byte) []byte {
	geometry, valid := snapshot.Current()
	term := append(storage[:0], "TERM="...)
	if valid && len(geometry.Term) != 0 && len(geometry.Term) <= len(storage)-len("TERM=") {
		return append(term, geometry.Term...)
	}
	return append(term, linuxTerminalDefaultTerm...)
}

// environmentHas reports whether a request names a variable, given with its
// equals sign.
func environmentHas(environment execproto.List, name string) bool {
	for offset, index := 0, 0; index < environment.Count(); index++ {
		var entry []byte
		entry, offset = environment.Next(offset)
		if len(entry) >= len(name) && string(entry[:len(name)]) == name {
			return true
		}
	}
	return false
}
