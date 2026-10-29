package abi

// The x86_64 Linux terminal interface: the struct termios TCGETS and the
// TCSETS family copy, the winsize TIOCGWINSZ copies, the ioctl requests, and
// the flag bits the compatibility terminal acts on. Values are from Linux
// include/uapi/asm-generic/termbits.h and ioctls.h.

const (
	IoctlTCGETS     = uint64(0x5401)
	IoctlTCSETS     = uint64(0x5402)
	IoctlTCSETSW    = uint64(0x5403)
	IoctlTCSETSF    = uint64(0x5404)
	IoctlTCSBRK     = uint64(0x5409)
	IoctlTCXONC     = uint64(0x540a)
	IoctlTCFLSH     = uint64(0x540b)
	IoctlTIOCSCTTY  = uint64(0x540e)
	IoctlTIOCGPGRP  = uint64(0x540f)
	IoctlTIOCSPGRP  = uint64(0x5410)
	IoctlTIOCOUTQ   = uint64(0x5411)
	IoctlTIOCGWINSZ = uint64(0x5413)
	IoctlTIOCSWINSZ = uint64(0x5414)
	IoctlFIONREAD   = uint64(0x541b)
	IoctlTIOCNOTTY  = uint64(0x5422)
	IoctlTIOCGSID   = uint64(0x5429)
)

// TermiosSize is the size of the kernel's struct termios: four 32-bit flag
// words, the line discipline byte, and NCCS control characters. The C
// library's struct is larger; the kernel reads and writes only this prefix.
const (
	TermiosSize = 36
	// TermiosControlChars is NCCS, the length of c_cc.
	TermiosControlChars = 19
	// WinsizeSize is struct winsize: rows, columns, and two pixel counts,
	// each 16 bits.
	WinsizeSize = 8
)

// Offsets of the fields of struct termios.
const (
	TermiosInputOffset   = 0
	TermiosOutputOffset  = 4
	TermiosControlOffset = 8
	TermiosLocalOffset   = 12
	TermiosLineOffset    = 16
	TermiosCharsOffset   = 17
)

// Indexes into c_cc.
const (
	VINTR    = 0
	VQUIT    = 1
	VERASE   = 2
	VKILL    = 3
	VEOF     = 4
	VTIME    = 5
	VMIN     = 6
	VSWTC    = 7
	VSTART   = 8
	VSTOP    = 9
	VSUSP    = 10
	VEOL     = 11
	VREPRINT = 12
	VDISCARD = 13
	VWERASE  = 14
	VLNEXT   = 15
	VEOL2    = 16
)

// c_iflag bits.
const (
	IGNBRK  = uint32(0o1)
	BRKINT  = uint32(0o2)
	IGNPAR  = uint32(0o4)
	ISTRIP  = uint32(0o40)
	INLCR   = uint32(0o100)
	IGNCR   = uint32(0o200)
	ICRNL   = uint32(0o400)
	IXON    = uint32(0o2000)
	IXANY   = uint32(0o4000)
	IXOFF   = uint32(0o10000)
	IMAXBEL = uint32(0o20000)
	IUTF8   = uint32(0o40000)
)

// c_oflag bits.
const (
	OPOST = uint32(0o1)
	ONLCR = uint32(0o4)
)

// c_cflag bits: the speed GoOS reports and the character format.
const (
	B38400 = uint32(0o17)
	CS8    = uint32(0o60)
	CREAD  = uint32(0o200)
	HUPCL  = uint32(0o2000)
)

// c_lflag bits.
const (
	ISIG    = uint32(0o1)
	ICANON  = uint32(0o2)
	ECHO    = uint32(0o10)
	ECHOE   = uint32(0o20)
	ECHOK   = uint32(0o40)
	ECHONL  = uint32(0o100)
	NOFLSH  = uint32(0o200)
	TOSTOP  = uint32(0o400)
	ECHOCTL = uint32(0o1000)
	ECHOKE  = uint32(0o4000)
	IEXTEN  = uint32(0o100000)
)

// TCFLSH queue selectors.
const (
	TCIFLUSH  = uint64(0)
	TCOFLUSH  = uint64(1)
	TCIOFLUSH = uint64(2)
)
