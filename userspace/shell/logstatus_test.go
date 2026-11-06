package shell

import (
	"testing"

	"goos/userspace/logpersist"
)

func TestLogStatusInvocation(t *testing.T) {
	for line, want := range map[string]bool{"log status": true, "log": false, "log status -f": false, "log -n 3": false, "log stat": false} {
		var storage Storage
		invocation, valid := ParseInvocation([]byte(line), &storage)
		if !valid || IsLogStatus(invocation) != want {
			t.Fatalf("%q: status %v", line, IsLogStatus(invocation))
		}
	}
	// The shell reads the file the log service writes.
	if LogStatusPath != "/state/system.log"+logpersist.StatusSuffix {
		t.Fatalf("status path %s", LogStatusPath)
	}
}
