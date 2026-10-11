//go:build goos_userspace

package main

// Terminal sessions: one terminal service and one shell per console
// (ADR 0089, ADR 0125). COM1 and the framebuffer with its PS/2 keyboard are
// separate consoles of their own sizes, so each gets its own pair, as a
// serial line and a virtual console each get a login in other systems. A
// session's services restart independently of each other and of the other
// session's.

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	supervisor "goos/userspace/init"
	"goos/userspace/runtime/sys"
	"goos/userspace/tty"
)

// serialConsoleHandle and graphicalConsoleHandle are the two terminal
// consoles, each with the right to read its input; secondExecPageHandle is
// the exec page of the second session's shell.
const (
	serialConsoleHandle    = handle.ID(0x0000000100000019)
	graphicalConsoleHandle = handle.ID(0x000000010000001a)
	secondExecPageHandle   = handle.ID(0x000000010000001b)
)

// terminalSession is one console's terminal service and shell and what PID 1
// keeps for them.
type terminalSession struct {
	geometry    tty.Snapshot
	resizeOwner supervisor.ResizeOwner
	// tty and shell are the services' names.
	tty, shell string
	// console is the terminal console the terminal service reads and writes.
	console handle.ID
	// output is the stream the shell and its commands write to and the
	// terminal service reads. PID 1 keeps both ends for as long as it runs:
	// the reader survives the service, so output written while it restarts
	// waits in the stream.
	output [2]uint64
	// ttyControl is PID 1's end of the terminal service's control channel,
	// over which a restarted shell's session is handed to a running terminal.
	ttyControl handle.ID
	// execServer is the end of the shell's exec channel PID 1 serves, or zero
	// while the shell is gone; execClient the shell's end at its first launch.
	execServer uint64
	execClient handle.ID
	// page is the area shared with the shell that carries its exec requests.
	pageHandle         handle.ID
	page               []byte
	pageBase, pageSize uint64
	shellPID           uint64
	// ttyStarted and shellStarted mark the first launches, which use the
	// endpoints created at boot; every later launch renews them.
	ttyStarted, shellStarted bool
	// The validation build keeps the services' process handles to inject the
	// faults that prove each restarts on its own.
	ttyProcess, shellProcess handle.ID
	active                   bool
}

// sessions are COM1's, which the boot test drives, and the framebuffer's. The
// framebuffer's exists only when the machine has one.
var sessions = [...]terminalSession{
	{tty: "tty", shell: "shell", console: serialConsoleHandle, execServer: execServerBoot, execClient: execClientHandle, pageHandle: execPageHandle},
	{tty: "tty-fb", shell: "shell-fb", console: graphicalConsoleHandle, pageHandle: secondExecPageHandle},
}

// replying is the session whose exec request is being handled: control
// replies go back to its shell.
var replying = &sessions[0]

// sessionOfService returns the session a service belongs to and whether it is
// that session's terminal service rather than its shell.
func sessionOfService(name string) (*terminalSession, bool, bool) {
	for index := range sessions {
		session := &sessions[index]
		if !session.active {
			continue
		}
		if name == session.tty {
			return session, true, true
		}
		if name == session.shell {
			return session, false, true
		}
	}
	return nil, false, false
}

// graphicalConsolePresent reports whether the machine has a framebuffer
// console to run the second session on.
func graphicalConsolePresent() bool {
	info, status := sys.ConsoleInformation(graphicalConsoleHandle)
	return status == kcall.StatusOK && info.Flags&kcall.ConsoleFlagFramebuffer != 0
}

// mapSessionPage maps a session's exec page: the first at the address
// the exec protocol fixes, the second wherever there is room. Each shell maps
// its own at the fixed address in its own space.
func mapSessionPage(session *terminalSession, fixed bool, address uint64, size uint64) bool {
	if fixed {
		page, status := sys.MemoryMap(session.pageHandle, address, size, true)
		session.page = page
		return status == kcall.StatusOK
	}
	base, mapped, status := sys.MemoryMapAnywhere(session.pageHandle, true)
	if status != kcall.StatusOK || mapped < size {
		return false
	}
	session.page = sys.Window(base, size)
	return true
}
