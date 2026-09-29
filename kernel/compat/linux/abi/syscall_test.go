package abi

import "testing"

func TestCanonicalX8664SyscallNumbers(t *testing.T) {
	cases := []struct {
		number Syscall
		want   uint64
	}{
		{SysRead, 0}, {SysWrite, 1}, {SysClose, 3}, {SysMmap, 9},
		{SysMprotect, 10}, {SysMunmap, 11}, {SysBrk, 12}, {SysMadvise, 28},
		{SysExit, 60}, {SysArchPrctl, 158},
		{SysGetdents64, 217}, {SysClockGettime, 228}, {SysExitGroup, 231},
		{SysOpenat, 257}, {SysPpoll, 271}, {SysPipe2, 293},
		{SysPrlimit64, 302}, {SysGetrandom, 318}, {SysStatx, 332},
	}
	if ArchSetGS != 0x1001 || ArchSetFS != 0x1002 || ArchGetFS != 0x1003 || ArchGetGS != 0x1004 {
		t.Fatal("arch_prctl constants do not match Linux x86_64 UAPI")
	}
	for _, test := range cases {
		if uint64(test.number) != test.want || !test.number.Known() {
			t.Fatalf("syscall %d: value=%d known=%v", test.want, test.number, test.number.Known())
		}
	}
	for _, number := range []Syscall{2, 59, 333, 512, ^Syscall(0)} {
		if number.Known() {
			t.Fatalf("out-of-contract syscall %d is known", number)
		}
	}
}
