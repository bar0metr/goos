package abi

import "testing"

func TestCanonicalX8664SyscallNumbers(t *testing.T) {
	cases := []struct {
		number Syscall
		want   uint64
	}{
		{SysRead, 0}, {SysWrite, 1}, {SysOpen, 2}, {SysClose, 3}, {SysStat, 4}, {SysFstat, 5}, {SysLstat, 6}, {SysReadv, 19}, {SysNewfstatat, 262}, {SysMmap, 9},
		{SysMprotect, 10}, {SysMunmap, 11}, {SysBrk, 12}, {SysMadvise, 28},
		{SysExit, 60}, {SysArchPrctl, 158},
		{SysWritev, 20}, {SysGetdents64, 217}, {SysSetTidAddress, 218}, {SysClockGettime, 228}, {SysExitGroup, 231},
		{SysLseek, 8}, {SysGetcwd, 79}, {SysChdir, 80}, {SysRename, 82}, {SysMkdir, 83}, {SysRmdir, 84}, {SysUnlink, 87},
		{SysOpenat, 257}, {SysMkdirat, 258}, {SysUnlinkat, 263}, {SysRenameat, 264}, {SysRenameat2, 316}, {SysPpoll, 271}, {SysPipe2, 293},
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
	for _, number := range []Syscall{17, 59, 333, 512, ^Syscall(0)} {
		if number.Known() {
			t.Fatalf("out-of-contract syscall %d is known", number)
		}
	}
}
