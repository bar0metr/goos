//go:build goos_userspace

// Command df reports capacity for every mounted filesystem.
package main

import (
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/fsdisplay"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "df",
	Summary: "filesystem capacity",
	Grants:  commands.GrantVFS,
}

var path [vfs.MaxPath]byte
var sizeText [32]byte

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(startup.Data) {
	utility.PrintText("Filesystem  Mount       Size      Used      Free      Flags")
	utility.EndLine()
	var cursor uint32
	for {
		mount, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationReadMount, Offset: uint64(cursor)})
		if received && mount.Status == vfs.StatusNotFound {
			break
		}
		utility.Check(mount, received, nil, false)
		pathSize := copy(path[:], mount.Name)
		info, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationFilesystemInfo, MountID: mount.MountID})
		utility.Check(info, received, path[:pathSize], false)
		printField([]byte(fsdisplay.TypeName(info.Filesystem.Type)), 12)
		printField(path[:pathSize], 12)
		printSize(info.Filesystem.TotalBytes, info.Filesystem.CapacityAvailable, 10)
		used, available := fsdisplay.Used(info.Filesystem)
		printSize(used, available, 10)
		printSize(info.Filesystem.FreeBytes, info.Filesystem.CapacityAvailable, 10)
		utility.PrintText(fsdisplay.Flag(info.Filesystem.ReadOnly))
		utility.EndLine()
		cursor = mount.Cursor
	}
	utility.Mark("DF_PROCESS_OK\n")
}

func printField(value []byte, width int) {
	utility.PrintName(value)
	utility.Pad(len(value), width)
}

func printSize(value uint64, available bool, width int) {
	text := fsdisplay.AppendSize(sizeText[:0], value, available)
	printField(text, width)
}
