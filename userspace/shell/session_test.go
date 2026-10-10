package shell

import (
	"goos/userspace/tty"
	"testing"
)

func TestOnlyRemoteSessionEndsOnCookedEOF(t *testing.T) {
	for _, session := range []string{"", "local", "remote", "remotely"} {
		for _, result := range []tty.Result{tty.ResultLine, tty.ResultInterrupt, tty.ResultEndOfInput, tty.ResultRaw} {
			want := session == "remote" && result == tty.ResultEndOfInput
			if SessionEndsOnEOF([]byte(session), result) != want {
				t.Fatal("unexpected session lifetime", session, result)
			}
		}
	}
}
