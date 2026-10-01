//go:build goos_userspace

package main

// Builtins: commands that change or report the shell's own state, or that ask
// PID 1 for something that launches nothing (ADR 0079, 0061).

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	execproto "goos/userspace/exec"
	"goos/userspace/runtime/sys"
	shellcmd "goos/userspace/shell"
	"strconv"
)

// injectFault asks PID 1 to kill the terminal service, this shell, or the
// filesystem service. It exists only in the validation build, where the boot
// test uses it to show that each restarts on its own while the rest keeps
// working (ADR 0089, 0096).
func injectFault(target []byte) {
	service := execproto.FaultTerminal
	if argumentIs(target, "shell") {
		service = execproto.FaultShell
	} else if argumentIs(target, "kernel-stack") {
		service = execproto.FaultKernelStack
	} else if argumentIs(target, "fat32") {
		service = execproto.FaultFilesystem
	} else if argumentIs(target, "vfs") {
		service = execproto.FaultVFS
	} else if argumentIs(target, "config") {
		service = execproto.FaultConfig
	} else if argumentIs(target, "log") {
		service = execproto.FaultLog
	} else if argumentIs(target, "block") {
		service = execproto.FaultBlock
	} else if argumentIs(target, "driver") {
		service = execproto.FaultDriver
	} else if argumentIs(target, "storage") {
		service = execproto.FaultStorage
	} else if !argumentIs(target, "tty") {
		write("fault: tty, shell, fat32, vfs, config, log, block, driver, storage or kernel-stack\n")
		return
	}
	reply, ok := controlExchange(execproto.ControlFault, service)
	if status, valid := execproto.DecodeControlStatus(reply); !ok || !valid || status != execproto.ControlOK {
		write("fault: refused\n")
	}
}

// printHelp lists every documented command, or describes the one named.
func printHelp(invocation shellcmd.Invocation) {
	if invocation.Count == 2 {
		name := invocation.Arguments[1]
		if info, found := shellcmd.LookupCommand(name); found {
			writeUsage(info.Usage, info.Summary)
			mark("SHELL_HELP_COMMAND_OK\n")
			return
		}
		if index, ok := systemCommands(); ok {
			if entry, found := commands.Lookup(index, name); found {
				writeUsageBytes(entry.Usage, entry.Summary)
				mark("SHELL_HELP_COMMAND_OK\n")
				return
			}
		}
		write("help: no such command: ")
		writeBytes(name)
		write("\n")
		return
	}
	write("Shell builtins:\n")
	for index := range shellcmd.Commands {
		writeCommandLine([]byte(shellcmd.Commands[index].Name), []byte(shellcmd.Commands[index].Summary))
	}
	index, ok := systemCommands()
	write("Commands in ")
	write(commandDirectory)
	write(":\n")
	if !ok {
		write("  (the command index could not be read)\n")
	}
	for offset := 0; ok; {
		entry, next, valid := commands.Next(index, offset)
		if !valid {
			break
		}
		writeCommandLine(entry.Name, entry.Summary)
		offset = next
	}
	write("Run 'help COMMAND' or 'COMMAND --help' for usage.\n")
	mark("SHELL_HELP_OK\n")
}

func writeCommandLine(name, summary []byte) {
	write("  ")
	writePadded(name, 10)
	writeBytes(summary)
	write("\n")
}

func writeCommandHelp(info shellcmd.CommandInfo) { writeUsage(info.Usage, info.Summary) }

func writeUsage(usage, summary string) {
	writeUsageBytes([]byte(usage), []byte(summary))
}

func writeUsageBytes(usage, summary []byte) {
	write("usage: ")
	writeBytes(usage)
	write("\n  ")
	writeBytes(summary)
	write("\n")
}

// systemCommands returns the index of the commands this shell runs: the
// installed one, or in recovery, where nothing is installed, the archive's.
func systemCommands() ([]byte, bool) {
	if !commandIndexLoaded {
		size, ok := readWholeFile([]byte(commands.IndexPath), commandIndex[:])
		if !ok {
			if size, ok = readWholeFile([]byte(commands.BootstrapIndexPath), commandIndex[:]); !ok {
				return nil, false
			}
			commandDirectory = execproto.BootstrapSearchRoot[:len(execproto.BootstrapSearchRoot)-1]
		}
		commandIndexSize, commandIndexLoaded = size, true
	}
	return commandIndex[:commandIndexSize], true
}

// commandDirectory is where the commands of the index read are.
var commandDirectory = execproto.SearchRoot[:len(execproto.SearchRoot)-1]

func printServices() {
	reply, ok := controlExchange(execproto.ControlServices, 0)
	count, valid := execproto.DecodeServices(reply, &serviceEntries)
	if !ok || !valid {
		write("services: no reply\n")
		return
	}
	write("NAME        STATE       PID   RESTARTS\n")
	for _, entry := range serviceEntries[:count] {
		writePadded(entry.Name, 12)
		state := "unknown"
		if int(entry.State) < len(serviceStates) {
			state = serviceStates[entry.State]
		}
		writePadded([]byte(state), 12)
		writePaddedUint(uint64(entry.PID), 6)
		writeUint(uint64(entry.Restarts))
		write("\n")
	}
}

// shutDown asks PID 1 to reboot or power off. Only a failure is answered.
func shutDown(operation execproto.ControlOperation, name string) {
	reply, ok := controlExchange(operation, 0)
	if !ok || reply.PayloadSize != 1 || int(reply.Payload[0]) >= len(controlStatus) || reply.Payload[0] == 0 {
		write(name)
		write(": no reply\n")
		return
	}
	write(name)
	write(": ")
	write(controlStatus[reply.Payload[0]])
	write("\n")
}

// printHandles lists the shell's own handle table. With names it shows each
// startup capability by the name it was granted under.
func printHandles(names bool) {
	if names {
		write("NAME      TYPE      RIGHTS\n")
	} else {
		write("HANDLE              TYPE      RIGHTS\n")
	}
	for slot := uint64(0); slot < handle.MaxHandles; slot++ {
		if sys.HandleInfo(slot, &handleRecord) != kcall.StatusOK {
			continue
		}
		id := binary.LittleEndian.Uint64(handleRecord[0:8])
		typeID := handleRecord[8]
		rights := binary.LittleEndian.Uint32(handleRecord[12:16])
		if names {
			name := capabilityName(handle.ID(id))
			if name == nil {
				continue
			}
			writePadded(name, 10)
		} else {
			writeHex(id)
			write("  ")
		}
		typeName := "unknown"
		if int(typeID) < len(handleTypes) {
			typeName = handleTypes[typeID]
		}
		writePadded([]byte(typeName), 10)
		separator := ""
		for bit, right := range rightNames {
			if rights&(1<<bit) != 0 {
				write(separator)
				write(right)
				separator = ","
			}
		}
		write("\n")
	}
}

func capabilityName(id handle.ID) []byte {
	for index := 0; index < startupData.HandleCount(); index++ {
		if name, bound := startupData.HandleAt(index); bound == id {
			return name
		}
	}
	return nil
}

func writePadded(value []byte, width int) {
	writeBytes(value)
	for column := len(value); column < width; column++ {
		write(" ")
	}
}

func writePaddedUint(value uint64, width int) {
	var digits [20]byte
	text := strconv.AppendUint(digits[:0], value, 10)
	writePadded(text, width)
}

func writeHex(value uint64) {
	var digits [18]byte
	digits[0], digits[1] = '0', 'x'
	for index := 0; index < 16; index++ {
		digits[17-index] = "0123456789abcdef"[value>>(uint(index)*4)&0xf]
	}
	writeBytes(digits[:])
}

// argumentIs compares an argument against a literal without building a string,
// which would allocate on this runtime's bump heap for every comparison.
func argumentIs(argument []byte, expected string) bool {
	if len(argument) != len(expected) {
		return false
	}
	for index := range argument {
		if argument[index] != expected[index] {
			return false
		}
	}
	return true
}

// printLog prints the retained system log, oldest record first.
func printLog() {
	for index := uint64(0); ; index++ {
		if sys.LogRead(logHandle, index, &logLine) != kcall.StatusOK {
			return
		}
		size := 0
		for size < len(logLine) && logLine[size] != 0 {
			size++
		}
		writeBytes(logLine[:size])
	}
}

func resolve(argument []byte, defaultCurrent bool) ([]byte, bool) {
	if len(argument) == 0 {
		if defaultCurrent {
			return cwd[:cwdSize], true
		}
		argument = []byte("/")
	}
	return shellcmd.ResolvePath(cwd[:cwdSize], argument, &pathBuffer)
}

func executeCd(argument []byte) {
	path, ok := resolve(argument, false)
	if !ok {
		writeCdError(argument, "invalid path")
		return
	}
	reply, received := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	if !received {
		writeCdError(path, "filesystem service unavailable")
		return
	}
	if reply.Status != vfs.StatusOK {
		writeCdError(path, shellcmd.VFSReason(reply.Status, false))
		return
	}
	if reply.Kind != ramfs.KindDirectory {
		writeCdError(path, "not a directory")
		return
	}
	copy(cwd[:], path)
	cwdSize = len(path)
	mark("SHELL_CD_OK\n")
}

func writeCdError(path []byte, reason string) {
	write("cd: ")
	writeBytes(path)
	write(": ")
	write(reason)
	write("\n")
}

// parseUint reads a bounded decimal job number.
func parseUint(value []byte) (uint64, bool) {
	if len(value) == 0 || len(value) > 3 {
		return 0, false
	}
	result := uint64(0)
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
		result = result*10 + uint64(character-'0')
	}
	return result, true
}
