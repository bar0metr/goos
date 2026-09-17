package supervisor

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"testing"
)

func TestLifecycleProtocol(t *testing.T) {
	exited, ok := ProcessExited(7, 0x8006)
	if !ok {
		t.Fatal("exit encode failed")
	}
	event, ok := DecodeLifecycle(exited)
	if !ok || event.Operation != LifecycleProcessExited || event.Process != 7 || event.Code != 0x8006 {
		t.Fatalf("exit event = %#v", event)
	}
	grants := []Capability{{Handle: 0x100000002, Type: handle.TypeDevice, Rights: capability.Write}}
	launch, ok := LaunchRequest(3, "/system/fault-probe", grants, 9)
	if !ok {
		t.Fatal("launch encode failed")
	}
	event, ok = DecodeLifecycle(launch)
	if !ok || event.Operation != LifecycleLaunch || event.Process != 3 || event.ExecutablePath() != "/system/fault-probe" ||
		event.CapabilityCount != 1 || event.Capabilities[0] != grants[0] {
		t.Fatalf("launch event = %#v", event)
	}
	launch.Version++
	if _, ok := DecodeLifecycle(launch); ok {
		t.Fatal("wrong protocol version accepted")
	}
	launch.Version = LifecycleVersion
	launch.Correlation = 0
	if _, ok := DecodeLifecycle(launch); ok {
		t.Fatal("uncorrelated launch accepted")
	}
	if _, ok := DecodeLifecycle(ipc.Message{}); ok {
		t.Fatal("empty message accepted")
	}
	if _, ok := LaunchRequest(3, "../escape", nil, 9); ok {
		t.Fatal("parent traversal accepted")
	}
	if _, ok := LaunchRequest(3, "/system/./init", nil, 9); ok {
		t.Fatal("non-canonical executable accepted")
	}
	if _, ok := LaunchRequest(3, "system/init", nil, 9); ok {
		t.Fatal("relative executable accepted")
	}
	result, ok := LaunchResult(8, handle.ID(0x100000001), LaunchOK, 9)
	if !ok {
		t.Fatal("result encode failed")
	}
	event, ok = DecodeLifecycle(result)
	if !ok || event.Operation != LifecycleLaunchResult || event.Process != 8 || event.ProcessHandle != handle.ID(0x100000001) || event.Code != 0 {
		t.Fatalf("launch result = %#v", event)
	}
	if _, ok := LaunchResult(0, 0, LaunchNotFound, 9); !ok {
		t.Fatal("failed launch result rejected")
	}
	if _, ok := LaunchResult(0, 0, LaunchOK, 9); ok {
		t.Fatal("successful result without process accepted")
	}
	if _, ok := LaunchRequest(3, "/system/init", []Capability{{Handle: 1, Type: handle.TypeDevice}}, 9); ok {
		t.Fatal("capability without rights accepted")
	}
	endpointGrant := []Capability{{Handle: 3, Type: handle.TypeChannel, Rights: capability.Read | capability.Write, Consume: true}}
	endpointLaunch, ok := LaunchRequest(4, "/system/vfs", endpointGrant, 10)
	if !ok {
		t.Fatal("endpoint grant encode failed")
	}
	endpointEvent, ok := DecodeLifecycle(endpointLaunch)
	if !ok || endpointEvent.Capabilities[0] != endpointGrant[0] {
		t.Fatalf("endpoint grant = %#v", endpointEvent)
	}
	startup := []byte("startup")
	startupLaunch, ok := LaunchRequestWithStartup(4, "/system/bin/cat", endpointGrant, startup, 11)
	if !ok {
		t.Fatal("startup launch encode failed")
	}
	startupEvent, ok := DecodeLifecycle(startupLaunch)
	if !ok || string(startupEvent.Startup[:startupEvent.StartupSize]) != "startup" {
		t.Fatalf("startup = %#v", startupEvent)
	}
	if _, ok := LaunchRequestWithStartup(4, "/system/x", nil, make([]byte, MaxStartupData+1), 12); ok {
		t.Fatal("oversized startup accepted")
	}
	tooMany := make([]Capability, MaxCapabilities+1)
	for index := range tooMany {
		tooMany[index] = Capability{Handle: handle.ID(index + 1), Type: handle.TypeDevice, Rights: capability.Write}
	}
	if _, ok := LaunchRequest(3, "/system/init", tooMany, 9); ok {
		t.Fatal("oversized capability manifest accepted")
	}
}
