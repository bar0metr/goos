package abi

// Syscall is a Linux x86_64 syscall number. It is intentionally distinct from
// the native kcall.Operation namespace.
type Syscall uint64

// Numbers are from Linux arch/x86/entry/syscalls/syscall_64.tbl. This bounded
// planning namespace includes measured release candidates and later fallbacks;
// membership is not a claim of implementation or a 0.2.0 release commitment.
const (
	SysRead          Syscall = 0
	SysWrite         Syscall = 1
	SysOpen          Syscall = 2
	SysClose         Syscall = 3
	SysStat          Syscall = 4
	SysFstat         Syscall = 5
	SysLstat         Syscall = 6
	SysPoll          Syscall = 7
	SysLseek         Syscall = 8
	SysMmap          Syscall = 9
	SysMprotect      Syscall = 10
	SysMunmap        Syscall = 11
	SysBrk           Syscall = 12
	SysRtSigaction   Syscall = 13
	SysRtSigprocmask Syscall = 14
	SysIoctl         Syscall = 16
	SysReadv         Syscall = 19
	SysWritev        Syscall = 20
	SysPipe          Syscall = 22
	SysSchedYield    Syscall = 24
	SysMadvise       Syscall = 28
	SysNanosleep     Syscall = 35
	SysGetpid        Syscall = 39
	SysExit          Syscall = 60
	SysUname         Syscall = 63
	SysGetcwd        Syscall = 79
	SysChdir         Syscall = 80
	SysRename        Syscall = 82
	SysMkdir         Syscall = 83
	SysRmdir         Syscall = 84
	SysUnlink        Syscall = 87
	SysReadlink      Syscall = 89
	SysGetuid        Syscall = 102
	SysGetgid        Syscall = 104
	SysGeteuid       Syscall = 107
	SysGetegid       Syscall = 108
	SysGetppid       Syscall = 110
	SysArchPrctl     Syscall = 158
	SysGettid        Syscall = 186
	SysFutex         Syscall = 202
	SysGetdents64    Syscall = 217
	SysSetTidAddress Syscall = 218
	SysClockGettime  Syscall = 228
	SysExitGroup     Syscall = 231
	SysOpenat        Syscall = 257
	SysMkdirat       Syscall = 258
	SysNewfstatat    Syscall = 262
	SysUnlinkat      Syscall = 263
	SysRenameat      Syscall = 264
	SysReadlinkat    Syscall = 267
	SysPpoll         Syscall = 271
	SysSetRobustList Syscall = 273
	SysPipe2         Syscall = 293
	SysPrlimit64     Syscall = 302
	SysRenameat2     Syscall = 316
	SysGetrandom     Syscall = 318
	SysStatx         Syscall = 332
)

const (
	ProtNone  = uint64(0)
	ProtRead  = uint64(1)
	ProtWrite = uint64(2)
	ProtExec  = uint64(4)

	MapPrivate   = uint64(0x02)
	MapAnonymous = uint64(0x20)

	MadvDontNeed = uint64(4)

	ArchSetGS = uint64(0x1001)
	ArchSetFS = uint64(0x1002)
	ArchGetFS = uint64(0x1003)
	ArchGetGS = uint64(0x1004)

	// AtFDCWD is the dirfd that names the working directory.
	AtFDCWD = int32(-100)

	OpenReadOnly  = uint32(0)
	OpenWriteOnly = uint32(1)
	OpenReadWrite = uint32(2)
	OpenAccess    = uint32(3)
	OpenCreate    = uint32(0o100)
	OpenExclusive = uint32(0o200)
	OpenNoCTTY    = uint32(0o400)
	OpenTruncate  = uint32(0o1000)
	OpenAppend    = uint32(0o2000)
	OpenLargeFile = uint32(0o100000)
	OpenDirectory = uint32(0o200000)
	OpenNoFollow  = uint32(0o400000)
	OpenCloseExec = uint32(0o2000000)

	// AtRemoveDir makes unlinkat remove a directory.
	AtRemoveDir = uint64(0x200)

	// Flags of newfstatat.
	AtSymlinkNoFollow = uint64(0x100)
	AtNoAutomount     = uint64(0x800)
	AtEmptyPath       = uint64(0x1000)

	// d_type values of a directory entry.
	DirentDirectory = uint8(4)
	DirentRegular   = uint8(8)

	// Clock identifiers of clock_gettime.
	ClockRealtime        = uint64(0)
	ClockMonotonic       = uint64(1)
	ClockMonotonicRaw    = uint64(4)
	ClockRealtimeCoarse  = uint64(5)
	ClockMonotonicCoarse = uint64(6)
	ClockBoottime        = uint64(7)

	// Resources of prlimit64 and the value that means no limit.
	ResourceCPU       = uint64(0)
	ResourceFileSize  = uint64(1)
	ResourceData      = uint64(2)
	ResourceStack     = uint64(3)
	ResourceCore      = uint64(4)
	ResourceOpenFiles = uint64(7)
	ResourceCount     = uint64(16)
	ResourceInfinity  = ^uint64(0)

	// File type and permission bits of st_mode.
	ModeFIFO      = uint32(0o010000)
	ModeDirectory = uint32(0o040000)
	ModeRegular   = uint32(0o100000)

	SeekSet = uint64(0)
	SeekCur = uint64(1)
	SeekEnd = uint64(2)

	// PathMax is Linux PATH_MAX: the longest path in bytes, terminator
	// included.
	PathMax = 4096

	// IOVecSize is the wire size of one struct iovec.
	IOVecSize = 16
)

// Known reports whether number belongs to the bounded planning namespace.
// Known does not mean implemented or release-scoped: an absent handler still
// returns ENOSYS.
func (number Syscall) Known() bool {
	switch number {
	case SysRead, SysWrite, SysOpen, SysClose, SysStat, SysFstat, SysLstat, SysPoll, SysLseek, SysMmap,
		SysMprotect, SysMunmap, SysBrk, SysRtSigaction, SysRtSigprocmask,
		SysIoctl, SysReadv, SysWritev, SysPipe, SysSchedYield, SysMadvise,
		SysNanosleep, SysGetpid, SysExit, SysUname, SysGetcwd, SysChdir,
		SysRename, SysMkdir, SysRmdir, SysUnlink, SysMkdirat, SysRenameat,
		SysRenameat2,
		SysReadlink, SysGetuid, SysGetgid, SysGeteuid, SysGetegid, SysGetppid,
		SysArchPrctl, SysGettid, SysFutex, SysGetdents64, SysSetTidAddress,
		SysClockGettime,
		SysExitGroup, SysOpenat, SysNewfstatat, SysUnlinkat, SysReadlinkat,
		SysPpoll, SysSetRobustList, SysPipe2, SysPrlimit64, SysGetrandom,
		SysStatx:
		return true
	default:
		return false
	}
}
