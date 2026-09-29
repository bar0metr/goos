package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
	"goos/kernel/process"
	"goos/userspace/startup"
)

func TestProcessStateRetainsLaunchAuthority(t *testing.T) {
	processStates = [process.MaxProcesses]ProcessState{}
	data := linuxStartup(t, "/persist/work", [4]handle.ID{11, 12, 13, 14})
	if !InstallProcess(0x4000, data) || InstallProcess(0x4000, data) {
		t.Fatal("state install did not enforce unique root")
	}
	state, found := FindProcess(0x4000)
	if !found || state.Input != 11 || state.Output != 12 || state.Error != 13 || state.VFS != 14 || string(state.CWD()) != "/persist/work" {
		t.Fatalf("state = %#v cwd=%q", state, state.CWD())
	}
	for fd := 0; fd <= 2; fd++ {
		if _, found := LookupFD(0x4000, fd); !found {
			t.Fatalf("startup fd %d missing", fd)
		}
	}
	if !RemoveProcess(0x4000) {
		t.Fatal("state removal failed")
	}
	if _, found := FindProcess(0x4000); found || RemoveProcess(0x4000) {
		t.Fatal("removed state remained visible")
	}
}

func TestProcessStateIsBounded(t *testing.T) {
	processStates = [process.MaxProcesses]ProcessState{}
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	for index := range processStates {
		if !InstallProcess(uint64(index+1), data) {
			t.Fatalf("install %d failed", index)
		}
	}
	if InstallProcess(uint64(len(processStates)+1), data) {
		t.Fatal("over-capacity state installed")
	}
}

func TestProcessFDExhaustionIsEMFILE(t *testing.T) {
	processStates = [process.MaxProcesses]ProcessState{}
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0x5000, data) {
		t.Fatal("install failed")
	}
	for fd := 3; fd < LinuxMaxFD; fd++ {
		allocated, errno := AllocateFD(0x5000, FileDescription{Kind: FDVFSFile, Object: uint64(fd + 100)}, false)
		if errno != 0 || allocated != fd {
			t.Fatalf("fd %d = %d errno=%d", fd, allocated, errno)
		}
	}
	if fd, errno := AllocateFD(0x5000, FileDescription{Kind: FDVFSFile, Object: 999}, false); fd != -1 || errno != abi.EMFILE {
		t.Fatalf("full table = fd %d errno %d", fd, errno)
	}
}

func linuxStartup(t *testing.T, cwd string, ids [4]handle.ID) startup.Data {
	t.Helper()
	var target [startup.MaxSize]byte
	writer := startup.NewWriter(target[:], 1, 0, 4)
	writer.Command([]byte("/system/linux/probe"))
	writer.CWD([]byte(cwd))
	writer.Argument([]byte("/system/linux/probe"))
	for _, name := range []string{"in", "stdout", "stderr", "vfs"} {
		writer.Handle([]byte(name), 0)
	}
	template, ok := writer.Finish()
	if !ok || !startup.BindHandles(template, ids[:]) {
		t.Fatal("startup construction failed")
	}
	data, ok := startup.Decode(template)
	if !ok {
		t.Fatal("startup decode failed")
	}
	return data
}
