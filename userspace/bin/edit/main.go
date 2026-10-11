//go:build goos_userspace

// Command edit is the native full-screen text editor. It is an ordinary VFS
// and terminal client: it loads the file through the 64 KiB bulk buffer into
// a fixed 1 MiB document, and saves through a temporary file and Replace
// (ADR 0129), so a failed save leaves the file as it was.
package main

import (
	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/editor"
	"goos/userspace/safesave"
	"goos/userspace/shell"
	"goos/userspace/startup"
	"goos/userspace/terminal"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "edit FILE",
	Summary: "edit text, max 1 MiB",
	MinArgs: 1,
	MaxArgs: 1,
	Grants:  commands.GrantVFS | commands.GrantTerminal,
}

// app is package-level: its document storage lives in demand-zero memory, so
// the part a small file never reaches costs no pages.
var (
	app           editor.App
	bulk          utility.Bulk
	parser        terminal.Parser
	outputStorage [4096]byte
	inputStorage  [256]byte
	peerGeometry  bool
)

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	path := utility.Path(data, 1)
	var ok bool
	if bulk, ok = utility.NewBulk(utility.BulkSize); !ok {
		utility.Fail(nil, "no memory for file transfers")
	}
	geometry, geometryValid := utility.TerminalGeometry(data)
	columns, rows, display := geometry.Columns, geometry.Rows, geometryValid && geometry.Display
	peerGeometry = geometryValid && geometry.Authoritative && !geometry.Display
	if !app.Init(files{}, path, data.CWD(), columns, rows, display) {
		utility.Fail(path, "invalid path")
	}
	// A file that cannot be edited is refused on the editor's own screen:
	// the shell clears the screen when a raw-mode job ends, which would
	// erase a message written to standard error just before.
	load(path)
	if peerGeometry {
		app.Handle(terminal.Event{Key: terminal.KeyPosition, Column: columns, Row: rows})
	}
	output := terminal.NewOutput(outputStorage[:], utility.Write)
	// Everything the editor keeps is static, so what one key allocated is
	// discarded before the next.
	mark := utility.HeapMark()
	for !app.Quit() {
		app.Render(&output)
		utility.HeapReset(mark)
		if app.SizeWanted() && !peerGeometry {
			requestSize(&output)
		}
		if !readKeys(&output) {
			break
		}
	}
	output.Reset()
	output.Flush()
	if app.Refused() {
		utility.Exit(1)
	}
	if app.Saves != 0 {
		utility.Mark("EDIT_SAVE_OK\n")
	}
	utility.Mark("EDIT_EXIT_OK\n")
}

// load opens an existing regular file into the document, or starts an empty
// one for a name that does not exist yet.
func load(path []byte) {
	kind, size, status := utility.Stat(path)
	switch {
	case status == vfs.StatusNotFound:
		app.NewFile()
		return
	case status != vfs.StatusOK:
		app.Refuse(shell.VFSReason(status, false))
		return
	case kind == ramfs.KindDirectory:
		app.Refuse("is a directory")
		return
	case kind != ramfs.KindFile:
		app.Refuse("not a regular file")
		return
	}
	opened, status := utility.Request(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if status != vfs.StatusOK {
		app.Refuse(shell.VFSReason(status, false))
		return
	}
	app.Load(fileReader{handle: opened.Handle}, size)
	utility.Request(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
}

// fileReader reads the file in order through the bulk buffer. The editor
// asks for at most editor.LoadChunk bytes, the size of that buffer.
type fileReader struct{ handle vfs.Handle }

func (reader fileReader) Read(target []byte) (int, vfs.Status) {
	if len(target) > len(bulk.Memory) {
		return 0, vfs.StatusInvalidArgument
	}
	mark := utility.HeapMark()
	reply, received := bulk.ReadFile(reader.handle, len(target))
	utility.HeapReset(mark)
	if !received {
		return 0, vfs.StatusPeerDied
	}
	if reply.Status != vfs.StatusOK {
		return 0, reply.Status
	}
	if reply.Size > uint64(len(target)) {
		return 0, vfs.StatusBackendError
	}
	return copy(target, bulk.Memory[:reply.Size]), vfs.StatusOK
}

// files is the editor's filesystem after loading: the command's VFS session.
type files struct{}

func (files) Stat(path []byte) (ramfs.Kind, uint64, vfs.Status) { return utility.Stat(path) }

func (files) Save(path []byte, source safesave.Source) (vfs.Status, safesave.Stage) {
	return utility.SaveFile(path, &bulk, source)
}

// idleTicks is how long the editor waits for a key before asking a COM1
// terminal for its size again.
const idleTicks = clock.TicksPerSecond

// readKeys waits for input and applies every key it holds. It returns false
// at the end of input, which means the shell has gone.
func readKeys(output *terminal.Output) bool {
	for {
		deadline := uint64(idleTicks)
		if peerGeometry {
			deadline = kcall.WaitAnyInfiniteTimeout
		}
		status, count, geometry, resized := utility.ReadTerminalWithin(inputStorage[:], deadline)
		if resized {
			app.Handle(terminal.Event{Key: terminal.KeyPosition, Column: geometry.Columns, Row: geometry.Rows})
			if count == 0 {
				return true
			}
		}
		if count == 0 {
			if status == uint64(kcall.StatusResourceExhausted) {
				if app.FollowsTerminal() && !peerGeometry {
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
			if event, ok := parser.Feed(value); ok && (!peerGeometry || event.Key != terminal.KeyPosition) {
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
// a key event.
func requestSize(output *terminal.Output) {
	output.RequestSize()
	output.Flush()
}
