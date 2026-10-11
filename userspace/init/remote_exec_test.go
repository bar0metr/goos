package supervisor

import (
	"goos/userspace/remoteapi"
	"testing"
)

func TestExecRegistryHasNoTerminalGeometry(t *testing.T) {
	var registry RemoteRegistry
	id, result := registry.Admit(1, 1)
	if result != remoteapi.ResultOK {
		t.Fatal(result)
	}
	if result = registry.Authenticate(id, 1, 4); result != remoteapi.ResultOK {
		t.Fatal(result)
	}
	if result = registry.BeginOpenMode(id, 1, remoteapi.ModeExec, remoteapi.Geometry{}); result != remoteapi.ResultOK {
		t.Fatal(result)
	}
	if result = registry.BeginOpenMode(id, 1, remoteapi.ModeExec, remoteapi.Geometry{}); result == remoteapi.ResultOK {
		t.Fatal("second execution accepted")
	}
	if result = registry.CommitOpen(id, 1); result != remoteapi.ResultOK {
		t.Fatal(result)
	}
	geometry, _ := remoteapi.MakeGeometry(80, 24, []byte("xterm"))
	if result = registry.Resize(id, 1, geometry); result == remoteapi.ResultOK {
		t.Fatal("exec resized terminal")
	}
}
