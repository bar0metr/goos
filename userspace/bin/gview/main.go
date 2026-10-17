//go:build goos_userspace

// Command gview displays a file through a bounded read window. It is an
// ordinary VFS and terminal client and owns no device or framebuffer handle.
package main

import (
	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/terminal"
	"goos/userspace/utility"
	"goos/userspace/viewer"
)

var manifest = commands.Manifest{
	Usage:   "gview FILE",
	Summary: "bounded text viewer",
	MinArgs: 1,
	MaxArgs: 1,
	Grants:  commands.GrantVFS | commands.GrantTerminal,
}

var (
	app           viewer.App
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
	stat, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	utility.Check(stat, received, path, false)
	if stat.Kind != ramfs.KindFile {
		utility.Fail(path, "not a regular file")
	}
	opened, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	utility.Check(opened, received, path, false)
	var ok bool
	if bulk, ok = utility.NewBulk(utility.BulkSize); !ok {
		utility.Fail(nil, "no memory for file window")
	}

	geometry, geometryValid := utility.TerminalGeometry(data)
	columns, rows, display := geometry.Columns, geometry.Rows, geometryValid && geometry.Display
	peerGeometry = geometryValid && geometry.Authoritative && !geometry.Display
	reader := fileReader{handle: opened.Handle, bulk: &bulk}
	if status := app.Init(reader, path, stat.Size, columns, rows, display); status != vfs.StatusOK {
		utility.Request(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
		utility.Fail(path, "cannot read file")
	}
	if peerGeometry {
		app.Handle(terminal.Event{Key: terminal.KeyPosition, Column: columns, Row: rows})
	}
	output := terminal.NewOutput(outputStorage[:], utility.Write)
	for !app.Quit() {
		app.Render(&output)
		if app.SizeWanted() && !peerGeometry {
			requestSize(&output)
		}
		if !readKeys(&output) {
			break
		}
	}
	utility.Request(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	output.Reset()
	output.Flush()
	utility.Mark("GVIEW_EXIT_OK\n")
}

type fileReader struct {
	handle vfs.Handle
	bulk   *utility.Bulk
}

func (reader fileReader) ReadAt(offset uint64, target []byte) (int, vfs.Status) {
	if reader.bulk == nil || len(target) > len(reader.bulk.Memory) {
		return 0, vfs.StatusInvalidArgument
	}
	if _, status := utility.Request(vfs.Request{Operation: vfs.OperationSeek, Handle: reader.handle, Offset: offset}); status != vfs.StatusOK {
		return 0, status
	}
	reply, received := reader.bulk.ReadFile(reader.handle, len(target))
	if !received {
		return 0, vfs.StatusPeerDied
	}
	if reply.Status != vfs.StatusOK {
		return 0, reply.Status
	}
	if reply.Size > uint64(len(target)) || reply.Size > uint64(len(reader.bulk.Memory)) {
		return 0, vfs.StatusBackendError
	}
	count := int(reply.Size)
	copy(target, reader.bulk.Memory[:count])
	return count, vfs.StatusOK
}

const idleTicks = clock.TicksPerSecond

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
		if event, ok := parser.Flush(); ok {
			app.Handle(event)
		}
		return true
	}
}

func requestSize(output *terminal.Output) {
	output.RequestSize()
	output.Flush()
}
