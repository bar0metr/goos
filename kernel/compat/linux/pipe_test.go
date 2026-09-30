package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/handle"
)

func TestPipeFlagsAcceptCloseOnExecOnly(t *testing.T) {
	if closeOnExec, errno := PipeFlags(0); closeOnExec || errno != 0 {
		t.Fatal("pipe2 with no flags refused")
	}
	if closeOnExec, errno := PipeFlags(uint64(abi.OpenCloseExec)); !closeOnExec || errno != 0 {
		t.Fatal("pipe2 O_CLOEXEC refused")
	}
	for _, flags := range []uint64{uint64(abi.OpenNonBlock), uint64(abi.OpenDirect), 1, uint64(abi.OpenCloseExec | abi.OpenNonBlock)} {
		if _, errno := PipeFlags(flags); errno != abi.EINVAL {
			t.Fatalf("pipe2 flags %#o = errno %d", flags, errno)
		}
	}
}

func TestInstallPipeTakesBothDescriptorsOrNeither(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0xd000, 700, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	readFD, writeFD, errno := InstallPipe(0xd000, 40, 41, true)
	if errno != 0 || readFD != 3 || writeFD != 4 {
		t.Fatalf("pipe = %d %d errno %d", readFD, writeFD, errno)
	}
	state, _ := FindProcess(0xd000)
	if reader, _ := state.FDs.Lookup(readFD); reader.Kind != FDStreamReader || reader.Object != 40 {
		t.Fatalf("reader = %#v", reader)
	}
	if closeOnExec, _ := state.FDs.CloseOnExec(writeFD); !closeOnExec {
		t.Fatal("pipe2 O_CLOEXEC was not applied")
	}
	for fd := 5; fd < LinuxMaxFD-1; fd++ {
		if _, ok := state.FDs.Allocate(FileDescription{Kind: FDSynthetic, Object: 1}, false); !ok {
			t.Fatalf("fill %d failed", fd)
		}
	}
	if _, _, errno := InstallPipe(0xd000, 50, 51, false); errno != abi.EMFILE {
		t.Fatalf("pipe with one free descriptor = errno %d", errno)
	}
	if !state.FDs.HasFree() {
		t.Fatal("a failed pipe kept its first descriptor")
	}
}
