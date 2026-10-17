//go:build goos_userspace

package admincmd

import (
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

type NativeFile struct{ handle vfs.Handle }

func OpenFile(path []byte) NativeFile {
	reply, status := utility.Request(vfs.Request{Operation: vfs.OperationOpen, Path: path, Rights: vfs.Read})
	utility.Check(reply, status != vfs.StatusPeerDied, path, false)
	return NativeFile{handle: reply.Handle}
}

func (file NativeFile) Read(buffer []byte) (int, Status) {
	reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationRead, Handle: file.handle, Size: uint32(min(len(buffer), vfs.MaxReplyData))})
	if !received || reply.Status != vfs.StatusOK || len(reply.Data) > len(buffer) {
		return 0, InputError
	}
	n := copy(buffer, reply.Data)
	if n == 0 {
		return 0, EOF
	}
	return n, OK
}

func (file NativeFile) Rewind() Status {
	_, status := utility.Request(vfs.Request{Operation: vfs.OperationSeek, Handle: file.handle, Offset: 0})
	if status != vfs.StatusOK {
		return InputError
	}
	return OK
}

func (file NativeFile) Close() {
	reply, status := utility.Request(vfs.Request{Operation: vfs.OperationClose, Handle: file.handle})
	utility.Check(reply, status != vfs.StatusPeerDied, nil, false)
}

type NativeInput struct{}

func (NativeInput) Read(buffer []byte) (int, Status) {
	for {
		status, n := utility.ReadInput(buffer)
		if utility.Retry(status) {
			continue
		}
		if status == uint64(kcall.StatusPeerDied) {
			return n, EOF
		}
		if status != 0 {
			return 0, InputError
		}
		if n != 0 {
			return n, OK
		}
	}
}

type NativeOutput struct{}

func (NativeOutput) Write(data []byte) Status { utility.Write(data); return OK }

type NativeDirectory struct{}

func (NativeDirectory) ReadDir(path []byte, visitor Visitor) vfs.Status {
	return utility.ReadDir(path, visitor)
}

func Finish(status Status) {
	switch status {
	case OK:
		return
	case LimitError:
		utility.Fail(nil, "input or traversal limit exceeded")
	case ChangedError:
		utility.Fail(nil, "file changed between scans; output may be partial")
	case OutputError:
		utility.Fail(nil, "cannot write output")
	default:
		utility.Fail(nil, "cannot read input")
	}
}

// LineArguments accepts only [-n N] FILE. Zero is valid and prints nothing.
func LineArguments(data startup.Data) (uint64, []byte) {
	count, file, ok := LinesArguments(data)
	if !ok {
		utility.Usage()
	}
	return count, utility.Path(data, file)
}
