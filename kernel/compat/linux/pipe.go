package linux

import "goos/kernel/compat/linux/abi"

// LinuxPipeBuf is PIPE_BUF: a write of at most this many bytes to a stream
// lands whole, after every byte of an earlier write and before every byte of a
// later one. It matches the value musl compiles into programs.
const LinuxPipeBuf = 4096

// PipeFlags decodes the flags of pipe2. O_CLOEXEC is supported. O_NONBLOCK
// and O_DIRECT need nonblocking and packet streams, which GoOS does not
// have, and every other bit is invalid, so both are EINVAL.
func PipeFlags(flags uint64) (closeOnExec bool, errno abi.Errno) {
	if flags&^uint64(abi.OpenCloseExec) != 0 {
		return false, abi.EINVAL
	}
	return flags != 0, 0
}

func installPipeHeld(root uint64, reader uint64, writer uint64, closeOnExec bool) (int, int, abi.Errno) {
	state, found := findProcessHeld(root)
	if !found {
		return -1, -1, abi.EBADF
	}
	readFD, ok := state.FDs.allocateHeld(FileDescription{Kind: FDStreamReader, Object: reader}, closeOnExec)
	if !ok {
		return -1, -1, abi.EMFILE
	}
	writeFD, ok := state.FDs.allocateHeld(FileDescription{Kind: FDStreamWriter, Object: writer}, closeOnExec)
	if !ok {
		state.FDs.closeHeld(readFD)
		return -1, -1, abi.EMFILE
	}
	return readFD, writeFD, 0
}

// InstallPipe gives a stream's two endpoints their open-file descriptions
// and descriptors: the reader on the lowest free descriptor and the writer
// on the next. Either both descriptors exist afterwards or neither does, and
// on failure the caller still owns both endpoint references.
func InstallPipe(root uint64, reader uint64, writer uint64, closeOnExec bool) (int, int, abi.Errno) {
	tablesLock.Lock()
	result0, result1, result2 := installPipeHeld(root, reader, writer, closeOnExec)
	tablesLock.Unlock()
	return result0, result1, result2
}
