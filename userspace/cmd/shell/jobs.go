//go:build goos_userspace

package main

// Job tracking: foreground and background jobs, their replies, and stopping
// them (ADR 0064, 0067).

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	execproto "goos/userspace/exec"
	"goos/userspace/runtime/sys"
	terminal "goos/userspace/tty"
)

func printJobs() {
	found := false
	for index := range jobs {
		if !jobs[index].active || !jobs[index].background {
			continue
		}
		found = true
		write("[")
		writeUint(uint64(index) + 1)
		write("] running  ")
		write(string(jobs[index].command[:jobs[index].commandSize]))
		write("\n")
	}
	if !found {
		write("no background jobs\n")
	}
	mark("SHELL_JOBS_OK\n")
}

// killJob stops one background job, named as N or %N.
func killJob(argument []byte) {
	if len(argument) != 0 && argument[0] == '%' {
		argument = argument[1:]
	}
	number, ok := parseUint(argument)
	if !ok || number == 0 || number > uint64(len(jobs)) {
		write("kill: invalid job\n")
		return
	}
	slot := number - 1
	if !jobs[slot].active || !jobs[slot].background {
		write("kill: no such job\n")
		return
	}
	reply, ok := controlExchange(execproto.ControlKill, uint8(slot))
	status, valid := execproto.DecodeControlStatus(reply)
	if !ok || !valid || status != execproto.ControlOK {
		write("kill: job could not be stopped\n")
		return
	}
	mark("SHELL_KILL_OK\n")
}

func freeSlot(background bool) (uint8, bool) {
	free, count := -1, 0
	for index := range jobs {
		if jobs[index].active {
			count++
		} else if free < 0 {
			free = index
		}
	}
	if free < 0 || background && count >= len(jobs)-1 {
		return 0, false
	}
	return uint8(free), true
}

// awaitReply receives until the reply with correlation arrives, reporting
// background jobs that finish in the meantime.
//
// While a foreground job runs (foreground is its slot), a read stays
// outstanding at the terminal service: a typed line is written to the job's
// standard input, Ctrl-C stops the job, and Ctrl-D ends its input. The job's
// input arrives from PID 1 in a started message before its reply.
func awaitReply(correlation uint64, foreground int) (ipc.Message, bool) {
	var input handle.ID
	for {
		if stashedValid && stashed.Correlation == correlation {
			stashedValid = false
			closeInput(&input)
			return stashed, true
		}
		if foreground >= 0 {
			if reading == 0 {
				requestLine(nil)
			}
			if result, text, ok := takeLine(); ok {
				switch result {
				case terminal.ResultLine:
					if input != 0 && !feedInput(input, text) {
						closeInput(&input)
					}
				case terminal.ResultInterrupt:
					controlExchange(execproto.ControlKill, uint8(foreground))
				case terminal.ResultEndOfInput:
					closeInput(&input)
				}
				continue
			}
		}
		status := sys.IPCReceive(execHandle, wire[:])
		if status == ipc.StatusNotFound {
			awaitInput()
			continue
		}
		if status != 0 {
			write("exec: service unavailable\n")
			closeInput(&input)
			return ipc.Message{}, false
		}
		message, valid := decodeWire()
		if !valid {
			write("exec: corrupt reply\n")
			closeInput(&input)
			return ipc.Message{}, false
		}
		if session, ok := terminal.DecodeSessionNotice(&message); ok {
			installTerminal(session)
			continue
		}
		if _, started, job, ok := execproto.DecodeStarted(message); ok {
			if job == correlation && input == 0 {
				input = started
			} else {
				sys.HandleClose(started)
			}
			continue
		}
		if message.Correlation == correlation {
			closeInput(&input)
			return message, true
		}
		if !reportJob(message) && isForeground(message.Correlation) {
			stashed, stashedValid = message, true
		}
	}
}

// isForeground reports whether correlation names a running foreground job.
func isForeground(correlation uint64) bool {
	for index := range jobs {
		if jobs[index].active && !jobs[index].background && jobs[index].correlation == correlation {
			return true
		}
	}
	return false
}

// closeInput ends a foreground job's input, if it still has one.
func closeInput(input *handle.ID) {
	if *input != 0 {
		sys.HandleClose(*input)
		*input = 0
	}
}

// pollReplies reports finished background jobs and takes a new terminal
// session without blocking. It reports whether the session was replaced, which
// needs the prompt asked for again.
func pollReplies() bool {
	replaced := false
	for sys.IPCReceive(execHandle, wire[:]) == 0 {
		message, valid := decodeWire()
		if !valid {
			continue
		}
		if session, ok := terminal.DecodeSessionNotice(&message); ok {
			installTerminal(session)
			replaced = true
			continue
		}
		reportJob(message)
	}
	return replaced
}

func reportJob(message ipc.Message) bool {
	for index := range jobs {
		if !jobs[index].active || !jobs[index].background || jobs[index].correlation != message.Correlation {
			continue
		}
		jobs[index].active = false
		write("[")
		writeUint(uint64(index) + 1)
		reply, valid := execproto.DecodeReply(message)
		switch {
		case !valid:
			write("] corrupt reply\n")
		case reply.LaunchStatus != 0:
			write("] ")
			writeLaunchFailure(index, reply)
		case reply.ExitCode == kcall.ExitKilled:
			write("] killed\n")
		case reply.ExitCode != 0:
			write("] exit status: ")
			writeUint(reply.ExitCode)
			write("\n")
		default:
			write("] done\n")
		}
		return true
	}
	return false
}

func decodeWire() (ipc.Message, bool) {
	payloadSize := int(wire[24]) | int(wire[25])<<8
	size := ipc.WireHeaderSize + payloadSize + int(wire[26])*8
	if size > len(wire) {
		return ipc.Message{}, false
	}
	return ipc.Decode(wire[:size])
}
