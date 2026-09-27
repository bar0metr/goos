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
	probe := Executable{Image: 0x100000015, Offset: 0x2200, Size: 0x3000}
	launch, ok := LaunchRequest(3, probe, "fault-probe", grants, 9)
	if !ok {
		t.Fatal("launch encode failed")
	}
	if launch.HandleCount != 0 {
		t.Fatal("manifest references became IPC attachments")
	}
	for _, id := range launch.Handles {
		if id != 0 {
			t.Fatal("nonzero unused attachment")
		}
	}
	grantOffset := launchHeaderSize + len("fault-probe")
	if launch.Payload[grantOffset+14] != 0 || launch.Payload[grantOffset+15] != 0 {
		t.Fatal("non-consuming grant has flags or reserved bytes set")
	}
	event, ok = DecodeLifecycle(launch)
	if !ok || event.Operation != LifecycleLaunch || event.Process != 3 || event.Executable != probe || string(event.ProcessName()) != "fault-probe" ||
		event.CapabilityCount != 1 || event.Capabilities[0] != grants[0] || event.ResourceProfile != ProfileNormal {
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
	// A launch names bytes, never a path: the name is a label and cannot
	// carry a separator, and an extent needs an image and a length.
	for _, name := range []string{"", "/system/init", "bin/ls", "..x/", "sp ace", "\x00", "seventeen-letters"} {
		if _, ok := LaunchRequest(3, probe, name, nil, 9); ok {
			t.Fatalf("name %q accepted", name)
		}
	}
	for _, extent := range []Executable{{Offset: 1, Size: 1}, {Image: 1, Offset: 1}} {
		if _, ok := LaunchRequest(3, extent, "x", nil, 9); ok {
			t.Fatalf("extent %#v accepted", extent)
		}
	}
	corruptName := launch
	corruptName.Payload[launchHeaderSize] = '/'
	if _, ok := DecodeLifecycle(corruptName); ok {
		t.Fatal("decoded a name with a separator")
	}
	corruptExtent := launch
	for index := 26; index < 30; index++ {
		corruptExtent.Payload[index] = 0
	}
	if _, ok := DecodeLifecycle(corruptExtent); ok {
		t.Fatal("decoded an empty extent")
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
	if _, ok := LaunchRequest(3, probe, "init", []Capability{{Handle: 1, Type: handle.TypeDevice}}, 9); ok {
		t.Fatal("capability without rights accepted")
	}
	endpointGrant := []Capability{{Handle: 3, Type: handle.TypeChannel, Rights: capability.Read | capability.Write, Consume: true}}
	endpointLaunch, ok := LaunchRequest(4, probe, "vfs", endpointGrant, 10)
	if !ok {
		t.Fatal("endpoint grant encode failed")
	}
	endpointEvent, ok := DecodeLifecycle(endpointLaunch)
	if !ok || endpointEvent.Capabilities[0] != endpointGrant[0] {
		t.Fatalf("endpoint grant = %#v", endpointEvent)
	}
	startupLaunch, ok := LaunchRequestWithStartup(4, probe, "cat", endpointGrant, ProfileLarge, 3, StagingSlotSize, 11)
	if !ok {
		t.Fatal("startup launch encode failed")
	}
	startupEvent, ok := DecodeLifecycle(startupLaunch)
	if !ok || startupEvent.StartupSlot != 3 || startupEvent.StartupSize != StagingSlotSize || startupEvent.ResourceProfile != ProfileLarge {
		t.Fatalf("startup = %#v", startupEvent)
	}
	if _, ok := LaunchRequestWithStartup(4, probe, "x", nil, ProfileTiny, 0, StagingSlotSize+1, 12); ok {
		t.Fatal("oversized startup accepted")
	}
	if _, ok := LaunchRequestWithStartup(4, probe, "x", nil, ProfileTiny, StagingSlots, 8, 12); ok {
		t.Fatal("out-of-range staging slot accepted")
	}
	corrupt := startupLaunch
	corrupt.Payload[11] = StagingSlots
	if _, ok := DecodeLifecycle(corrupt); ok {
		t.Fatal("decoded out-of-range staging slot")
	}
	corrupt = startupLaunch
	corrupt.Payload[32] = 0
	if _, ok := DecodeLifecycle(corrupt); ok {
		t.Fatal("decoded unknown resource profile")
	}
	corrupt = startupLaunch
	corrupt.Payload[33] = 1
	if _, ok := DecodeLifecycle(corrupt); ok {
		t.Fatal("decoded nonzero launch reserved byte")
	}
	if _, ok := LaunchRequestWithStartup(4, probe, "x", nil, 0, 0, 0, 12); ok {
		t.Fatal("encoded unknown resource profile")
	}
	tooMany := make([]Capability, MaxCapabilities+1)
	for index := range tooMany {
		tooMany[index] = Capability{Handle: handle.ID(index + 1), Type: handle.TypeDevice, Rights: capability.Write}
	}
	if _, ok := LaunchRequest(3, probe, "init", tooMany, 9); ok {
		t.Fatal("oversized capability manifest accepted")
	}
}
