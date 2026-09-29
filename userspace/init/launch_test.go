package supervisor

import (
	"goos/userspace/commands"
	execproto "goos/userspace/exec"
	"testing"
)

func oneStage(t *testing.T, path string, background bool) execproto.Request {
	t.Helper()
	var slot [execproto.SlotSize]byte
	builder := execproto.NewBuilder(slot[:], []byte("/"), nil, background)
	builder.Stage([]byte(path), [][]byte{[]byte("argv0")})
	data, ok := builder.Finish(0)
	if !ok {
		t.Fatal("finish")
	}
	request, ok := execproto.DecodeRequest(data)
	if !ok {
		t.Fatal("decode")
	}
	return request
}

func index(command []byte) (commands.Grant, bool) {
	switch string(command) {
	case "gview", "edit":
		return commands.GrantVFS | commands.GrantTerminal, true
	case "gfm":
		return commands.GrantVFS | commands.GrantTerminal | commands.GrantLaunch, true
	case "ls":
		return commands.GrantVFS, true
	}
	return 0, false
}

func TestChildLaunchGrantsAFullScreenCommand(t *testing.T) {
	command, grants, status := ChildLaunch(oneStage(t, "/system/bin/gview", false), index)
	if status != LaunchOK || string(command) != "gview" || grants != commands.GrantVFS|commands.GrantTerminal {
		t.Fatalf("gview: %q %d %d", command, grants, status)
	}
}

func TestChildLaunchNeverPassesTheLaunchGrantOn(t *testing.T) {
	_, grants, status := ChildLaunch(oneStage(t, "/system/bin/gfm", false), index)
	if status != LaunchOK || grants&commands.GrantLaunch != 0 {
		t.Fatalf("nested gfm: grants %d status %d", grants, status)
	}
}

func TestChildLaunchRefusals(t *testing.T) {
	cases := map[string]struct {
		request execproto.Request
		want    LaunchStatus
	}{
		"line command":  {oneStage(t, "/system/bin/ls", false), LaunchCapabilityDenied},
		"unknown":       {oneStage(t, "/system/bin/nosuch", false), LaunchNotFound},
		"from a file":   {oneStage(t, "/home/gview", false), LaunchCapabilityDenied},
		"a service":     {oneStage(t, "/system/vfs", false), LaunchCapabilityDenied},
		"in background": {oneStage(t, "/system/bin/gview", true), LaunchCapabilityDenied},
	}
	for name, test := range cases {
		if _, _, status := ChildLaunch(test.request, index); status != test.want {
			t.Fatalf("%s: status %d, want %d", name, status, test.want)
		}
	}
}

func TestLaunchesChildrenOnlyForOneForegroundStage(t *testing.T) {
	grant := commands.GrantTerminal | commands.GrantLaunch
	if !LaunchesChildren(oneStage(t, "/system/bin/gfm", false), grant) {
		t.Fatal("a foreground gfm gets no launch channel")
	}
	if LaunchesChildren(oneStage(t, "/system/bin/gfm", true), grant) {
		t.Fatal("a background job got a launch channel")
	}
	if LaunchesChildren(oneStage(t, "/system/bin/gfm", false), commands.GrantTerminal) {
		t.Fatal("a command that did not ask got a launch channel")
	}
	pipeline := oneStage(t, "/system/bin/gfm", false)
	pipeline.Count = 2
	if LaunchesChildren(pipeline, grant) {
		t.Fatal("a pipeline stage got a launch channel")
	}
}
