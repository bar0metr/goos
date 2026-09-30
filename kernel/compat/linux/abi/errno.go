package abi

// Linux x86_64 errno values. Keep these numeric wire values independent of
// native kcall, VFS, and stream status spaces.
const (
	EPERM        Errno = 1
	ENOENT       Errno = 2
	EIO          Errno = 5
	EBADF        Errno = 9
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
	ENOSPC       Errno = 28
	ESPIPE       Errno = 29
	EROFS        Errno = 30
	EPIPE        Errno = 32
	ERANGE       Errno = 34
	ENAMETOOLONG Errno = 36
	ENOSYS       Errno = 38
	ENOTEMPTY    Errno = 39
	EOPNOTSUPP   Errno = 95
	ETIMEDOUT    Errno = 110
	EUCLEAN      Errno = 117
)
