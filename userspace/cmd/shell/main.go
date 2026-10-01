//go:build goos_userspace

package main

import (
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/config"
	execproto "goos/userspace/exec"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	shellcmd "goos/userspace/shell"
	"goos/userspace/startup"
	terminal "goos/userspace/tty"
)

var (
	// outputHandle is the shell's copy of the terminal's writing end: what the
	// shell prints goes through the terminal service, like every command's
	// output (ADR 0089). The shell holds no console capability; what is typed
	// arrives as lines over ttyHandle from the same service.
	outputHandle handle.ID
	ttyHandle    handle.ID
	logHandle    handle.ID
	vfsHandle    handle.ID
	execHandle   handle.ID
	configHandle handle.ID
	configWire   [ipc.WireMaxSize]byte
	// prompt is read from the configuration service rather than compiled in.
	// It falls back to the built-in text when there is no session, because the
	// shell has to come up whether or not configuration is available.
	promptText = [config.MaxText]byte{'g', 'o', 'o', 's', '>', ' '}
	promptSize = 6
	ttyWire    [ipc.WireMaxSize]byte
	// lineStorage holds the command line being executed; typedStorage the
	// last line received, which may be input for a running command. They are
	// separate so input typed during `a; b` cannot overwrite `b`.
	lineStorage  [terminal.MaxLine]byte
	typedStorage [terminal.MaxLine]byte
	// typedSize is how much of a line delivered in parts has arrived.
	typedSize int
	// reading is the correlation of the outstanding read from the terminal
	// service, or zero. One is kept outstanding while a foreground command
	// runs too, so what is typed reaches the command and Ctrl-C reaches the
	// shell.
	reading uint64
	// stashed holds a foreground job's reply that arrived while the shell was
	// waiting for another answer from PID 1, such as the one to a kill.
	stashed           ipc.Message
	stashedValid      bool
	arenaBase         arena.Mark
	wire              [ipc.WireMaxSize]byte
	correlation       uint64
	startupData       startup.Data
	handleRecord      [kcall.HandleInfoSize]byte
	cwd               = [shellcmd.MaxPath]byte{'/'}
	cwdSize           = 1
	pathBuffer        [shellcmd.MaxPath]byte
	executableBuffers [execproto.MaxStages][execproto.MaxPath]byte
	argumentStorage   shellcmd.Storage
	jobs              [execproto.Slots]job
	// execPage is the area shared with PID 1 that carries exec requests, one
	// slot per job in flight (ADR 0064, ADR 0109).
	execPage []byte
)

// job is an exec request in flight; its slot of the shared page is reused
// only after PID 1 replies.
type job struct {
	active      bool
	background  bool
	correlation uint64
	command     [terminal.MaxLine]byte
	commandSize int
	// requestSize is the length of the request left in the job's exec page
	// slot; a launch failure is described from it.
	requestSize int
}

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("SHELL_OUT_OF_MEMORY\n") })
	var validStartup, found bool
	startupData, validStartup = startup.Read()
	if !validStartup {
		fatal("SHELL_STARTUP_INVALID\n")
	}
	outputHandle, found = startupData.Handle("stdout")
	if !found {
		fatal("SHELL_OUTPUT_MISSING\n")
	}
	ttyHandle, found = startupData.Handle("tty")
	if !found {
		fatal("SHELL_TERMINAL_MISSING\n")
	}
	logHandle, found = startupData.Handle("log")
	if !found {
		fatal("SHELL_LOG_MISSING\n")
	}
	vfsHandle, found = startupData.Handle("vfs")
	if !found {
		fatal("SHELL_VFS_MISSING\n")
	}
	execHandle, found = startupData.Handle("exec")
	if !found {
		fatal("SHELL_EXEC_MISSING\n")
	}
	// The configuration session is optional: the service warns and continues if
	// it cannot start, and the shell must still come up without it.
	configHandle, _ = startupData.Handle("config")
	if path, found := startupData.Env("PATH"); found {
		setSearchPath(path)
	}
	pageHandle, found := startupData.Handle("execpage")
	if !found {
		fatal("SHELL_EXEC_PAGE_FAILED\n")
	}
	page, pageStatus := sys.MemoryMap(pageHandle, execproto.AreaAddress, execproto.AreaSize, true)
	if pageStatus != kcall.StatusOK {
		fatal("SHELL_EXEC_PAGE_FAILED\n")
	}
	execPage = page
	arenaBase = arena.Current()
	root, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: cwd[:cwdSize]})
	if !ok || root.Status != vfs.StatusOK || root.Kind != ramfs.KindDirectory {
		fatal("SHELL_VFS_CONNECT_FAILED\n")
	}
	arena.Reset(arenaBase)
	mark("SHELL_PROCESS_OK\nSHELL_VFS_CONNECTED_OK\n")
	if validation.Enabled {
		checkOutputIsWriteOnly()
		checkNoLoaderAuthority()
	}
	loadPrompt()
	requestGeometry()
	write("\n")
	for {
		showPrompt()
		for {
			// A finished background job is reported through the terminal
			// service, which takes the line being typed off the screen and
			// puts it back, so the shell does not redraw it. A new terminal
			// session needs the prompt asked for again.
			if pollReplies() {
				break
			}
			result, text, received := takeLine()
			if !received {
				awaitInput()
				continue
			}
			if result == terminal.ResultLine {
				mark("SHELL_TERMINAL_LINE_OK\n")
				size := copy(lineStorage[:], text)
				execute(lineStorage[:size])
				arena.Reset(arenaBase)
			}
			// Ctrl-C at the prompt was echoed by the terminal service and
			// discards the line; end of input there is ignored.
			break
		}
	}
}

// The command index describes the shell's commands. It is protected from
// change, so it is read once and kept (ADR 0080, ADR 0175).
var (
	commandIndex       [commands.MaxIndex]byte
	commandIndexSize   int
	commandIndexLoaded bool
)

// inputDeadline bounds one wait for room in a command's input stream.
const inputDeadline = 5 * clock.TicksPerSecond

var (
	logLine        [kcall.LogText]byte
	handleTypes    = [...]string{"invalid", "process", "task", "memory", "channel", "device", "irq", "stream"}
	rightNames     = [...]string{"read", "write", "execute", "transfer", "wait"}
	serviceStates  = [...]string{"registered", "blocked", "starting", "running", "stopped", "failed"}
	controlStatus  = [...]string{"", "storage sync failed", "refused by the machine", "request rejected", "not found", "not supported on this machine"}
	serviceEntries [execproto.MaxServiceEntries]execproto.ServiceEntry
)

func mark(message string) {
	if validation.Enabled {
		write(message)
	}
}

// Output is assembled into lines before it is sent. The terminal service
// relays each write as it arrives, so a line written in pieces could have a
// command's output land between them; a whole line of at most 256 bytes is
// one write, which the stream never splits.
var (
	outputLine [256]byte
	outputSize int
)

// outputDeadline bounds one wait for room in the terminal's output stream.
const outputDeadline = 2 * clock.TicksPerSecond

// fatal reports and exits, so the supervisor starts a replacement rather than
// leaving a shell that neither works nor ends.
func fatal(message string) {
	if outputHandle != 0 {
		write(message)
		flushOutput()
	}
	sys.Exit(1)
	for {
	}
}

func main() {}
