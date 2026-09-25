//go:build goos_userspace

package main

// Command execution: a line becomes builtins run here, or a job PID 1 runs.

import (
	"goos/internal/validation"
	"goos/kernel/kcall"
	execproto "goos/userspace/exec"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	shellcmd "goos/userspace/shell"
)

func execute(line []byte) {
	if command, background := shellcmd.SplitBackground(line); background {
		executeBackground(command)
		return
	}
	if command, timed := shellcmd.SplitTime(line); timed {
		executeTimed(command)
		return
	}
	commands, count, valid := shellcmd.ParseSequence(line, &argumentStorage)
	if !valid {
		write("exec: invalid command sequence\n")
		return
	}
	for index := 0; index < count; index++ {
		executeCommand(commands[index])
		// Sequence slices borrow the static line buffer, not the arena.
		arena.Reset(arenaBase)
	}
}

// executeBackground starts one pipeline without waiting for it. Builtins and
// command sequences run in the shell itself and cannot be backgrounded.
func executeBackground(line []byte) {
	commands, count, valid := shellcmd.ParseSequence(line, &argumentStorage)
	if !valid || count != 1 || len(commands[0]) == 0 {
		write("exec: & applies to one command or pipeline\n")
		return
	}
	redirects, _ := shellcmd.SplitRedirections(line)
	stages, _, _ := shellcmd.SplitPipeline(redirects.Command)
	invocation, parsed := shellcmd.ParseInvocation(stages[0], &argumentStorage)
	if !parsed || invocation.Count == 0 || shellcmd.IsBuiltin(invocation.Arguments[0]) {
		write("exec: builtins cannot run in the background\n")
		return
	}
	executeExternal(line, true)
}

// executeTimed runs the rest of a command line through the normal path and
// reports the elapsed wall-clock time, including every launch it caused.
func executeTimed(line []byte) {
	if len(line) == 0 {
		write("time: expected a command\n")
		return
	}
	start, status := sys.ClockReadNanos()
	if status != kcall.StatusOK {
		write("time: clock unavailable\n")
		return
	}
	execute(line)
	end, status := sys.ClockReadNanos()
	if status != kcall.StatusOK {
		write("time: clock unavailable\n")
		return
	}
	write("time: ")
	writeUint((end - start) / 1000)
	write(" us\n")
}

func executeCommand(line []byte) {
	redirects, _ := shellcmd.SplitRedirections(line)
	stages, count, _ := shellcmd.SplitPipeline(redirects.Command)
	invocation, _ := shellcmd.ParseInvocation(stages[0], &argumentStorage)
	if validation.Enabled && count == 1 && invocation.Count == 2 && argumentIs(invocation.Arguments[0], "fault") {
		injectFault(invocation.Arguments[1])
		return
	}
	if count != 1 || !shellcmd.IsBuiltin(invocation.Arguments[0]) {
		executeExternal(line, false)
		return
	}
	// A builtin with the wrong arguments is answered with its usage here. It
	// used to fall through to the launch path and fail as a missing executable.
	info, _ := shellcmd.LookupCommand(invocation.Arguments[0])
	arguments := int(invocation.Count) - 1
	if arguments == 1 && shellcmd.IsHelpFlag(invocation.Arguments[1]) {
		writeCommandHelp(info)
		return
	}
	if !info.Accepts(arguments) {
		write("usage: ")
		write(info.Usage)
		write("\n")
		return
	}
	switch info.Name {
	case "help":
		printHelp(invocation)
	case "log":
		printLog()
	case "config":
		runConfig(invocation)
	case "services":
		printServices()
	case "jobs":
		printJobs()
	case "kill":
		killJob(invocation.Arguments[1])
	case "reboot":
		shutDown(execproto.ControlReboot, "reboot")
	case "poweroff":
		shutDown(execproto.ControlPowerOff, "poweroff")
	case "handles":
		printHandles(false)
	case "caps":
		printHandles(true)
	case "diag":
		// The dump goes to the console, which only PID 1 and the terminal
		// service can write, so PID 1 asks the kernel for it.
		reply, ok := controlExchange(execproto.ControlDiagnostics, 0)
		if status, valid := execproto.DecodeControlStatus(reply); !ok || !valid || status != execproto.ControlOK {
			write("diag: unavailable in this build\n")
		}
	case "time":
		command, _ := shellcmd.SplitTime(line)
		executeTimed(command)
	case "clear":
		write("\x1b[2J\x1b[H")
		mark("SHELL_CLEAR_OK\n")
	case "cd":
		if invocation.Count == 2 {
			executeCd(invocation.Arguments[1])
		} else {
			executeCd(nil)
		}
	}
}
