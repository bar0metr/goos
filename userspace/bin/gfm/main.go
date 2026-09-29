//go:build goos_userspace

// Command gfm is the two-pane file manager (ADR 0124). It is an ordinary
// command: the files it shows come through its VFS session, the keys it reads
// arrive on standard input, and the screen it draws is ANSI output on standard
// output. Its manifest asks the shell for raw terminal mode, which is all that
// makes it full-screen; it holds no console, framebuffer or keyboard authority.
package main

import (
	"goos/internal/timekeeping"
	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/filemanager"
	supervisor "goos/userspace/init"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/terminal"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "gfm",
	Summary: "two-pane file manager",
	Grants:  commands.GrantVFS | commands.GrantTerminal | commands.GrantLaunch,
}

var (
	app           filemanager.App
	bulk          utility.Bulk
	parser        terminal.Parser
	outputStorage [4096]byte
	inputStorage  [256]byte
)

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	var ok bool
	if bulk, ok = utility.NewBulk(utility.BulkSize); !ok {
		utility.Fail(nil, "no memory for copying")
	}
	columns, rows, display := utility.TerminalSize(data)
	output := terminal.NewOutput(outputStorage[:], utility.Write)
	app.Init(files{}, data.CWD(), columns, rows, display)
	app.SetPrograms(programs{data: data})
	if now, status := sys.ClockNow(timekeeping.Realtime); status == kcall.StatusOK {
		year, _, _, _, _, _ := timekeeping.CivilUTC(now.Seconds)
		app.SetYear(year)
	}
	// Everything the manager keeps is static, so what one key allocated is
	// discarded before the next: it can run for as long as it is used.
	mark := utility.HeapMark()
	for !app.Quit() {
		app.Render(&output)
		utility.HeapReset(mark)
		if app.SizeWanted() {
			requestSize(&output)
		}
		if !readKeys(&output) {
			break
		}
	}
	output.Reset()
	output.Flush()
	report()
}

// idleTicks is how long gfm waits for a key before asking the terminal for
// its size again. A terminal on COM1 is resized without telling anyone, so
// this is how the manager follows it; the framebuffer console's size is
// fixed and never asked.
const idleTicks = clock.TicksPerSecond

// readKeys waits for input and applies every key it holds, asking the
// terminal for its size while nothing is typed. It returns false at the end
// of input, which means the shell has gone.
func readKeys(output *terminal.Output) bool {
	for {
		status, count := utility.ReadInputWithin(inputStorage[:], idleTicks)
		if count == 0 {
			if status == uint64(kcall.StatusResourceExhausted) {
				if app.FollowsTerminal() {
					requestSize(output)
				}
				continue
			}
			if utility.Retry(status) {
				continue
			}
			return false
		}
		for _, value := range inputStorage[:count] {
			if event, ok := parser.Feed(value); ok {
				app.Handle(event)
			}
		}
		// Each key's sequence arrives whole, so an ESC that ends the read
		// is the Escape key.
		if event, ok := parser.Flush(); ok {
			app.Handle(event)
		}
		return true
	}
}

// requestSize asks the terminal on COM1 for its size; its answer arrives as
// a key event. The query leaves the cursor in the corner, and every frame
// starts with a MoveTo.
func requestSize(output *terminal.Output) {
	output.RequestSize()
	output.Flush()
}

// report writes the validation build's markers once the screen is reset.
func report() {
	if app.Keys != 0 {
		utility.Mark("GFM_RAW_INPUT_OK\n")
	}
	if app.FunctionKeys != 0 {
		utility.Mark("GFM_FUNCTION_KEYS_OK\n")
	}
	if app.Directories != 0 {
		utility.Mark("GFM_MKDIR_OK\n")
	}
	if app.Copies != 0 {
		utility.Mark("GFM_COPY_OK\n")
	}
	if app.Launches != 0 {
		utility.Mark("GFM_LAUNCH_OK\n")
	}
	utility.Mark("GFM_EXIT_OK\n")
}

// files is the manager's filesystem: the command's VFS session.
type files struct{}

func (files) ReadDir(path []byte, visitor filemanager.Visitor) vfs.Status {
	return utility.ReadDir(path, visitor)
}

func (files) Stat(path []byte) (ramfs.Kind, uint64, vfs.Status) { return utility.Stat(path) }
func (files) Mkdir(path []byte) vfs.Status                      { return utility.Mkdir(path) }
func (files) Rename(source, target []byte) vfs.Status           { return utility.Rename(source, target) }
func (files) Unlink(path []byte) vfs.Status                     { return utility.Unlink(path) }

func (files) CopyFile(source, target []byte, overwrite bool) (vfs.Status, bool) {
	status, failure := utility.CopyFile(source, target, &bulk, overwrite)
	return status, failure == utility.CopyTarget
}

func (files) FilesystemInfo(path []byte) (ramfs.FilesystemInfo, vfs.MountID, vfs.Status) {
	return utility.FilesystemInfo(path)
}

// programs starts the viewer and the editor through PID 1 (ADR 0130). They
// read gfm's input and draw on its terminal while gfm waits, and get the
// framebuffer's size the way gfm got it.
type programs struct{ data startup.Data }

var (
	programArgs [2][]byte
	programEnv  [2][]byte
	columnsVar  [16 + 8]byte
	linesVar    [16 + 8]byte
)

func (runner programs) Run(command, path, dir []byte) (supervisor.LaunchStatus, uint64) {
	programArgs[0], programArgs[1] = command, path
	environment := programEnv[:0]
	if value, found := runner.data.Env("COLUMNS"); found && len(value) <= 16 {
		environment = append(environment, append(append(columnsVar[:0], "COLUMNS="...), value...))
	}
	if value, found := runner.data.Env("LINES"); found && len(value) <= 16 {
		environment = append(environment, append(append(linesVar[:0], "LINES="...), value...))
	}
	return utility.Launch(command, programArgs[:], dir, environment)
}
