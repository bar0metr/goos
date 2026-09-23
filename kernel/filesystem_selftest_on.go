//go:build uefi && goos_validation

package main

import (
	"goos/kernel/arch/amd64"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

// Filesystems do not belong in the kernel: the RAMFS and VFS packages run in
// the userspace filesystem service. The kernel links them only in validation
// builds, to check the libraries in place before any service starts (ADR 0084).

var (
	bootRAMFS     ramfs.FileSystem
	deviceRAMFS   ramfs.FileSystem
	bootVFS       vfs.Namespace
	ramfsReadback [16]byte
	vfsReadback   [16]byte
)

func runFilesystemSelfTests(console amd64.COM1) {
	ramfsRoot := bootRAMFS.Init()
	ramfsDirectory, ramfsDirectoryCreated := bootRAMFS.Mkdir(ramfsRoot, []byte("etc"))
	ramfsFile, ramfsFileCreated := bootRAMFS.Create(ramfsDirectory, []byte("motd"))
	ramfsWritten, ramfsWriteStatus := bootRAMFS.Write(ramfsFile, 0, []byte("GoOS RAMFS"))
	ramfsRead, ramfsReadStatus := bootRAMFS.Read(ramfsFile, 0, ramfsReadback[:])
	ramfsStat, ramfsStatStatus := bootRAMFS.Stat(ramfsFile)
	ramfsEntry, _, ramfsReadDirStatus := bootRAMFS.ReadDir(ramfsDirectory, 0)
	ramfsRenamed := bootRAMFS.Rename(ramfsFile, ramfsRoot, []byte("welcome"))
	ramfsOpened, ramfsOpenStatus := bootRAMFS.Open(ramfsRoot, []byte("welcome"))
	ramfsRequest, ramfsRequestEncoded := ramfs.EncodeRequest(ramfs.Request{Operation: ramfs.OperationOpen, Parent: ramfsRoot, Name: []byte("welcome")}, 1)
	ramfsDecoded, ramfsRequestDecoded := ramfs.DecodeRequest(ramfsRequest)
	if ramfsDirectoryCreated != ramfs.StatusOK || ramfsFileCreated != ramfs.StatusOK || ramfsWriteStatus != ramfs.StatusOK || ramfsWritten != 10 ||
		ramfsReadStatus != ramfs.StatusOK || ramfsRead != 10 || string(ramfsReadback[:ramfsRead]) != "GoOS RAMFS" || ramfsStatStatus != ramfs.StatusOK || ramfsStat.Size != 10 ||
		ramfsReadDirStatus != ramfs.StatusOK || string(ramfsEntry.Name[:ramfsEntry.Size]) != "motd" || ramfsRenamed != ramfs.StatusOK || ramfsOpenStatus != ramfs.StatusOK || ramfsOpened != ramfsFile ||
		!ramfsRequestEncoded || !ramfsRequestDecoded || ramfsDecoded.Operation != ramfs.OperationOpen || ramfsDecoded.Parent != ramfsRoot || string(ramfsDecoded.Name) != "welcome" {
		console.WriteString("PANIC: RAMFS operation failed\n")
		for {
			amd64.Halt()
		}
	}
	if bootRAMFS.Truncate(ramfsFile, 4) != ramfs.StatusOK || bootRAMFS.Unlink(ramfsRoot, []byte("welcome")) != ramfs.StatusOK {
		console.WriteString("PANIC: RAMFS lifecycle failed\n")
		for {
			amd64.Halt()
		}
	}
	if _, status := bootRAMFS.Stat(ramfsFile); status != ramfs.StatusNotFound {
		console.WriteString("PANIC: RAMFS stale node accepted\n")
		for {
			amd64.Halt()
		}
	}
	if _, status := bootRAMFS.Create(ramfsRoot, []byte("bad/name")); status != ramfs.StatusInvalidArgument {
		console.WriteString("PANIC: RAMFS invalid name accepted\n")
		for {
			amd64.Halt()
		}
	}
	mark("RAMFS_PROTOCOL_OK")
	mark("RAMFS_METADATA_OK")
	mark("RAMFS_IO_OK")
	mark("RAMFS_BOUNDS_OK")

	deviceRoot := deviceRAMFS.Init()
	if bootVFS.Mount([]byte("/"), &bootRAMFS, ramfsRoot) != vfs.StatusOK || bootVFS.Create([]byte("/tmp"), true) != vfs.StatusOK ||
		bootVFS.Mount([]byte("/tmp/dev"), &deviceRAMFS, deviceRoot) != vfs.StatusOK || bootVFS.Create([]byte("/tmp/note"), false) != vfs.StatusOK ||
		bootVFS.Create([]byte("/tmp/dev/console"), false) != vfs.StatusOK {
		console.WriteString("PANIC: VFS namespace setup failed\n")
		for {
			amd64.Halt()
		}
	}
	vfsFile, vfsOpened := bootVFS.Open([]byte("//tmp/./note"), vfs.Read|vfs.Write)
	vfsWritten, vfsWriteStatus := bootVFS.Write(vfsFile, []byte("VFS works"))
	vfsSeekStatus := bootVFS.Seek(vfsFile, 0)
	vfsRead, vfsReadStatus := bootVFS.Read(vfsFile, vfsReadback[:])
	vfsStat, vfsStatStatus := bootVFS.Stat([]byte("/tmp/note"))
	_, mountedConsole, mountedResolveStatus := bootVFS.Resolve([]byte("/tmp/dev/console"))
	deviceConsole, deviceConsoleStatus := deviceRAMFS.Open(deviceRoot, []byte("console"))
	vfsRequest, vfsRequestEncoded := vfs.EncodeRequest(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/tmp/note")}, 2)
	vfsDecoded, vfsRequestDecoded := vfs.DecodeRequest(vfsRequest)
	if vfsOpened != vfs.StatusOK || vfsWriteStatus != vfs.StatusOK || vfsWritten != 9 || vfsSeekStatus != vfs.StatusOK ||
		vfsReadStatus != vfs.StatusOK || vfsRead != 9 || string(vfsReadback[:vfsRead]) != "VFS works" || vfsStatStatus != vfs.StatusOK || vfsStat.Size != 9 ||
		mountedResolveStatus != vfs.StatusOK || deviceConsoleStatus != ramfs.StatusOK || mountedConsole != deviceConsole ||
		!vfsRequestEncoded || !vfsRequestDecoded || vfsDecoded.Operation != vfs.OperationOpen || string(vfsDecoded.Path) != "/tmp/note" {
		console.WriteString("PANIC: VFS operation failed\n")
		for {
			amd64.Halt()
		}
	}
	if bootVFS.Close(vfsFile) != vfs.StatusOK || bootVFS.Rename([]byte("/tmp/note"), []byte("/tmp/renamed")) != vfs.StatusOK {
		console.WriteString("PANIC: VFS lifecycle failed\n")
		for {
			amd64.Halt()
		}
	}
	if _, _, status := bootVFS.Resolve([]byte("/tmp/note")); status != vfs.StatusNotFound {
		console.WriteString("PANIC: VFS stale path cache accepted\n")
		for {
			amd64.Halt()
		}
	}
	if bootVFS.Unlink([]byte("/tmp/renamed")) != vfs.StatusOK {
		console.WriteString("PANIC: VFS unlink failed\n")
		for {
			amd64.Halt()
		}
	}
	mark("VFS_NAMESPACE_OK")
	mark("VFS_MOUNT_OK")
	mark("VFS_PATH_OK")
	mark("VFS_CACHE_OK")
	mark("VFS_PROTOCOL_OK")
}
