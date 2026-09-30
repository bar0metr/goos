package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
)

func TestExecSnapshotOwnsBoundedArgumentsAndEnvironment(t *testing.T) {
	var snapshot ExecSnapshot
	path := []byte("/system/linux/app")
	argument := []byte("first")
	variable := []byte("KEY=value")
	if errno := snapshot.SetCommand(path); errno != 0 {
		t.Fatalf("command errno = %d", errno)
	}
	if errno := snapshot.AddArgument(argument); errno != 0 {
		t.Fatalf("argument errno = %d", errno)
	}
	if errno := snapshot.AddEnvironment(variable); errno != 0 {
		t.Fatalf("environment errno = %d", errno)
	}
	path[0], argument[0], variable[0] = 'x', 'x', 'x'
	if string(snapshot.Command()) != "/system/linux/app" || snapshot.Args() != 1 {
		t.Fatalf("snapshot command=%q args=%d", snapshot.Command(), snapshot.Args())
	}
	if value, ok := snapshot.Arg(0); !ok || string(value) != "first" {
		t.Fatalf("argument = %q ok=%v", value, ok)
	}
	name, value := snapshot.EnvAt(0)
	if string(name) != "KEY" || string(value) != "value" {
		t.Fatalf("environment = %q=%q", name, value)
	}
}

func TestExecSnapshotEnforcesCountsAndBytes(t *testing.T) {
	var snapshot ExecSnapshot
	if errno := snapshot.SetCommand(nil); errno != abi.ENOENT {
		t.Fatalf("empty command errno = %d", errno)
	}
	if errno := snapshot.SetCommand([]byte("/x")); errno != 0 {
		t.Fatalf("command errno = %d", errno)
	}
	for index := 0; index < MaxExecArgs; index++ {
		if errno := snapshot.AddArgument(nil); errno != 0 {
			t.Fatalf("argument %d errno = %d", index, errno)
		}
	}
	if errno := snapshot.AddArgument(nil); errno != abi.E2BIG {
		t.Fatalf("argument overflow errno = %d", errno)
	}
	snapshot.Reset()
	if errno := snapshot.SetCommand([]byte("/x")); errno != 0 {
		t.Fatalf("command after reset errno = %d", errno)
	}
	if errno := snapshot.AddArgument(make([]byte, MaxExecStringBytes+1)); errno != abi.E2BIG {
		t.Fatalf("string overflow errno = %d", errno)
	}
	if errno := snapshot.AddEnvironment([]byte("missing")); errno != 0 {
		t.Fatalf("raw environment errno = %d", errno)
	}
	if value, ok := snapshot.Environment(0); !ok || string(value) != "missing" {
		t.Fatalf("raw environment = %q ok=%v", value, ok)
	}
}
