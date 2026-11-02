//go:build goos_userspace

// Command sftpd serves one SFTP session over two streams. PID 1 starts it for
// an SSH connection and gives it those streams, a VFS session limited to the
// configured root, and the log. It can launch nothing, sign nothing, and reach
// no other file, whatever a peer gets it to do.
package main

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/sftp"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var stdin, stdout handle.ID
var bulk utility.Bulk

// nativeFiles is the session's filesystem. The utility package answers a request
// the service never answered with StatusPeerDied, bounded by its own deadline.
type nativeFiles struct{}

func attrsOf(reply vfs.Reply) sftp.Attrs {
	return sftp.Attrs{Kind: reply.Kind, Size: reply.Size, Modified: reply.Modified, Node: reply.Node}
}

func (nativeFiles) Stat(path []byte) (sftp.Attrs, vfs.Status) {
	reply, status := utility.Request(vfs.Request{Operation: vfs.OperationStat, Path: path})
	return attrsOf(reply), status
}
func (nativeFiles) StatOpen(file vfs.Handle) (sftp.Attrs, vfs.Status) {
	reply, status := utility.Request(vfs.Request{Operation: vfs.OperationStat, Handle: file})
	return attrsOf(reply), status
}
func (nativeFiles) Open(path []byte, rights vfs.Rights) (vfs.Handle, vfs.Status) {
	reply, status := utility.Request(vfs.Request{Operation: vfs.OperationOpen, Rights: rights, Path: path})
	return reply.Handle, status
}
func (nativeFiles) Close(file vfs.Handle) vfs.Status {
	_, status := utility.Request(vfs.Request{Operation: vfs.OperationClose, Handle: file})
	return status
}
func (nativeFiles) Seek(file vfs.Handle, offset uint64) vfs.Status {
	_, status := utility.Request(vfs.Request{Operation: vfs.OperationSeek, Handle: file, Offset: offset})
	return status
}
func (nativeFiles) Read(file vfs.Handle, target []byte) (int, vfs.Status) {
	if len(target) > len(bulk.Memory) {
		target = target[:len(bulk.Memory)]
	}
	reply, received := bulk.ReadFile(file, len(target))
	if !received {
		return 0, vfs.StatusPeerDied
	}
	if reply.Status != vfs.StatusOK {
		return 0, reply.Status
	}
	return copy(target, bulk.Memory[:reply.Size]), vfs.StatusOK
}
func (nativeFiles) Write(file vfs.Handle, data []byte) (int, vfs.Status) {
	count := copy(bulk.Memory, data)
	reply, received := bulk.WriteFile(file, count)
	if !received {
		return 0, vfs.StatusPeerDied
	}
	return int(reply.Size), reply.Status
}
func (nativeFiles) Create(path []byte) vfs.Status { return utility.CreateFile(path) }
func (nativeFiles) Mkdir(path []byte) vfs.Status  { return utility.Mkdir(path) }
func (nativeFiles) Unlink(path []byte) vfs.Status { return utility.Unlink(path) }
func (nativeFiles) Rename(source, target []byte) vfs.Status {
	return utility.Rename(source, target)
}
func (nativeFiles) Truncate(path []byte, size uint64) vfs.Status {
	_, status := utility.Request(vfs.Request{Operation: vfs.OperationTruncate, Path: path, Offset: size})
	return status
}
func (nativeFiles) ReadDir(path []byte, cursor uint32, target []byte) (sftp.DirBatch, vfs.Status) {
	reply, received := utility.ReadDirBatch(path, cursor, target)
	if !received {
		return sftp.DirBatch{}, vfs.StatusPeerDied
	}
	return sftp.DirBatch{Used: int(reply.Size), Next: reply.Cursor, End: reply.BatchEnd}, reply.Status
}

// writeAll returns false once nobody reads the session's replies.
func writeAll(data []byte) bool {
	for len(data) != 0 {
		chunk := data
		if uint64(len(chunk)) > kcall.MaxStreamIO {
			chunk = chunk[:kcall.MaxStreamIO]
		}
		count, status := sys.StreamWrite(stdout, chunk)
		if count < 0 || count > len(chunk) {
			return false
		}
		data = data[count:]
		switch status {
		case kcall.StatusOK:
		case kcall.StatusResourceExhausted:
			if count == 0 && sys.StreamWaitWrite(stdout, kcall.WaitAnyInfiniteTimeout) != kcall.StatusOK {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// fill reads what the stream holds, waiting only when the session has nothing
// else to do. It returns false at end of input or on failure.
func fill(server *sftp.Server) (bool, error) {
	got := false
	for {
		space := server.Space()
		if len(space) == 0 {
			return true, nil
		}
		if uint64(len(space)) > kcall.MaxStreamIO {
			space = space[:kcall.MaxStreamIO]
		}
		count, status := sys.StreamRead(stdin, space)
		if count > 0 {
			if err := server.Commit(count); err != nil {
				return false, err
			}
			got = true
		}
		if status != kcall.StatusOK && status != kcall.StatusResourceExhausted {
			return got, nil
		}
		if count != 0 {
			continue
		}
		if got {
			return true, nil
		}
		if sys.StreamWaitRead(stdin, kcall.WaitAnyInfiniteTimeout) != kcall.StatusOK {
			return false, nil
		}
	}
}

//export userspaceMain
func userspaceMain() {
	arena.InitService()
	arena.OnExhausted(func() { sys.Exit(1) })
	data, ok := startup.Read()
	if !ok {
		sys.Exit(1)
	}
	logHandle, ok := data.Handle("log")
	if !ok {
		sys.Exit(1)
	}
	syslog.Init(logHandle, "sftpd")
	stdin, ok = data.Handle("stdin")
	if !ok {
		sys.Exit(1)
	}
	stdout, ok = data.Handle("stdout")
	if !ok {
		sys.Exit(1)
	}
	files, ok := data.Handle("vfs")
	if !ok {
		sys.Exit(1)
	}
	root, ok := data.Arg(1)
	if !ok {
		sys.Exit(1)
	}
	utility.BindVFS(files)
	bulk, ok = utility.NewBulk(sftp.MaxData)
	if !ok {
		sys.Exit(1)
	}
	nonce, status := sys.RandomUint64()
	if status != kcall.StatusOK {
		sys.Exit(1)
	}
	server, ok := sftp.NewServer(nativeFiles{}, root, nonce)
	if !ok {
		sys.Exit(1)
	}
	code := serve(server)
	counters := &server.Counters
	syslog.Info("sftp session closed", syslog.Uint("requests", counters.Requests), syslog.Uint("uploaded", counters.BytesUploaded), syslog.Uint("downloaded", counters.BytesDownloaded), syslog.Uint("vfs-reads", counters.VFSReads), syslog.Uint("vfs-writes", counters.VFSWrites), syslog.Uint("dir-batches", counters.DirBatches))
	server.Close()
	sys.Exit(code)
}

func serve(server *sftp.Server) uint64 {
	for {
		reply, err := server.Step()
		if err != nil {
			syslog.Warn("sftp session failed", syslog.Str("reason", err.Error()))
			return 1
		}
		if reply != nil {
			if !writeAll(reply) {
				return 0
			}
			continue
		}
		more, err := fill(server)
		if err != nil {
			syslog.Warn("sftp session failed", syslog.Str("reason", err.Error()))
			return 1
		}
		if !more {
			return 0
		}
	}
}

func main() {}
