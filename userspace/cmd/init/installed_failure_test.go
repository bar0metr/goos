package main

import (
	"strings"
	"testing"

	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/utility"
)

func TestInstalledFailureIncludesPhaseRawStatusAndElapsed(t *testing.T) {
	for _, phase := range []string{"stat", "open", "read", "close"} {
		var storage [512]byte
		line := appendInstalledFailure(storage[:0], "/persist/system/services/netd", phase, 7, 44, 200, false, 0,
			utility.VFSFailure{Phase: "receive", Status: kcall.StatusTimeout, Sent: true})
		for _, field := range []string{"path=/persist/system/services/netd", "phase=" + phase, "session=7", "correlation=44", "elapsed=200", "received=0", "transaction=receive", "raw=6", "sent=1"} {
			if !strings.Contains(string(line), field) {
				t.Fatalf("missing %q in %s", field, line)
			}
		}
		if len(line) >= len(storage) || line[len(line)-1] != '\n' {
			t.Fatal("failure record not bounded and whole")
		}
	}
}

func TestInstalledFailureKeepsCompletedErrorDistinct(t *testing.T) {
	line := appendInstalledFailure(nil, "/persist/system/services/virtio-net", "open", 7, 45, 1, true, vfs.StatusBackendError,
		utility.VFSFailure{Phase: "complete", Status: kcall.StatusOK, Sent: true})
	for _, field := range []string{"received=1", "transaction=complete", "raw=0", "sent=1"} {
		if !strings.Contains(string(line), field) {
			t.Fatalf("missing %q: %s", field, line)
		}
	}
}
