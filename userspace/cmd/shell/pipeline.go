//go:build goos_userspace

package main

// Pipelines and redirection: the exec request for a job, the streams its
// stages share, and the files redirection reads or writes (ADR 0041-0047,
// 0064).

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	"goos/userspace/runtime/sys"
	shellcmd "goos/userspace/shell"
	terminal "goos/userspace/tty"
	userwait "goos/userspace/wait"
)

// execSlot is slot of the shared exec area.
func execSlot(slot int) []byte {
	start := slot * execproto.SlotSize
	return execPage[start : start+execproto.SlotSize]
}

// pathVariable is the environment every command receives.
var pathVariable = []byte("PATH=" + execproto.SearchRoot[:len(execproto.SearchRoot)-1])

// readWholeFile reads path into target and refuses a file that does not fit,
// rather than using a prefix of it.
func readWholeFile(path []byte, target []byte) (int, bool) {
	opened, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if !ok || opened.Status != vfs.StatusOK {
		return 0, false
	}
	size, complete := 0, false
	for {
		want := uint32(vfs.MaxReplyData)
		if remaining := len(target) - size; remaining < int(want) {
			want = uint32(remaining) + 1
		}
		reply, ok := callVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: want})
		if !ok || reply.Status != vfs.StatusOK || size+len(reply.Data) > len(target) {
			break
		}
		if len(reply.Data) == 0 {
			complete = true
			break
		}
		size += copy(target[size:], reply.Data)
	}
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	return size, complete
}

// commandPath turns a command word into the path PID 1 launches. A bare name
// is looked up in /system/bin; a word with a slash is a program path, absolute
// or relative to the working directory, which PID 1 reads from the filesystem
// when it lies outside /system (ADR 0097).
func commandPath(word []byte, target *[execproto.MaxPath]byte) ([]byte, bool) {
	for _, value := range word {
		if value == '/' {
			path, ok := shellcmd.ResolvePath(cwd[:cwdSize], word, target)
			return path, ok && len(path) > 1
		}
	}
	return execproto.ResolveExecutable(word, target)
}

// writeSink adds the isolated `write` stage that drains its input stream into
// one VFS path. It reports its own diagnostics and returns false on failure.
func writeSink(builder *execproto.Builder, encodedTarget []byte, appendMode bool, index int) bool {
	target, parsed := shellcmd.ParseInvocation(encodedTarget, &argumentStorage)
	if !parsed || target.Count != 1 {
		write("exec: invalid redirection\n")
		return false
	}
	writePath, ok := execproto.ResolveExecutable([]byte("write"), &executableBuffers[index])
	if !ok {
		write("exec: invalid redirection\n")
		return false
	}
	arguments := [][]byte{[]byte("write"), target.Arguments[0]}
	if appendMode {
		arguments = [][]byte{[]byte("write"), []byte("--append"), target.Arguments[0]}
	}
	builder.Stage(writePath, arguments)
	return true
}

// awaitInput blocks until the terminal service or PID 1 has something for the
// shell. Both are waitable objects, so the shell never polls either.
func awaitInput() {
	flushOutput()
	var interests [userwait.MaxObjects]userwait.Interest
	interests[0] = userwait.Interest{Handle: execHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
	count := 1
	if ttyHandle != 0 {
		interests[1] = userwait.Interest{Handle: ttyHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		count++
	}
	if _, status := userwait.Any(&interests, count, kcall.WaitAnyInfiniteTimeout); status != kcall.StatusOK {
		// A wait that cannot be established must not become a spin. Sleeping a
		// tick degrades to slow polling, which is visible in the CPU column
		// rather than hidden in a loop.
		userwait.Sleep(1)
	}
}

// feedInput writes one typed line to a foreground command's standard input.
// It reports false once the command has stopped reading.
func feedInput(input handle.ID, text []byte) bool {
	var line [terminal.MaxLine + 1]byte
	size := copy(line[:], text)
	line[size] = '\n'
	// A full stream is a command not reading yet; wait for room, but not for
	// ever, so a stuck command cannot hold the shell.
	return writeStream(input, line[:size+1], inputDeadline)
}

// writeStream writes all of data to stream, waiting at most deadline ticks
// each time the stream is full. It reports false if the reader has gone, the
// wait ran out, or the stream answered with something other than progress.
func writeStream(stream handle.ID, data []byte, deadline uint64) bool {
	for len(data) != 0 {
		written, status := sys.StreamWrite(stream, data)
		if status == kcall.StatusResourceExhausted {
			if sys.StreamWait(stream, deadline) != kcall.StatusOK {
				return false
			}
			continue
		}
		if status != kcall.StatusOK || written == 0 || written > len(data) {
			return false
		}
		data = data[written:]
	}
	return true
}

func executeExternal(line []byte, background bool) {
	rawJob = false
	redirects, validRedirect := shellcmd.SplitRedirections(line)
	if !validRedirect {
		write("exec: invalid redirection\n")
		return
	}
	stages, count, split := shellcmd.SplitPipeline(redirects.Command)
	if !split {
		write("exec: invalid pipeline\n")
		return
	}
	redirectCount := redirects.Stages()
	if count+redirectCount > execproto.MaxStages {
		write("exec: invalid redirection\n")
		return
	}
	slot, ok := freeSlot(background)
	if !ok {
		write("exec: too many background jobs\n")
		return
	}
	correlation++
	// Each stage is written to the slot as soon as it is parsed, so the
	// tokenizer's storage serves every stage in turn.
	environment := [][]byte{pathVariable}
	if columnsSize != 0 && linesSize != 0 {
		environment = append(environment, columnsVariable[:columnsSize], linesVariable[:linesSize])
	}
	builder := execproto.NewBuilder(execSlot(int(slot)), cwd[:cwdSize], environment, background)
	stageOffset := 0
	if len(redirects.Input) != 0 {
		target, parsed := shellcmd.ParseInvocation(redirects.Input, &argumentStorage)
		readPath, ok := execproto.ResolveExecutable([]byte("cat"), &executableBuffers[0])
		if !parsed || target.Count != 1 || !ok {
			write("exec: invalid redirection\n")
			return
		}
		builder.Stage(readPath, [][]byte{[]byte("cat"), target.Arguments[0]})
		stageOffset = 1
	}
	for index := 0; index < count; index++ {
		requestIndex := index + stageOffset
		invocation, parsed := shellcmd.ParseInvocation(stages[index], &argumentStorage)
		if !parsed {
			write("exec: invalid arguments\n")
			return
		}
		executable, ok := commandPath(invocation.Arguments[0], &executableBuffers[requestIndex])
		if !ok {
			writeBytes(invocation.Arguments[0])
			write(": not a command name or a program path\n")
			return
		}
		builder.Stage(executable, invocation.Arguments)
		if count == 1 && !background && stageOffset == 0 && redirectCount == 0 {
			rawJob = wantsRawTerminal(invocation.Arguments[0])
		}
	}
	nextIndex := stageOffset + count
	errorSource := uint8(0)
	if len(redirects.Output) != 0 {
		if !writeSink(&builder, redirects.Output, redirects.Append, nextIndex) {
			return
		}
		nextIndex++
	}
	if len(redirects.Error) != 0 {
		// The sink reads the stderr of the last user stage, not stdout.
		if !writeSink(&builder, redirects.Error, redirects.ErrorAppend, nextIndex) {
			return
		}
		errorSource = uint8(stageOffset + count)
	}
	data, ok := builder.Finish(errorSource)
	if !ok {
		write("exec: command too large\n")
		return
	}
	request, ok := execproto.EncodeSubmit(slot, len(data), correlation)
	if !ok {
		write("exec: invalid request\n")
		return
	}
	if !sendExec(request) {
		return
	}
	jobs[slot] = job{active: true, background: background, correlation: correlation, requestSize: len(data)}
	jobs[slot].commandSize = copy(jobs[slot].command[:], line)
	if background {
		write("[")
		writeUint(uint64(slot) + 1)
		write("] started\n")
		return
	}
	message, received := awaitReply(correlation, int(slot))
	endRawJob()
	jobs[slot].active = false
	reply, validReply := execproto.DecodeReply(message)
	if !received {
		return
	}
	if !validReply {
		write("exec: corrupt reply\n")
		return
	}
	if reply.LaunchStatus != 0 {
		writeLaunchFailure(int(slot), reply)
		return
	}
	if reply.ExitCode == kcall.ExitKilled {
		mark("SHELL_KILL_OK\n")
	} else if reply.ExitCode != 0 {
		write("exit status: ")
		writeUint(reply.ExitCode)
		write("\n")
		mark("EXIT_STATUS_PROPAGATION_OK\n")
	}
	mark("SHELL_EXEC_OK\n")
	if count+redirectCount > 1 {
		mark("PIPELINE_EXEC_OK\n")
	}
	if redirectCount != 0 {
		mark("REDIRECTION_EXEC_OK\n")
	}
}

// writeLaunchFailure names the stage PID 1 could not start. The request is
// still in the job's exec page slot, so the name is read back from it rather
// than kept for every job.
func writeLaunchFailure(slot int, reply execproto.Reply) {
	status := supervisor.LaunchStatus(reply.LaunchStatus)
	var name []byte
	if request, ok := execproto.DecodeRequest(execSlot(slot)[:jobs[slot].requestSize]); ok && reply.Stage < request.Count {
		// A command is named as it was typed; a program path in full.
		path := request.Stages[reply.Stage].Path
		if command, searched := execproto.CommandFromSystemPath(path); searched {
			name = command
		} else {
			name = path
		}
	}
	if len(name) != 0 && status.NamesExecutable() {
		writeBytes(name)
		write(": ")
	} else if len(name) != 0 {
		write("exec: cannot start ")
		writeBytes(name)
		write(": ")
	} else {
		write("exec: cannot start: ")
	}
	write(status.Reason())
	write("\n")
}

func sendExec(request ipc.Message) bool {
	size, ok := ipc.Encode(request, wire[:])
	if !ok {
		write("exec: encode failed\n")
		return false
	}
	if status := sys.IPCSend(execHandle, wire[:size]); status != 0 {
		write("exec: service unavailable\n")
		return false
	}
	return true
}

// controlExchange sends one control request to PID 1 and returns its reply.
func controlExchange(operation execproto.ControlOperation, argument uint8) (ipc.Message, bool) {
	correlation++
	request, ok := execproto.EncodeControl(operation, argument, correlation)
	if !ok {
		return ipc.Message{}, false
	}
	if !sendExec(request) {
		return ipc.Message{}, false
	}
	return awaitReply(correlation, -1)
}
