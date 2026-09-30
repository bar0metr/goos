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
	if !InstallProcess(0x4000, data, 0x600000, 0x4600000) || InstallProcess(0x4000, data, 0x600000, 0x4600000) {
		t.Fatal("state install did not enforce unique root")
	}
	state, found := FindProcess(0x4000)
	if !found || state.Input != 11 || state.Output != 12 || state.Error != 13 || state.VFS.Handle != 14 || state.PID < firstPID ||
		state.BrkBase != 0x600000 || state.BrkCurrent != 0x600000 || state.BrkLimit != 0x4600000 || string(state.CWD()) != "/persist/work" {
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
		if !InstallProcess(uint64(index+1), data, 0x600000, 0x4600000) {
			t.Fatalf("install %d failed", index)
		}
	}
	if InstallProcess(uint64(len(processStates)+1), data, 0x600000, 0x4600000) {
		t.Fatal("over-capacity state installed")
	}
}

func TestProcessFDExhaustionIsEMFILE(t *testing.T) {
	processStates = [process.MaxProcesses]ProcessState{}
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0x5000, data, 0x600000, 0x4600000) {
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

func TestCompatibilityPIDsAreDistinctAndBounded(t *testing.T) {
	processStates = [process.MaxProcesses]ProcessState{}
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	nextPID = MaxPID
	if !InstallProcess(0x1000, data, 0x600000, 0x4600000) || !InstallProcess(0x2000, data, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	first, _ := FindProcess(0x1000)
	second, _ := FindProcess(0x2000)
	if first.PID != MaxPID || second.PID != firstPID {
		t.Fatalf("pids = %d,%d; the range did not wrap", first.PID, second.PID)
	}
	// The counter returns to a live PID and must step past it.
	nextPID = MaxPID
	if !InstallProcess(0x3000, data, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	if third, _ := FindProcess(0x3000); third.PID != firstPID+1 {
		t.Fatalf("pid = %d, want the first free value", third.PID)
	}
	processStates = [process.MaxProcesses]ProcessState{}
	nextPID = firstPID
}

func TestCloseFDReportsTheLastReference(t *testing.T) {
	processStates = [process.MaxProcesses]ProcessState{}
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0x6000, data, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	state, _ := FindProcess(0x6000)
	fd, errno := AllocateFD(0x6000, FileDescription{Kind: FDVFSFile, Object: 77}, true)
	duplicate, duplicated := state.FDs.Duplicate(fd, 0, false)
	if errno != 0 || fd != 3 || !duplicated || duplicate != 4 || !state.FDs.HasFree() {
		t.Fatalf("fd=%d errno=%d duplicate=%d", fd, errno, duplicate)
	}
	if _, last, errno := CloseFD(0x6000, fd); errno != 0 || last {
		t.Fatalf("first close: last=%v errno=%d", last, errno)
	}
	released, last, errno := CloseFD(0x6000, duplicate)
	if errno != 0 || !last || released.Kind != FDVFSFile || released.Object != 77 {
		t.Fatalf("final close: %#v last=%v errno=%d", released, last, errno)
	}
	if _, _, errno := CloseFD(0x6000, duplicate); errno != abi.EBADF {
		t.Fatalf("stale fd errno = %d", errno)
	}
	if _, _, errno := CloseFD(0x7000, 0); errno != abi.EBADF {
		t.Fatalf("unknown process errno = %d", errno)
	}
	for state.FDs.HasFree() {
		if _, errno := AllocateFD(0x6000, FileDescription{Kind: FDVFSFile, Object: 1}, false); errno != 0 {
			t.Fatalf("allocate errno = %d", errno)
		}
	}
	if _, errno := AllocateFD(0x6000, FileDescription{Kind: FDVFSFile, Object: 1}, false); errno != abi.EMFILE {
		t.Fatalf("full table errno = %d", errno)
	}
	processStates = [process.MaxProcesses]ProcessState{}
}
