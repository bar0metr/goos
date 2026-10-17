//go:build goos_userspace

// Command cp copies a file.
package main

import (
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "cp SOURCE TARGET",
	Summary: "copy a file",
	MinArgs: 2,
	MaxArgs: 2,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	source := utility.Path(data, 1)
	target := utility.Target(data, 2)
	// Open the source first so a missing one leaves the target untouched.
	input, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: source})
	utility.Check(input, received, source, false)
	created, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationCreate, Path: target})
	if created.Status == vfs.StatusAlreadyExists {
		created.Status = vfs.StatusOK
	}
	utility.Check(created, received, target, true)
	truncated, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationTruncate, Path: target, Offset: 0})
	utility.Check(truncated, received, target, true)
	output, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: target})
	utility.Check(output, received, target, true)
	// The data moves through a shared buffer, 64 KiB per exchange, rather
	// than inline in the messages (ADR 0107).
	buffer, ok := utility.NewBulk(utility.BulkSize)
	if !ok {
		utility.Fail(nil, "no memory for the copy")
	}
	for {
		chunk, ok := buffer.ReadFile(input.Handle, utility.BulkSize)
		utility.Check(chunk, ok, source, false)
		if chunk.Size == 0 {
			break
		}
		written, ok := buffer.WriteFile(output.Handle, int(chunk.Size))
		utility.CheckWritten(written, ok, target, int(chunk.Size))
	}
	utility.CallVFS(vfs.Request{Operation: vfs.OperationClose, Handle: input.Handle})
	utility.CallVFS(vfs.Request{Operation: vfs.OperationClose, Handle: output.Handle})
	utility.Mark("CP_PROCESS_OK\n")
}
