package shell

import "goos/userspace/tty"

func SessionEndsOnEOF(session []byte, result tty.Result) bool {
	return result == tty.ResultEndOfInput && RemoteSession(session)
}

func RemoteSession(session []byte) bool { return string(session) == "remote" }
