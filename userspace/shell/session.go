package shell

import "goos/userspace/tty"

func SessionEndsOnEOF(session []byte, result tty.Result) bool {
	return result == tty.ResultEndOfInput && string(session) == "remote"
}
