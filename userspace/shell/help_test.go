package shell

import (
	"goos/kernel/vfs"
	"testing"
)

func TestCommandTableIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, command := range Commands {
		if command.Name == "" || command.Summary == "" || len(command.Usage) < len(command.Name) || command.Usage[:len(command.Name)] != command.Name {
			t.Fatalf("incomplete entry %+v", command)
		}
		if seen[command.Name] {
			t.Fatalf("%s listed twice", command.Name)
		}
		seen[command.Name] = true
		if command.MinArgs < 0 || command.MaxArgs != Unbounded && command.MaxArgs < command.MinArgs {
			t.Fatalf("%s: impossible arity %d..%d", command.Name, command.MinArgs, command.MaxArgs)
		}
	}
	for _, name := range []string{"cd", "help", "clear", "diag", "log", "services", "reboot", "poweroff", "handles", "caps", "time", "jobs", "kill", "config", "hosts"} {
		if !IsBuiltin([]byte(name)) {
			t.Fatalf("%s is not a builtin", name)
		}
	}
	if IsBuiltin([]byte("ls")) || IsBuiltin([]byte("missing")) {
		t.Fatal("an executable or unknown name is a builtin")
	}
}

func TestCommandArity(t *testing.T) {
	for _, test := range []struct {
		name  string
		count int
		want  bool
	}{
		{"help", 0, true}, {"help", 1, true}, {"help", 2, false},
		{"time", 0, false}, {"time", 40, true},
		{"services", 1, false}, {"kill", 0, false}, {"kill", 1, true},
	} {
		command, found := LookupCommand([]byte(test.name))
		if !found || command.Accepts(test.count) != test.want {
			t.Fatalf("%s with %d arguments: accepted=%v", test.name, test.count, !test.want)
		}
	}
}

func TestHelpFlag(t *testing.T) {
	if !IsHelpFlag([]byte("-h")) || !IsHelpFlag([]byte("--help")) || IsHelpFlag([]byte("-help")) || IsHelpFlag(nil) {
		t.Fatal("help flag misrecognised")
	}
}

func TestVFSReasonNamesEveryStatus(t *testing.T) {
	for status := vfs.StatusInvalidArgument; status <= vfs.StatusBusy; status++ {
		if VFSReason(status, false) == "filesystem error" {
			t.Fatalf("status %d has no reason", status)
		}
	}
	if VFSReason(vfs.StatusPermissionDenied, true) != "read-only filesystem" || VFSReason(vfs.StatusPermissionDenied, false) != "permission denied" {
		t.Fatal("read-only refusals are not named")
	}
}

func TestFormatUptime(t *testing.T) {
	var buffer [32]byte
	for seconds, want := range map[uint64]string{
		0:               "up 0:00:00",
		59:              "up 0:00:59",
		3723:            "up 1:02:03",
		86400 + 61:      "up 1 day, 0:01:01",
		3*86400 + 86399: "up 3 days, 23:59:59",
	} {
		if got := string(FormatUptime(seconds, &buffer)); got != want {
			t.Fatalf("FormatUptime(%d) = %q, want %q", seconds, got, want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	var buffer [24]byte
	for bytes, want := range map[uint64]string{
		0:               "0.0 KiB",
		4096:            "4.0 KiB",
		1536 * 1024:     "1.5 MiB",
		512 << 20:       "512.0 MiB",
		(1 << 20) - 1:   "1024.0 KiB",
		3<<30 + 512<<20: "3.5 GiB",
	} {
		if got := string(FormatSize(bytes, &buffer)); got != want {
			t.Fatalf("FormatSize(%d) = %q, want %q", bytes, got, want)
		}
	}
}

func TestFormattingDoesNotAllocate(t *testing.T) {
	var uptime [32]byte
	var size [24]byte
	if allocations := testing.AllocsPerRun(10, func() {
		FormatUptime(1<<40, &uptime)
		FormatSize(1<<62, &size)
	}); allocations != 0 {
		t.Fatalf("formatting allocated %v times", allocations)
	}
}
