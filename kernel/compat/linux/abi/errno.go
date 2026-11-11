package abi

// Linux x86_64 errno values. Keep these numeric wire values independent of
// native kcall, VFS, and stream status spaces.
const (
	EPERM        Errno = 1
	ENOENT       Errno = 2
	ESRCH        Errno = 3
	EINTR        Errno = 4
	EIO          Errno = 5
	ENXIO        Errno = 6
	E2BIG        Errno = 7
	ENOEXEC      Errno = 8
	EBADF        Errno = 9
	ECHILD       Errno = 10
	EAGAIN       Errno = 11
	ENOMEM       Errno = 12
	EACCES       Errno = 13
	EFAULT       Errno = 14
	EBUSY        Errno = 16
	EEXIST       Errno = 17
	EXDEV        Errno = 18
	ENOTDIR      Errno = 20
	EISDIR       Errno = 21
	EINVAL       Errno = 22
	ENFILE       Errno = 23
	EMFILE       Errno = 24
	ENOTTY       Errno = 25
	ENOSPC       Errno = 28
	ESPIPE       Errno = 29
	EROFS        Errno = 30
	EPIPE        Errno = 32
	EDOM         Errno = 33
	ERANGE       Errno = 34
	ENAMETOOLONG Errno = 36
	ENOSYS       Errno = 38
	ENOTEMPTY    Errno = 39
	ELOOP        Errno = 40
	ENOTSOCK     Errno = 88
	EDESTADDRREQ Errno = 89
	EMSGSIZE     Errno = 90
	EPROTOTYPE   Errno = 91
	ENOPROTOOPT  Errno = 92
	// EPROTONOSUPPORT and ESOCKTNOSUPPORT refuse a socket protocol and a
	// socket type.
	EPROTONOSUPPORT Errno = 93
	ESOCKTNOSUPPORT Errno = 94
	EOPNOTSUPP      Errno = 95
	EAFNOSUPPORT    Errno = 97
	EADDRINUSE      Errno = 98
	EADDRNOTAVAIL   Errno = 99
	ENETDOWN        Errno = 100
	ENETUNREACH     Errno = 101
	ENETRESET       Errno = 102
	ECONNABORTED    Errno = 103
	ECONNRESET      Errno = 104
	ENOBUFS         Errno = 105
	EISCONN         Errno = 106
	ENOTCONN        Errno = 107
	ETIMEDOUT       Errno = 110
	ECONNREFUSED    Errno = 111
	EHOSTUNREACH    Errno = 113
	EALREADY        Errno = 114
	EINPROGRESS     Errno = 115
	EUCLEAN         Errno = 117
)
