//go:build uefi

package main

import (
	linuxcompat "goos/kernel/compat/linux"
	linuxabi "goos/kernel/compat/linux/abi"
	"goos/kernel/mm/vm"
	"goos/kernel/process"
	"goos/kernel/stream"
)

// Pipe serves pipe and pipe2 with a native stream. The two open-file
// descriptions own the stream's two endpoint references, so the stream closes
// its ends as the last descriptor of each goes, in whichever process holds it.
// The stream is charged to the caller's stream quota.
func (handler *linuxCallHandler) Pipe(request linuxcompat.Request) linuxcompat.Result {
	local := thisCPU()
	closeOnExec := false
	if request.Number == linuxabi.SysPipe2 {
		var errno linuxabi.Errno
		if closeOnExec, errno = linuxcompat.PipeFlags(request.Arguments[1]); errno != 0 {
			return linuxErrno(errno)
		}
	}
	address := request.Arguments[0]
	if !vm.CanCopyOut(handler.root, address, uint64(len(local.linuxPipeWire))) {
		return linuxErrno(linuxabi.EFAULT)
	}
	owner, found := handler.self()
	if !found {
		return linuxErrno(linuxabi.EBADF)
	}
	if !owner.Charge(process.ResourceStreams, 1) {
		return linuxErrno(linuxabi.EMFILE)
	}
	endpoint, id, created := streamRegistry.Create(uint64(owner.ID()))
	if !created {
		owner.Release(process.ResourceStreams, 1)
		return linuxErrno(linuxabi.ENFILE)
	}
	reader, _ := stream.ReaderObject(id)
	writer, _ := stream.WriterObject(id)
	readFD, writeFD, errno := linuxcompat.InstallPipe(handler.root, reader, writer, closeOnExec)
	if errno != 0 {
		endpoint.CloseReader()
		endpoint.CloseWriter()
		releaseStreamSlot(id)
		return linuxErrno(errno)
	}
	putUint32(local.linuxPipeWire[0:4], uint32(readFD))
	putUint32(local.linuxPipeWire[4:8], uint32(writeFD))
	if !vm.CopyOut(handler.root, address, local.linuxPipeWire[:]) {
		// The destination was writable a moment ago, so this is not expected.
		// The descriptors go without dropping references, and the stream
		// closes as one nobody used.
		linuxcompat.CloseFD(handler.root, readFD)
		linuxcompat.CloseFD(handler.root, writeFD)
		endpoint.CloseReader()
		endpoint.CloseWriter()
		releaseStreamSlot(id)
		return linuxErrno(linuxabi.EFAULT)
	}
	return linuxcompat.Result{}
}

// awaitWholeWrite holds a stream write of at most PIPE_BUF bytes until the
// ring has room for all of it. The write then lands in one piece, because
// nothing else runs until the call returns. ready reports that the caller
// may write now; otherwise result ends the call, as a block or an error.
func (handler *linuxCallHandler) awaitWholeWrite(request linuxcompat.Request, endpoint *stream.Stream, streamID uint64, size uint64) (linuxcompat.Result, bool) {
	if size == 0 || size > linuxcompat.LinuxPipeBuf {
		return linuxcompat.Result{}, true
	}
	for {
		switch status := endpoint.WriteStatusFor(int(size)); status {
		case stream.StatusOK:
			return linuxcompat.Result{}, true
		case stream.StatusWouldBlock:
			blocked, retry := handler.blockStream(request, endpoint, streamID, 1, int(size))
			if blocked.Action == linuxabi.ActionBlock || !retry {
				return blocked, false
			}
		default:
			return linuxcompat.Result{Errno: linuxcompat.StreamWriteErrno(status)}, false
		}
	}
}
