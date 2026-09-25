package exec

// CopyStatus says how copying a file into an executable object ended.
type CopyStatus uint8

const (
	CopyOK CopyStatus = iota
	// CopyReadFailed is a read the filesystem refused or never answered.
	CopyReadFailed
	// CopyWriteFailed is an append the kernel refused.
	CopyWriteFailed
	// CopySizeChanged is a file that ended before, or ran past, the size it
	// had when it was opened: it changed while it was read.
	CopySizeChanged
)

// CopyExecutable copies a file of size bytes into an executable object (ADR
// 0097). read returns the file's next bytes, none at its end; write appends
// bytes to the object. The copy must end exactly at size: the object seals
// only when full, and a file that grew or shrank under the copy is refused
// rather than launched as a mixture of two versions.
func CopyExecutable(size uint64, read func() ([]byte, bool), write func([]byte) bool) CopyStatus {
	copied := uint64(0)
	for {
		data, ok := read()
		if !ok {
			return CopyReadFailed
		}
		if len(data) == 0 {
			if copied != size {
				return CopySizeChanged
			}
			return CopyOK
		}
		if uint64(len(data)) > size-copied {
			return CopySizeChanged
		}
		if !write(data) {
			return CopyWriteFailed
		}
		copied += uint64(len(data))
	}
}

// ExecutableName is the last component of a path, which names the process in
// ps and is the command name in its startup block.
func ExecutableName(path []byte) []byte {
	for index := len(path) - 1; index >= 0; index-- {
		if path[index] == '/' {
			return path[index+1:]
		}
	}
	return path
}
