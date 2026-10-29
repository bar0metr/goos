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

// linuxTerminalEntries builds the terminal environment of an interactive
// Linux launch from the session: the current size and terminal type. They
// replace whatever the request carried, which may be stale.
func linuxTerminalEntries(snapshot *tty.Snapshot, storage *[3][32]byte) [][]byte {
	entries := make([][]byte, 0, 3)
	geometry, valid := snapshot.Current()
	if valid && geometry.Rows > 0 && geometry.Columns > 0 {
		entries = append(entries, appendDecimal(append(storage[0][:0], "COLUMNS="...), uint64(geometry.Columns)))
		entries = append(entries, appendDecimal(append(storage[1][:0], "LINES="...), uint64(geometry.Rows)))
	}
	term := storage[2][:0]
	term = append(term, "TERM="...)
	if valid && len(geometry.Term) != 0 && len(geometry.Term) <= len(storage[2])-len("TERM=") {
		term = append(term, geometry.Term...)
	} else {
		term = append(term, linuxTerminalDefaultTerm...)
	}
	return append(entries, term)
}

// linuxTerminalRole is the startup name the kernel accepts for the control
// stream; it matches the kernel's own constant.
const linuxTerminalRole = "linux-tty"

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
